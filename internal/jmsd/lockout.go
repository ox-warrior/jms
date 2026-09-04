package jmsd

import (
	"sync"
	"time"
)

const (
	// mfaMaxFailures is how many consecutive wrong codes a user may submit
	// before the MFA endpoint stops accepting any for mfaLockout.
	//
	// Five is chosen against what a person does, not what an attacker does: a
	// mistyped code, a code that expired mid-typing, and a phone showing the
	// wrong account are all one or two attempts, never five in a row.
	mfaMaxFailures = 5
	// mfaLockout is how long the MFA endpoint refuses a user after
	// mfaMaxFailures consecutive wrong codes.
	//
	// This is the whole defence, so it is worth stating what it buys. A TOTP
	// code is six digits and verifyTOTP accepts three time windows, so one
	// guess lands with probability about 3/10^6. Unthrottled -- which this
	// endpoint was -- that is not a defence at all: whoever holds a stolen SSH
	// private key can sign an unlimited number of requests, and at a hundred
	// attempts a second they expect to be through in around an hour. MFA
	// exists precisely for the case where the key is already compromised, so
	// the second factor has to survive it.
	//
	// At 5 attempts per 15 minutes the same search takes on the order of
	// centuries, and a real user who fatfingers a code five times waits a
	// quarter of an hour. JumpServer throttles its own login the same way.
	mfaLockout = 15 * time.Minute
)

// failureTracker counts consecutive failures per key and locks a key out once
// there have been too many.
//
// State lives in this process and is lost on restart, unlike the MFA
// timestamps in StateStore. That asymmetry is deliberate: forgetting a lockout
// only helps an attacker who can restart jmsd, and anyone who can do that has
// already won by other means -- whereas forgetting an MFA timestamp would make
// every user re-verify, which is why that one had to be persisted.
type failureTracker struct {
	max      int
	lockFor  time.Duration
	now      func() time.Time
	mu       sync.Mutex
	failures map[string]int
	// lockedUntil holds only keys currently locked; an entry is cleared the
	// first time it is consulted after expiring.
	lockedUntil map[string]time.Time
}

func newFailureTracker(max int, lockFor time.Duration) *failureTracker {
	return &failureTracker{
		max:         max,
		lockFor:     lockFor,
		now:         time.Now,
		failures:    make(map[string]int),
		lockedUntil: make(map[string]time.Time),
	}
}

// LockedFor reports how much longer key is locked out, and whether it is locked
// at all. An expired lock is cleared here rather than swept on a timer -- the
// keys are resolved JumpServer usernames, so there are only ever as many as
// there are real users and nothing unbounded accumulates.
func (t *failureTracker) LockedFor(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	until, ok := t.lockedUntil[key]
	if !ok {
		return 0, false
	}
	remaining := until.Sub(t.now())
	if remaining <= 0 {
		delete(t.lockedUntil, key)
		delete(t.failures, key)
		return 0, false
	}
	return remaining, true
}

// Fail records one failure for key, locking it out once max consecutive
// failures have accumulated. It reports whether this failure was the one that
// tripped the lock, so the caller can log that transition -- the interesting
// line is the moment it engages, not each attempt on the way there.
func (t *failureTracker) Fail(key string) (locked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.failures[key]++
	if t.failures[key] < t.max {
		return false
	}
	// Counter cleared along with setting the lock, so the next failure after
	// the lock expires starts a fresh run of max rather than re-locking
	// immediately on a single mistake.
	delete(t.failures, key)
	t.lockedUntil[key] = t.now().Add(t.lockFor)
	return true
}

// Reset clears key's failure run. Called on success: the threshold counts
// CONSECUTIVE failures, so someone who gets it wrong twice a week for months
// should never be locked out.
func (t *failureTracker) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, key)
	delete(t.lockedUntil, key)
}
