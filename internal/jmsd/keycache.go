package jmsd

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// userListTTL is how long the active-user list is served before Core's
// database is read again. It bounds how long a user disabled in JumpServer
// keeps working through a bound key.
const userListTTL = 30 * time.Second

// errUnknownKey means nobody owns the key: no Google sign-in bound it (or the
// binding lapsed) and it is not a service key. Answered as "sign in", since
// that is how a fresh jms gets its key known.
var errUnknownKey = errors.New("this key is not signed in")

// userDirectory answers "who is user X" from a cached copy of Core's active
// users, refreshing it at most once per ttl.
//
// The cache is the point, not an optimisation: identify runs before a
// signature is checked, so anyone who can reach the port could otherwise
// make jmsd query Core's production database once per garbage request.
type userDirectory struct {
	// load is CoreDB.activeUsers in production, injected so the caching and
	// single-flight behaviour can be tested without a database.
	load func() ([]userRecord, error)
	ttl  time.Duration
	// now is time.Now in production, injected so expiry is testable without
	// sleeping.
	now func() time.Time

	// mu guards users/builtAt AND is held across the refresh, which is what
	// makes the refresh single-flight: a burst arriving on an expired cache
	// produces one query with the rest waiting on it.
	mu      sync.Mutex
	users   []userRecord
	builtAt time.Time
	loaded  bool
}

func newUserDirectory(load func() ([]userRecord, error), ttl time.Duration) *userDirectory {
	return &userDirectory{load: load, ttl: ttl, now: time.Now}
}

// ByID returns the active user with this id. An error means the user was
// disabled or deleted in JumpServer.
func (d *userDirectory) ByID(userID string) (*userRecord, error) {
	return d.find(func(u userRecord) bool { return u.ID == userID }, "id "+userID)
}

// ByUsername returns the active user with this username (service keys name
// their user this way in jmsd's config).
func (d *userDirectory) ByUsername(username string) (*userRecord, error) {
	return d.find(func(u userRecord) bool { return u.Username == username }, "username "+username)
}

// ByEmail returns the one active user with this email. Two users sharing an
// email is refused rather than resolved: picking one would sign the caller in
// as whichever happened to sort first.
func (d *userDirectory) ByEmail(email string) (*userRecord, error) {
	return d.find(func(u userRecord) bool { return strings.EqualFold(u.Email, email) }, "email "+email)
}

func (d *userDirectory) find(match func(userRecord) bool, what string) (*userRecord, error) {
	users, err := d.current()
	if err != nil {
		return nil, err
	}
	var found *userRecord
	for _, u := range users {
		if !match(u) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("more than one active JumpServer user has %s", what)
		}
		u := u
		found = &u
	}
	if found == nil {
		return nil, fmt.Errorf("no active JumpServer user has %s", what)
	}
	return found, nil
}

// current returns the cached list, reloading it when it has expired.
//
// A failed reload is returned rather than papered over with the previous
// list: serving a stale snapshot through a database outage would keep
// disabled users working for as long as the outage lasted.
func (d *userDirectory) current() ([]userRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.loaded && d.now().Sub(d.builtAt) < d.ttl {
		return d.users, nil
	}
	started := d.now()
	users, err := d.load()
	if err != nil {
		d.loaded = false
		return nil, err
	}
	d.users, d.builtAt, d.loaded = users, d.now(), true
	debugf("user list: refreshed %d active users in %s", len(users), d.builtAt.Sub(started).Round(time.Millisecond))
	return d.users, nil
}
