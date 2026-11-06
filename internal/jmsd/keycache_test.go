package jmsd

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// countingLoader stands in for CoreDB.activeUsers, recording how many times
// the "database" was actually read.
type countingLoader struct {
	mu    sync.Mutex
	calls int
	rows  []userRecord
	err   error
	// block, when non-nil, holds every call until it is closed -- used to get
	// several callers inside one refresh at the same time.
	block chan struct{}
}

func (l *countingLoader) load() ([]userRecord, error) {
	l.mu.Lock()
	l.calls++
	block, rows, err := l.block, l.rows, l.err
	l.mu.Unlock()
	if block != nil {
		<-block
	}
	return rows, err
}

func (l *countingLoader) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// testClock is a manually advanced clock, so TTL expiry is tested without
// sleeping.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestDirectory(loader *countingLoader, ttl time.Duration) (*userDirectory, *testClock) {
	clock := &testClock{t: time.Unix(1700000000, 0)}
	d := newUserDirectory(loader.load, ttl)
	d.now = clock.now
	return d, clock
}

var (
	alice = userRecord{ID: "u1", Username: "alice", Email: "alice@example.com"}
	bob   = userRecord{ID: "u2", Username: "bob", Email: "bob@example.com"}
)

// The reason this cache exists: identify runs before any signature is
// checked, so lookups driven by forged requests must not each reach Core's
// database.
func TestUserDirectoryCachesWithinTTL(t *testing.T) {
	loader := &countingLoader{rows: []userRecord{alice}}
	d, _ := newTestDirectory(loader, userListTTL)

	for range 500 {
		if _, err := d.ByID("nobody"); err == nil {
			t.Fatal("ByID() found a user that does not exist")
		}
	}
	if u, err := d.ByID("u1"); err != nil || u.Username != "alice" {
		t.Fatalf("ByID(u1) = %v, %v; want alice", u, err)
	}
	if got := loader.count(); got != 1 {
		t.Errorf("database reads = %d, want 1", got)
	}
}

// Staleness has a bound: once ttl has passed the next lookup re-reads, which
// is what makes a user disabled in JumpServer stop working.
func TestUserDirectoryRefreshesAfterTTL(t *testing.T) {
	loader := &countingLoader{rows: []userRecord{alice}}
	d, clock := newTestDirectory(loader, userListTTL)

	if _, err := d.ByID("u1"); err != nil {
		t.Fatalf("first ByID() error = %v", err)
	}
	loader.mu.Lock()
	loader.rows = nil // alice disabled
	loader.mu.Unlock()

	clock.advance(userListTTL - time.Second)
	if _, err := d.ByID("u1"); err != nil {
		t.Fatalf("ByID() inside the TTL error = %v", err)
	}
	clock.advance(2 * time.Second)
	if _, err := d.ByID("u1"); err == nil {
		t.Fatal("ByID() still found alice after the TTL, though she was disabled")
	}
}

// A burst on a cold cache must produce ONE database read, not one per caller.
func TestUserDirectoryRefreshIsSingleFlight(t *testing.T) {
	loader := &countingLoader{rows: []userRecord{alice}, block: make(chan struct{})}
	d, _ := newTestDirectory(loader, userListTTL)

	const callers = 20
	var started, finished sync.WaitGroup
	started.Add(callers)
	finished.Add(callers)
	for range callers {
		go func() {
			started.Done()
			defer finished.Done()
			if _, err := d.ByID("u1"); err != nil {
				t.Errorf("ByID() error = %v", err)
			}
		}()
	}
	started.Wait()
	close(loader.block)
	finished.Wait()

	if got := loader.count(); got != 1 {
		t.Errorf("database reads = %d for %d concurrent cold lookups, want 1", got, callers)
	}
}

// A failed refresh is reported, not answered from the previous snapshot:
// that would keep disabled users working for as long as the outage lasted.
func TestUserDirectoryDoesNotServeStaleAfterFailedRefresh(t *testing.T) {
	loader := &countingLoader{rows: []userRecord{alice}}
	d, clock := newTestDirectory(loader, userListTTL)

	if _, err := d.ByID("u1"); err != nil {
		t.Fatalf("first ByID() error = %v", err)
	}
	loader.mu.Lock()
	loader.err = errors.New("database went away")
	loader.mu.Unlock()
	clock.advance(userListTTL + time.Second)

	if _, err := d.ByID("u1"); err == nil {
		t.Fatal("ByID() served the expired snapshot through a failed refresh")
	}
}

// Two users with one email: a Google sign-in for it must be refused, not
// resolved to whichever sorts first.
func TestUserDirectoryRefusesSharedEmail(t *testing.T) {
	eve := userRecord{ID: "u3", Username: "eve", Email: "ALICE@example.com"}
	d, _ := newTestDirectory(&countingLoader{rows: []userRecord{alice, bob, eve}}, userListTTL)
	if _, err := d.ByEmail("alice@example.com"); err == nil {
		t.Fatal("ByEmail() resolved an email two users share")
	}
	if u, err := d.ByEmail("BOB@example.com"); err != nil || u.ID != "u2" {
		t.Fatalf("ByEmail(bob) = %v, %v; want u2 (case-insensitive)", u, err)
	}
}
