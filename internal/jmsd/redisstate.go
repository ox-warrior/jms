package jmsd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// nonceKeyPrefix / mfaKeyPrefix namespace jmsd's keys so a shared Redis
	// stays legible under `redis-cli KEYS 'jmsd:*'`.
	nonceKeyPrefix = "jmsd:nonce:"
	mfaKeyPrefix   = "jmsd:mfa:"
	// redisTimeout bounds a single command. StateStore's methods take no
	// context -- threading one through would touch every handler for a
	// sub-millisecond loopback call -- so the deadline is applied here
	// instead. It only has to be generous enough that a healthy local Redis
	// never trips it.
	redisTimeout = 2 * time.Second
)

// redisStateStore keeps jmsd's auth state in Redis so it survives a restart.
// The MFA timestamp is the point: without it, restarting jmsd resets every
// user's 10-day clock and makes them all re-verify. Nonces come along because
// SET NX EX is exactly the atomic check-and-record they need, and its TTL
// replaces memStateStore's per-request sweep of the whole map.
type redisStateStore struct {
	rdb *redis.Client
	// mfaTTL expires an MFA key at the same point cfg.MFAIntervalDays would
	// have declared it stale, so Redis cleans up after itself rather than
	// accumulating a key per user forever. mfaFresh still compares the stored
	// timestamp: the TTL was fixed when the key was written, so shortening
	// mfa_interval_days later leaves keys living too long, and only the
	// timestamp comparison enforces the new interval. (Lengthening it is
	// harmless the other way -- the key expires early and the user verifies
	// once more than strictly needed.)
	mfaTTL time.Duration
}

// newRedisStateStore dials Redis and proves it answers before returning.
// Failing here fails jmsd.New and so aborts startup, matching NewCoreDB's
// eager Ping: a wrong address should surface when the operator starts the
// service, not on some user's first request hours later. There is
// deliberately no fallback to memStateStore -- silently reverting would turn
// "why is it asking for MFA again?" into an unanswerable question.
func newRedisStateStore(cfg RedisConfig, mfaTTL time.Duration) (*redisStateStore, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Addr,
		DB:   cfg.DB,
		// Bounded explicitly rather than left at go-redis's defaults: an
		// unreachable Redis otherwise retries the dial five times per command
		// before giving up, turning "Redis is down" into seconds of hang on
		// every request instead of a prompt 503. Each command still gets
		// redisTimeout overall via its context.
		DialTimeout:  redisTimeout,
		ReadTimeout:  redisTimeout,
		WriteTimeout: redisTimeout,
		MaxRetries:   1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("ping redis %s: %w", cfg.Addr, err)
	}
	return &redisStateStore{rdb: rdb, mfaTTL: mfaTTL}, nil
}

// SeenNonce is one SET NX EX: Redis sets the key only if absent and reports
// whether it did, which is the same check-and-record atomicity memStateStore
// gets from holding its mutex across the read and the write. The value is
// irrelevant -- only the key's existence carries meaning -- and the TTL
// retires it exactly when the timestamp window in verifySignedRequest would
// have rejected the request anyway.
func (s *redisStateStore) SeenNonce(nonce string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	stored, err := s.rdb.SetNX(ctx, nonceKeyPrefix+nonce, "1", nonceWindow).Result()
	if err != nil {
		return false, fmt.Errorf("%w: record nonce: %v", errStateUnavailable, err)
	}
	return !stored, nil
}

func (s *redisStateStore) LastMFA(username string) (time.Time, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	raw, err := s.rdb.Get(ctx, mfaKeyPrefix+username).Result()
	if errors.Is(err, redis.Nil) {
		// No key means either never verified or expired past mfaTTL. Both are
		// "not fresh", which is what a missing entry already means to
		// mfaFresh, so neither is an error.
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%w: read last mfa: %v", errStateUnavailable, err)
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		// Someone else wrote a non-timestamp under our key. Refusing beats
		// guessing: treating it as "never verified" would be safe but would
		// silently re-prompt forever with no clue why.
		return time.Time{}, false, fmt.Errorf("%w: last mfa for %s is not a unix timestamp: %q", errStateUnavailable, username, raw)
	}
	return time.Unix(seconds, 0), true, nil
}

func (s *redisStateStore) RecordMFA(username string, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	value := strconv.FormatInt(at.Unix(), 10)
	if err := s.rdb.Set(ctx, mfaKeyPrefix+username, value, s.mfaTTL).Err(); err != nil {
		return fmt.Errorf("%w: record mfa: %v", errStateUnavailable, err)
	}
	return nil
}

func (s *redisStateStore) Close() error { return s.rdb.Close() }
