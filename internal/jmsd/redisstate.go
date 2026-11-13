package jmsd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// The prefixes namespace jmsd's keys so a shared Redis
	// stays legible under `redis-cli KEYS 'jmsd:*'`.
	nonceKeyPrefix = "jmsd:nonce:"
	keyKeyPrefix   = "jmsd:key:"
	// redisTimeout bounds a single command. StateStore's methods take no
	// context -- threading one through would touch every handler for a
	// sub-millisecond loopback call -- so the deadline is applied here
	// instead. It only has to be generous enough that a healthy local Redis
	// never trips it.
	redisTimeout = 2 * time.Second
)

// redisStateStore keeps jmsd's auth state in Redis so it survives a restart.
// The key bindings are the point: without them, restarting jmsd signs every
// user out. Nonces come along because
// SET NX EX is exactly the atomic check-and-record they need, and its TTL
// replaces memStateStore's per-request sweep of the whole map.
type redisStateStore struct {
	rdb *redis.Client
}

// newRedisStateStore dials Redis and proves it answers before returning.
// Failing here fails jmsd.New and so aborts startup, matching NewCoreDB's
// eager Ping: a wrong address should surface when the operator starts the
// service, not on some user's first request hours later. There is
// deliberately no fallback to memStateStore -- silently reverting would turn
// "why is it asking me to sign in again?" into an unanswerable question.
func newRedisStateStore(cfg RedisConfig) (*redisStateStore, error) {
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
	return &redisStateStore{rdb: rdb}, nil
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

// BindKey stores the binding with a TTL ending at until, so Redis itself
// retires it; BoundKey then needs no timestamp of its own.
func (s *redisStateStore) BindKey(fp string, b keyBinding, until time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	ttl := time.Until(until)
	if ttl <= 0 {
		return nil // already expired; a negative TTL would make go-redis store it forever
	}
	value, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err := s.rdb.Set(ctx, keyKeyPrefix+fp, value, ttl).Err(); err != nil {
		return fmt.Errorf("%w: bind key: %v", errStateUnavailable, err)
	}
	return nil
}

func (s *redisStateStore) BoundKey(fp string) (keyBinding, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisTimeout)
	defer cancel()
	raw, err := s.rdb.Get(ctx, keyKeyPrefix+fp).Bytes()
	if errors.Is(err, redis.Nil) {
		return keyBinding{}, false, nil
	}
	if err != nil {
		return keyBinding{}, false, fmt.Errorf("%w: read key binding: %v", errStateUnavailable, err)
	}
	var b keyBinding
	if err := json.Unmarshal(raw, &b); err != nil || b.UserID == "" || b.PublicKey == "" {
		return keyBinding{}, false, fmt.Errorf("%w: key binding for %s is malformed", errStateUnavailable, fp)
	}
	return b, true, nil
}

func (s *redisStateStore) Close() error { return s.rdb.Close() }
