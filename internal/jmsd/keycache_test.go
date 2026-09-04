package jmsd

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// countingLoader stands in for CoreDB.activeSSHKeys, recording how many times
// the "database" was actually read.
type countingLoader struct {
	mu    sync.Mutex
	calls int
	rows  []sshKeyRow
	err   error
	// block, when non-nil, holds every call until it is closed -- used to get
	// several callers inside one refresh at the same time.
	block chan struct{}
}

func (l *countingLoader) load() ([]sshKeyRow, error) {
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

// The reason this cache exists: a request that resolves to nothing -- which is
// every forged request -- must not reach the database. Before the cache, each
// one cost a full authentication_sshkey scan plus a per-row AES-GCM decrypt.
func TestUserDirectoryForgedLookupsDoNotReloadFromDatabase(t *testing.T) {
	loader := &countingLoader{rows: []sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyA},
	}}
	d, _ := newTestDirectory(loader, keyIndexTTL)

	for range 500 {
		if _, err := d.Lookup(testFingerprintFP); err == nil {
			t.Fatal("Lookup() accepted a fingerprint nobody registered")
		}
	}
	if got := loader.count(); got != 1 {
		t.Errorf("database reads = %d, want 1 for 500 forged lookups", got)
	}
}

func TestUserDirectoryResolvesAndCaches(t *testing.T) {
	loader := &countingLoader{rows: []sshKeyRow{
		{userID: "u1", username: "alice", otpSecretKey: "SECRETA", publicKey: testKeyA},
		{userID: "u2", username: "bob", publicKey: testKeyB},
	}}
	d, _ := newTestDirectory(loader, keyIndexTTL)

	for range 3 {
		user, err := d.Lookup(testKeyAFP)
		if err != nil {
			t.Fatalf("Lookup() error = %v", err)
		}
		if user.ID != "u1" || user.Username != "alice" {
			t.Fatalf("Lookup() = %s/%s, want u1/alice", user.ID, user.Username)
		}
		if user.OTPSecretKey != "SECRETA" {
			t.Errorf("OTPSecretKey = %q, want SECRETA", user.OTPSecretKey)
		}
		if len(user.PublicKeys) != 1 {
			t.Errorf("PublicKeys = %v, want just alice's", user.PublicKeys)
		}
	}
	if got := loader.count(); got != 1 {
		t.Errorf("database reads = %d, want 1", got)
	}
}

// Staleness has a bound: once ttl has passed the next lookup re-reads, which is
// what lets a newly registered key start working (and a deactivated one stop).
func TestUserDirectoryRefreshesAfterTTL(t *testing.T) {
	loader := &countingLoader{rows: []sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyA},
	}}
	d, clock := newTestDirectory(loader, keyIndexTTL)

	if _, err := d.Lookup(testKeyAFP); err != nil {
		t.Fatalf("first Lookup() error = %v", err)
	}
	clock.advance(keyIndexTTL - time.Second)
	if _, err := d.Lookup(testKeyAFP); err != nil {
		t.Fatalf("Lookup() inside the TTL error = %v", err)
	}
	if got := loader.count(); got != 1 {
		t.Fatalf("database reads = %d inside the TTL, want 1", got)
	}

	// A key added to Core after the snapshot was taken.
	loader.mu.Lock()
	loader.rows = append(loader.rows, sshKeyRow{userID: "u2", username: "bob", publicKey: testKeyB})
	loader.mu.Unlock()

	clock.advance(2 * time.Second)
	user, err := d.Lookup(testKeyBFP)
	if err != nil {
		t.Fatalf("Lookup() after the TTL error = %v, want bob's new key to be visible", err)
	}
	if user.ID != "u2" {
		t.Errorf("Lookup() = %q, want u2", user.ID)
	}
	if got := loader.count(); got != 2 {
		t.Errorf("database reads = %d after the TTL, want 2", got)
	}
}

