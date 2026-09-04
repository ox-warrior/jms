package jmsd

import (
	"sync"
	"testing"
	"time"
)

func newTestTracker(max int, lockFor time.Duration) (*failureTracker, *testClock) {
	clock := &testClock{t: time.Unix(1700000000, 0)}
	t := newFailureTracker(max, lockFor)
	t.now = clock.now
	return t, clock
}

// The threshold is where it engages: attempts up to max-1 must still be
// answered, or a user who mistypes once is locked out.
func TestFailureTrackerLocksOnlyAtTheThreshold(t *testing.T) {
	tracker, _ := newTestTracker(mfaMaxFailures, mfaLockout)

	for i := 1; i < mfaMaxFailures; i++ {
		if locked := tracker.Fail("alice"); locked {
			t.Fatalf("Fail() locked at attempt %d, want no lock before %d", i, mfaMaxFailures)
		}
		if _, locked := tracker.LockedFor("alice"); locked {
			t.Fatalf("LockedFor() reports locked after %d failures, want not locked", i)
		}
	}
	if locked := tracker.Fail("alice"); !locked {
		t.Fatalf("Fail() did not lock at attempt %d", mfaMaxFailures)
	}
	remaining, locked := tracker.LockedFor("alice")
	if !locked {
		t.Fatal("LockedFor() reports not locked after reaching the threshold")
	}
	if remaining != mfaLockout {
		t.Errorf("remaining = %s, want %s", remaining, mfaLockout)
	}
}

// This is the brute-force case the tracker exists for: whoever holds a stolen
// SSH key can sign unlimited requests, so the endpoint has to keep refusing
// however many more they send.
func TestFailureTrackerKeepsRefusingWhileLocked(t *testing.T) {
	tracker, clock := newTestTracker(mfaMaxFailures, mfaLockout)
	for range mfaMaxFailures {
		tracker.Fail("alice")
	}
	for range 1000 {
		if _, locked := tracker.LockedFor("alice"); !locked {
			t.Fatal("LockedFor() stopped reporting a lock while it was still in force")
		}
		clock.advance(time.Millisecond)
	}
}

// The lock has to actually expire, or a fatfingering user is locked out for
// good.
func TestFailureTrackerLockExpires(t *testing.T) {
	tracker, clock := newTestTracker(mfaMaxFailures, mfaLockout)
	for range mfaMaxFailures {
		tracker.Fail("alice")
	}

	clock.advance(mfaLockout - time.Second)
	if _, locked := tracker.LockedFor("alice"); !locked {
		t.Fatal("LockedFor() released the lock one second early")
	}
	clock.advance(2 * time.Second)
	if _, locked := tracker.LockedFor("alice"); locked {
		t.Fatal("LockedFor() still locked after the cooldown elapsed")
	}
}

// After a lock expires, a single further mistake must not re-lock immediately:
// the counter starts a fresh run of max.
func TestFailureTrackerStartsFreshRunAfterExpiry(t *testing.T) {
	tracker, clock := newTestTracker(mfaMaxFailures, mfaLockout)
	for range mfaMaxFailures {
		tracker.Fail("alice")
	}
	clock.advance(mfaLockout + time.Second)
	if _, locked := tracker.LockedFor("alice"); locked {
		t.Fatal("lock did not expire")
	}

	if locked := tracker.Fail("alice"); locked {
		t.Fatal("Fail() re-locked on the first mistake after the cooldown")
	}
}

// Consecutive, not cumulative: getting it right resets the run, so occasional
// mistakes spread over months never add up to a lockout.
func TestFailureTrackerResetClearsTheRun(t *testing.T) {
	tracker, _ := newTestTracker(mfaMaxFailures, mfaLockout)

	for range mfaMaxFailures - 1 {
		tracker.Fail("alice")
	}
	tracker.Reset("alice")
	for i := 1; i < mfaMaxFailures; i++ {
		if locked := tracker.Fail("alice"); locked {
			t.Fatalf("Fail() locked at attempt %d after a Reset, want a fresh run", i)
		}
	}
}

// One user's failures must not lock anybody else out.
func TestFailureTrackerIsPerKey(t *testing.T) {
	tracker, _ := newTestTracker(mfaMaxFailures, mfaLockout)
	for range mfaMaxFailures {
		tracker.Fail("alice")
	}
	if _, locked := tracker.LockedFor("alice"); !locked {
		t.Fatal("alice should be locked")
	}
	if _, locked := tracker.LockedFor("bob"); locked {
		t.Error("bob is locked out by alice's failures")
	}
}

// A key that never failed is never locked.
func TestFailureTrackerUnknownKeyIsNotLocked(t *testing.T) {
	tracker, _ := newTestTracker(mfaMaxFailures, mfaLockout)
	if remaining, locked := tracker.LockedFor("nobody"); locked || remaining != 0 {
		t.Errorf("LockedFor(unknown) = %s/%v, want 0/false", remaining, locked)
	}
}

// Concurrent submissions must not let more than max failures through before the
// lock engages -- the count would otherwise be lost to a race and the threshold
// would mean nothing.
func TestFailureTrackerConcurrentFailuresLockExactlyOnce(t *testing.T) {
	tracker, _ := newTestTracker(mfaMaxFailures, mfaLockout)

	const attempts = 200
	var wg sync.WaitGroup
	locks := make(chan bool, attempts)
	wg.Add(attempts)
	for range attempts {
		go func() {
			defer wg.Done()
			locks <- tracker.Fail("alice")
		}()
	}
	wg.Wait()
	close(locks)

	engaged := 0
	for locked := range locks {
		if locked {
			engaged++
		}
	}
	// 200 failures over runs of 5 can trip the lock at most 200/5 times; the
	// point is that it is a whole number of runs and never zero.
	if engaged == 0 {
		t.Fatal("no lock engaged across 200 concurrent failures")
	}
	if engaged > attempts/mfaMaxFailures {
		t.Errorf("lock engaged %d times, want at most %d", engaged, attempts/mfaMaxFailures)
	}
}
