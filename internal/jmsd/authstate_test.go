package jmsd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// stateStoreSuite is the behaviour every StateStore owes its callers,
// exercised against both implementations so the Redis one cannot quietly
// diverge from the in-memory one it replaces.
func stateStoreSuite(t *testing.T, newStore func(t *testing.T) StateStore) {
	t.Helper()

	t.Run("nonce is accepted once and refused after", func(t *testing.T) {
		s := newStore(t)
		nonce := uniqueKey("nonce")
		seen, err := s.SeenNonce(nonce)
		if err != nil {
			t.Fatalf("first SeenNonce: %v", err)
		}
		if seen {
			t.Fatal("a fresh nonce was reported as already seen")
		}
		seen, err = s.SeenNonce(nonce)
		if err != nil {
			t.Fatalf("second SeenNonce: %v", err)
		}
		if !seen {
			// This is the whole replay defence: a captured request resent
			// inside the 30s window has a valid signature and is rejected
			// only because its nonce is remembered.
			t.Fatal("a replayed nonce was accepted")
		}
	})

	t.Run("distinct nonces do not collide", func(t *testing.T) {
		s := newStore(t)
		for i := 0; i < 3; i++ {
			seen, err := s.SeenNonce(uniqueKey(fmt.Sprintf("nonce-%d", i)))
			if err != nil {
				t.Fatalf("SeenNonce: %v", err)
			}
			if seen {
				t.Fatalf("nonce %d was reported as already seen", i)
			}
		}
	})

	t.Run("unknown key is not bound", func(t *testing.T) {
		s := newStore(t)
		if _, ok, err := s.BoundKey(uniqueKey("fp")); err != nil || ok {
			t.Fatalf("BoundKey on a fresh key: ok=%v err=%v", ok, err)
		}
	})

	t.Run("binding round-trips and later sign-ins overwrite", func(t *testing.T) {
		s := newStore(t)
		fp := uniqueKey("fp")
		for _, user := range []string{"u1", "u2"} {
			want := keyBinding{UserID: user, PublicKey: "ssh-ed25519 AAAA"}
			if err := s.BindKey(fp, want, time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("BindKey: %v", err)
			}
			got, ok, err := s.BoundKey(fp)
			if err != nil || !ok || got != want {
				t.Fatalf("BoundKey = %+v ok=%v err=%v, want %+v", got, ok, err, want)
			}
		}
	})

	t.Run("expired binding is gone", func(t *testing.T) {
		s := newStore(t)
		fp := uniqueKey("fp")
		if err := s.BindKey(fp, keyBinding{UserID: "u1", PublicKey: "k"}, time.Now().Add(-time.Second)); err != nil {
			t.Fatalf("BindKey: %v", err)
		}
		// This is what sends a user back to the browser every interval.
		if _, ok, err := s.BoundKey(fp); err != nil || ok {
			t.Fatalf("expired binding still returned: ok=%v err=%v", ok, err)
		}
	})
}

func TestMemStateStore(t *testing.T) {
	t.Parallel()
	stateStoreSuite(t, func(t *testing.T) StateStore { return newMemStateStore() })
}

// TestMemStateStoreSweepsExpiredNonces covers the one behaviour the shared
// suite can't: memStateStore's map would grow forever without the sweep, and
// the sweep is also what lets a nonce be reused once its window has passed.
// Reaching into the map directly beats sleeping 30 seconds.
func TestMemStateStoreSweepsExpiredNonces(t *testing.T) {
	t.Parallel()
	s := newMemStateStore()
	if seen, _ := s.SeenNonce("n1"); seen {
		t.Fatal("a fresh nonce was reported as already seen")
	}
	s.mu.Lock()
	s.nonces["n1"] = time.Now().Add(-2 * nonceWindow)
	s.mu.Unlock()

	if seen, _ := s.SeenNonce("n2"); seen {
		t.Fatal("n2 was reported as already seen")
	}
	s.mu.Lock()
	_, stillThere := s.nonces["n1"]
	s.mu.Unlock()
	if stillThere {
		t.Error("an expired nonce survived the sweep, so the map grows unbounded")
	}
}

func TestMemStateStoreCloseIsNoop(t *testing.T) {
	t.Parallel()
	s := newMemStateStore()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.SeenNonce("n1"); err != nil {
		t.Fatalf("SeenNonce after Close: %v", err)
	}
}

// redisAddrEnv names a running Redis to test against. Unset, the Redis tests
// skip: the repo keeps no fakes, and pulling in a mock Redis to test a
// hundred lines of client calls costs more than it proves. Run them with
//
//	JMSD_TEST_REDIS_ADDR=127.0.0.1:6379 go test ./internal/jmsd/ -run Redis
const redisAddrEnv = "JMSD_TEST_REDIS_ADDR"

// testRedisDB is deliberately not 0: every key here is namespaced by
// uniqueKey anyway, but a stray FLUSHDB during debugging should not land on
// whatever else the developer keeps in db 0.
const testRedisDB = 15

// testRedisAddr returns the configured Redis address or skips the test.
func testRedisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv(redisAddrEnv)
	if addr == "" {
		t.Skipf("%s not set; skipping Redis-backed tests", redisAddrEnv)
	}
	return addr
}