// A burst on a cold cache must produce ONE database read, not one per caller --
// otherwise the stampede is the very thing the cache was meant to stop.
func TestUserDirectoryRefreshIsSingleFlight(t *testing.T) {
	loader := &countingLoader{
		rows:  []sshKeyRow{{userID: "u1", username: "alice", publicKey: testKeyA}},
		block: make(chan struct{}),
	}
	d, _ := newTestDirectory(loader, keyIndexTTL)

	const callers = 20
	var started, finished sync.WaitGroup
	started.Add(callers)
	finished.Add(callers)
	for range callers {
		go func() {
			started.Done()
			defer finished.Done()
			if _, err := d.Lookup(testKeyAFP); err != nil {
				t.Errorf("Lookup() error = %v", err)
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

// A failed refresh is reported, not silently answered from the previous
// snapshot: serving stale keys through a database outage would keep deactivated
// keys working for as long as the outage lasted.
func TestUserDirectoryReportsLoadFailure(t *testing.T) {
	wantErr := errors.New("connection refused")
	loader := &countingLoader{err: wantErr}
	d, _ := newTestDirectory(loader, keyIndexTTL)

	if _, err := d.Lookup(testKeyAFP); !errors.Is(err, wantErr) {
		t.Fatalf("Lookup() error = %v, want %v", err, wantErr)
	}
}

func TestUserDirectoryDoesNotServeStaleAfterFailedRefresh(t *testing.T) {
	loader := &countingLoader{rows: []sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyA},
	}}
	d, clock := newTestDirectory(loader, keyIndexTTL)

	if _, err := d.Lookup(testKeyAFP); err != nil {
		t.Fatalf("first Lookup() error = %v", err)
	}
	loader.mu.Lock()
	loader.err = errors.New("database went away")
	loader.mu.Unlock()
	clock.advance(keyIndexTTL + time.Second)

	if _, err := d.Lookup(testKeyAFP); err == nil {
		t.Fatal("Lookup() served the expired snapshot through a failed refresh")
	}
}

// The ambiguity rule has to survive precomputation: the same key held by two
// distinct users is refused, while one user holding it twice resolves cleanly.
// matchFingerprint's own tests cover this too -- it is now a wrapper over the
// same index -- but the cached path is what production uses.
func TestUserDirectoryAmbiguityRules(t *testing.T) {
	t.Run("two distinct users share a key", func(t *testing.T) {
		loader := &countingLoader{rows: []sshKeyRow{
			{userID: "u1", username: "alice", publicKey: testKeyA},
			{userID: "u2", username: "eve", publicKey: testKeyA},
		}}
		d, _ := newTestDirectory(loader, keyIndexTTL)
		if _, err := d.Lookup(testKeyAFP); err == nil {
			t.Fatal("Lookup() resolved a key registered to two distinct users")
		}
	})

	t.Run("one user registered the same key twice", func(t *testing.T) {
		loader := &countingLoader{rows: []sshKeyRow{
			{userID: "u1", username: "alice", publicKey: testKeyA},
			{userID: "u1", username: "alice", publicKey: testKeyA},
		}}
		d, _ := newTestDirectory(loader, keyIndexTTL)
		user, err := d.Lookup(testKeyAFP)
		if err != nil {
			t.Fatalf("Lookup() error = %v, want u1", err)
		}
		if user.ID != "u1" {
			t.Errorf("Lookup() = %q, want u1", user.ID)
		}
	})
}

// One malformed stored key must not make everybody else unidentifiable.
func TestBuildSSHKeyIndexSkipsUnparseableKeys(t *testing.T) {
	index := buildSSHKeyIndex([]sshKeyRow{
		{userID: "u1", username: "alice", publicKey: testKeyBogus},
		{userID: "u2", username: "bob", publicKey: testKeyB},
	})
	got, err := index.lookupUserID(testKeyBFP)
	if err != nil {
		t.Fatalf("lookupUserID() error = %v", err)
	}
	if got != "u2" {
		t.Errorf("lookupUserID() = %q, want u2", got)
	}
}
