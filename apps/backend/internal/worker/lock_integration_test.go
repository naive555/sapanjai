package worker_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	appredis "github.com/sapanjai/backend/internal/infra/redis"
	"github.com/sapanjai/backend/internal/worker"
)

// setupRedisLock skips unless REDIS_URL is set, and returns a RedisLock
// plus the underlying client for cleanup — same skip convention as
// internal/infra/database/database_test.go.
func setupRedisLock(t *testing.T) (*worker.RedisLock, *redis.Client) {
	t.Helper()

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		t.Skip("REDIS_URL not set; skipping integration test")
	}

	rdb, err := appredis.New(context.Background(), redisURL)
	if err != nil {
		t.Fatalf("redis.New: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	return worker.NewRedisLock(rdb), rdb
}

// lockKey mirrors the unexported "worker:lock:" prefix documented in
// CLAUDE.md's Redis keys list, so tests can force-clean a key regardless of
// which owner (if any) currently holds it.
func lockKey(name string) string {
	return "worker:lock:" + name
}

func TestRedisLock_AcquireExclusivity(t *testing.T) {
	lock, rdb := setupRedisLock(t)
	ctx := context.Background()
	name := "test-lock-" + uuid.NewString()
	t.Cleanup(func() { _ = rdb.Del(context.Background(), lockKey(name)).Err() })

	ok, err := lock.Acquire(ctx, name, "owner-a", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !ok {
		t.Fatal("expected the first Acquire to succeed")
	}

	ok, err = lock.Acquire(ctx, name, "owner-b", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if ok {
		t.Fatal("expected a second Acquire (different owner) to fail while the lock is held")
	}
}

func TestRedisLock_ReleaseByOwnerAllowsReacquire(t *testing.T) {
	lock, rdb := setupRedisLock(t)
	ctx := context.Background()
	name := "test-lock-" + uuid.NewString()
	t.Cleanup(func() { _ = rdb.Del(context.Background(), lockKey(name)).Err() })

	ok, err := lock.Acquire(ctx, name, "owner-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("initial Acquire: ok=%v err=%v", ok, err)
	}

	if err := lock.Release(ctx, name, "owner-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	ok, err = lock.Acquire(ctx, name, "owner-b", time.Minute)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if !ok {
		t.Fatal("expected Acquire to succeed after the true owner released the lock")
	}
}

func TestRedisLock_ReleaseByWrongOwnerIsNoop(t *testing.T) {
	lock, rdb := setupRedisLock(t)
	ctx := context.Background()
	name := "test-lock-" + uuid.NewString()
	t.Cleanup(func() { _ = rdb.Del(context.Background(), lockKey(name)).Err() })

	ok, err := lock.Acquire(ctx, name, "owner-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("initial Acquire: ok=%v err=%v", ok, err)
	}

	if err := lock.Release(ctx, name, "owner-b"); err != nil {
		t.Fatalf("Release (wrong owner) returned an error: %v", err)
	}

	ok, err = lock.Acquire(ctx, name, "owner-c", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if ok {
		t.Fatal("expected the original holder's lock to survive a wrong-owner release")
	}
}

func TestRedisLock_ExpiresAfterTTL(t *testing.T) {
	lock, rdb := setupRedisLock(t)
	ctx := context.Background()
	name := "test-lock-" + uuid.NewString()
	t.Cleanup(func() { _ = rdb.Del(context.Background(), lockKey(name)).Err() })

	ok, err := lock.Acquire(ctx, name, "owner-a", 50*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("initial Acquire: ok=%v err=%v", ok, err)
	}

	time.Sleep(150 * time.Millisecond)

	ok, err = lock.Acquire(ctx, name, "owner-b", time.Minute)
	if err != nil {
		t.Fatalf("Acquire after TTL expiry: %v", err)
	}
	if !ok {
		t.Fatal("expected Acquire to succeed once the TTL expired")
	}
}