func newTestRedisStore(t *testing.T) *redisStateStore {
	t.Helper()
	addr := testRedisAddr(t)
	store, err := newRedisStateStore(RedisConfig{Addr: addr, DB: testRedisDB})
	if err != nil {
		t.Fatalf("connect to redis at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		// Bindings carry an hour's TTL here, so without this every run leaves
		// litter in the developer's Redis. Scoped to
		// uniqueKey's "test-" prefix rather than FLUSHDB, which would take
		// out anything else living in this database.
		deleteTestKeys(t, store, nonceKeyPrefix+"test-*", keyKeyPrefix+"test-*")
		_ = store.Close()
	})
	return store
}

func deleteTestKeys(t *testing.T, store *redisStateStore, patterns ...string) {
	t.Helper()
	ctx := context.Background()
	for _, pattern := range patterns {
		iter := store.rdb.Scan(ctx, 0, pattern, 100).Iterator()
		for iter.Next(ctx) {
			if err := store.rdb.Del(ctx, iter.Val()).Err(); err != nil {
				t.Logf("clean up %s: %v", iter.Val(), err)
			}
		}
		if err := iter.Err(); err != nil {
			t.Logf("scan %s: %v", pattern, err)
		}
	}
}

func TestRedisStateStore(t *testing.T) {
	stateStoreSuite(t, func(t *testing.T) StateStore { return newTestRedisStore(t) })
}

// TestRedisStateStoreRefusesBadAddress is the startup contract: an
// unreachable Redis has to fail New rather than silently fall back to memory,
// so an operator's typo shows up when they start jmsd.
func TestRedisStateStoreRefusesBadAddress(t *testing.T) {
	t.Parallel()
	// Port 1 on loopback refuses immediately rather than hanging.
	if _, err := newRedisStateStore(RedisConfig{Addr: "127.0.0.1:1"}); err == nil {
		t.Fatal("newRedisStateStore accepted an address nothing is listening on")
	}
}

// TestRedisStateStoreSetsTTLs proves both kinds of key expire on their own.
// Without it Redis would accumulate one nonce per request forever, and a
// binding would never send anyone back to sign in.
func TestRedisStateStoreSetsTTLs(t *testing.T) {
	store := newTestRedisStore(t)
	ctx := context.Background()

	nonce := uniqueKey("nonce")
	if _, err := store.SeenNonce(nonce); err != nil {
		t.Fatalf("SeenNonce: %v", err)
	}
	ttl, err := store.rdb.TTL(ctx, nonceKeyPrefix+nonce).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > nonceWindow {
		t.Errorf("nonce TTL = %v, want between 0 and %v", ttl, nonceWindow)
	}

	fp := uniqueKey("fp")
	if err := store.BindKey(fp, keyBinding{UserID: "u1", PublicKey: "k"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("BindKey: %v", err)
	}
	ttl, err = store.rdb.TTL(ctx, keyKeyPrefix+fp).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > time.Hour {
		t.Errorf("binding TTL = %v, want between 0 and 1h", ttl)
	}
}

// TestRedisStateStoreRejectsCorruptBinding: the store shares an
// unauthenticated Redis, so anything could write over a binding. Reading
// garbage back must be an error, not a silent "not signed in" that re-prompts
// forever with no explanation.
func TestRedisStateStoreRejectsCorruptBinding(t *testing.T) {
	store := newTestRedisStore(t)
	fp := uniqueKey("garbled")
	if err := store.rdb.Set(context.Background(), keyKeyPrefix+fp, "not-json", time.Minute).Err(); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	_, _, err := store.BoundKey(fp)
	if !errors.Is(err, errStateUnavailable) {
		t.Errorf("error = %v, want it to wrap errStateUnavailable so authorize answers 503", err)
	}
}

// TestRedisStateStoreErrorsAfterClose stands in for "Redis went away
// mid-flight": the failure has to surface as errStateUnavailable, which is
// what turns it into a 503 instead of a fabricated "nonce already used".
func TestRedisStateStoreErrorsAfterClose(t *testing.T) {
	// Built directly rather than via newTestRedisStore: closing the client out
	// from under that helper's cleanup would only make it log failures at a
	// point where nothing was written and nothing needs cleaning.
	store, err := newRedisStateStore(RedisConfig{Addr: testRedisAddr(t), DB: testRedisDB})
	if err != nil {
		t.Fatalf("connect to redis: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.SeenNonce(uniqueKey("nonce")); !errors.Is(err, errStateUnavailable) {
		t.Errorf("SeenNonce error = %v, want errStateUnavailable", err)
	}
	if _, _, err := store.BoundKey(uniqueKey("fp")); !errors.Is(err, errStateUnavailable) {
		t.Errorf("BoundKey error = %v, want errStateUnavailable", err)
	}
	if err := store.BindKey(uniqueKey("fp"), keyBinding{UserID: "u1", PublicKey: "k"}, time.Now().Add(time.Hour)); !errors.Is(err, errStateUnavailable) {
		t.Errorf("BindKey error = %v, want errStateUnavailable", err)
	}
}

// uniqueKey keeps each test's keys to itself. The Redis under test is a real
// one the developer also uses, and a failed run leaves its keys behind -- so
// tests must not assume an empty keyspace, and must not clobber each other
// when run in any order.
func uniqueKey(prefix string) string {
	return fmt.Sprintf("test-%s-%d", prefix, testKeyID.Add(1))
}

var testKeyID atomic.Int64

// Compile-time proof that both stores satisfy the interface, so a signature
// change breaks here rather than at the one line in New that picks between
// them.
var (
	_ StateStore = (*memStateStore)(nil)
	_ StateStore = (*redisStateStore)(nil)
)
