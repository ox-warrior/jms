package jmsd

import (
	"errors"
	"sync"
	"time"
)

// nonceWindow does double duty: it is both how far a signed request's
// timestamp may be from now and how long its nonce is remembered for replay
// detection (docs/PLAN.md §4: "每个请求携带 SSH 私钥签名 + 时间戳，jmsd 验签
// 并记住每个 nonce 30 秒来防重放"). The two must match -- remembering nonces
// for less time than the timestamp window would leave a gap in which a
// captured request is both still fresh and no longer recognised.
const nonceWindow = 30 * time.Second

// errStateUnavailable marks a failure to reach the state backend, as opposed
// to a failure to authenticate. The distinction matters at the HTTP layer:
// the caller's signature may be perfectly good and jmsd simply cannot tell,
// so this maps to 503, not 401. Answering 401 -- or worse, letting a store
// error read as "nonce already used" -- would send someone hunting a replay
// attack that never happened.
var errStateUnavailable = errors.New("auth state store unavailable")

// StateStore holds jmsd's auth state: which nonces have been used, and when
// each user last completed MFA. Two implementations: memStateStore below,
// which loses everything on restart, and redisStateStore, which doesn't.
//
// Every method returns an error because the Redis implementation can fail at
// any call. Returning a plain bool would force it to guess, and both guesses
// are bad: SeenNonce reporting "not seen" silently disables replay
// protection, while reporting "seen" rejects every request with a fabricated
// reason.
type StateStore interface {
	// SeenNonce records nonce if it hasn't been seen within nonceWindow and
	// returns false; returns true (without recording) if it was already seen.
	// Check-and-record is atomic: two concurrent replays of the same captured
	// request must not both pass.
	SeenNonce(nonce string) (bool, error)
	// LastMFA returns the last time username completed MFA, if ever.
	LastMFA(username string) (time.Time, bool, error)
	// RecordMFA sets username's last-MFA timestamp.
	RecordMFA(username string, at time.Time) error
	// Close releases whatever the implementation holds open.
	Close() error
}

// memStateStore is a mutex-guarded in-memory StateStore, used when no Redis
// is configured. State is lost on restart: last_mfa_at resets to "never", so
// every user re-verifies MFA once after jmsd restarts. Configure Redis (see
// RedisConfig) to avoid that.
type memStateStore struct {
	mu      sync.Mutex
	nonces  map[string]time.Time // nonce -> when it was recorded
	lastMFA map[string]time.Time // username -> last successful MFA
}

func newMemStateStore() *memStateStore {
	return &memStateStore{
		nonces:  make(map[string]time.Time),
		lastMFA: make(map[string]time.Time),
	}
}

// The error returns are always nil here -- an in-memory map cannot fail.
// They exist to satisfy StateStore, whose Redis implementation can.

func (s *memStateStore) SeenNonce(nonce string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.sweepNoncesLocked(now)

	if _, ok := s.nonces[nonce]; ok {
		return true, nil
	}
	s.nonces[nonce] = now
	return false, nil
}

// sweepNoncesLocked drops nonces older than nonceWindow so the map doesn't
// grow unbounded. Must be called with s.mu held.
func (s *memStateStore) sweepNoncesLocked(now time.Time) {
	for n, seenAt := range s.nonces {
		if now.Sub(seenAt) > nonceWindow {
			delete(s.nonces, n)
		}
	}
}

func (s *memStateStore) LastMFA(username string) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastMFA[username]
	return t, ok, nil
}

func (s *memStateStore) RecordMFA(username string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMFA[username] = at
	return nil
}

func (s *memStateStore) Close() error { return nil }
