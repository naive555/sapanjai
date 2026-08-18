package redis_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	appredis "github.com/sapanjai/backend/internal/infra/redis"
)

// newTestLimiter skips unless REDIS_URL is set (matches
// internal/server's setupTestServer convention) and returns a RateLimiter
// backed by the real Redis instance plus a fresh, uuid-suffixed connector
// id so concurrent test runs never share a bucket.
func newTestLimiter(t *testing.T, perMinute int) (*appredis.RateLimiter, string) {
	t.Helper()

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		t.Skip("REDIS_URL not set; skipping redis rate limit test")
	}

	ctx := context.Background()
	client, err := appredis.New(ctx, redisURL)
	if err != nil {
		t.Fatalf("appredis.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	connectorID := "ratelimit-test-" + uuid.NewString()
	return appredis.NewRateLimiter(client, perMinute), connectorID
}

func TestRateLimiter_AllowsUnderBudget(t *testing.T) {
	limiter, connectorID := newTestLimiter(t, 5)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		allowed, retryAfter, err := limiter.Take(ctx, connectorID, 1)
		if err != nil {
			t.Fatalf("Take[%d]: %v", i, err)
		}
		if !allowed {
			t.Fatalf("Take[%d] = denied, want allowed (capacity 5, this is call %d)", i, i+1)
		}
		if retryAfter != 0 {
			t.Errorf("Take[%d] retryAfter = %v, want 0 when allowed", i, retryAfter)
		}
	}
}

func TestRateLimiter_DeniesAtLimit(t *testing.T) {
	limiter, connectorID := newTestLimiter(t, 3)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		allowed, _, err := limiter.Take(ctx, connectorID, 1)
		if err != nil {
			t.Fatalf("Take[%d]: %v", i, err)
		}
		if !allowed {
			t.Fatalf("Take[%d] = denied, want allowed within the 3-token budget", i)
		}
	}

	allowed, retryAfter, err := limiter.Take(ctx, connectorID, 1)
	if err != nil {
		t.Fatalf("Take (4th): %v", err)
	}
	if allowed {
		t.Fatal("Take (4th) = allowed, want denied: budget of 3 already spent")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v, want > 0 when denied", retryAfter)
	}
}

func TestRateLimiter_ChargesRequestedUnitsInOneCall(t *testing.T) {
	limiter, connectorID := newTestLimiter(t, 10)
	ctx := context.Background()

	// A single call charging n=7 must leave only 3 tokens, not 10 minus 1
	// (the floor-of-1 a naive per-call-not-per-unit implementation might
	// apply) — this is the "n matters" contract callers rely on to charge
	// more than one unit per upstream fan-out.
	allowed, _, err := limiter.Take(ctx, connectorID, 7)
	if err != nil {
		t.Fatalf("Take(7): %v", err)
	}
	if !allowed {
		t.Fatal("Take(7) = denied, want allowed: capacity is 10")
	}

	allowed, _, err = limiter.Take(ctx, connectorID, 3)
	if err != nil {
		t.Fatalf("Take(3): %v", err)
	}
	if !allowed {
		t.Fatal("Take(3) = denied, want allowed: exactly 3 tokens should remain after charging 7 of 10")
	}

	allowed, retryAfter, err := limiter.Take(ctx, connectorID, 1)
	if err != nil {
		t.Fatalf("Take(1) after exhausting: %v", err)
	}
	if allowed {
		t.Fatal("Take(1) = allowed, want denied: the 10-token bucket is now fully spent (7 + 3)")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v, want > 0", retryAfter)
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	// Capacity 60/min refills at 1 token/second, so waiting a little over a
	// second after exhausting the bucket should free up exactly one more
	// admission — small enough to keep the test fast, large enough to be
	// robust against normal scheduling jitter.
	limiter, connectorID := newTestLimiter(t, 60)
	ctx := context.Background()

	allowed, _, err := limiter.Take(ctx, connectorID, 60)
	if err != nil {
		t.Fatalf("Take(60): %v", err)
	}
	if !allowed {
		t.Fatal("Take(60) = denied, want allowed: exactly the full capacity")
	}

	allowed, _, err = limiter.Take(ctx, connectorID, 1)
	if err != nil {
		t.Fatalf("Take(1) immediately after exhausting: %v", err)
	}
	if allowed {
		t.Fatal("Take(1) = allowed immediately after exhausting the bucket, want denied")
	}

	time.Sleep(1200 * time.Millisecond)

	allowed, _, err = limiter.Take(ctx, connectorID, 1)
	if err != nil {
		t.Fatalf("Take(1) after refill wait: %v", err)
	}
	if !allowed {
		t.Fatal("Take(1) = denied after waiting > 1s for a 1 token/sec refill, want allowed")
	}
}

func TestRateLimiter_RetryAfterIsSane(t *testing.T) {
	// Capacity 60/min => 1 token/sec refill. Fully exhausting the bucket
	// and asking for one more token should report a retry-after of about 1
	// second — never zero (that would mean "allowed"), never anywhere near
	// the full 60s window a single missing token doesn't justify.
	limiter, connectorID := newTestLimiter(t, 60)
	ctx := context.Background()

	if allowed, _, err := limiter.Take(ctx, connectorID, 60); err != nil || !allowed {
		t.Fatalf("Take(60): allowed=%v err=%v", allowed, err)
	}

	allowed, retryAfter, err := limiter.Take(ctx, connectorID, 1)
	if err != nil {
		t.Fatalf("Take(1): %v", err)
	}
	if allowed {
		t.Fatal("Take(1) = allowed, want denied")
	}
	if retryAfter < time.Second || retryAfter > 3*time.Second {
		t.Errorf("retryAfter = %v, want roughly 1 second (1 token/sec refill, 1 token short)", retryAfter)
	}
}

func TestRateLimiter_ZeroOrNegativeNTreatedAsOne(t *testing.T) {
	limiter, connectorID := newTestLimiter(t, 1)
	ctx := context.Background()

	allowed, _, err := limiter.Take(ctx, connectorID, 0)
	if err != nil {
		t.Fatalf("Take(0): %v", err)
	}
	if !allowed {
		t.Fatal("Take(0) = denied, want allowed: n<1 must be treated as n=1 against a capacity-1 bucket")
	}

	// The single token is now spent (by the n=0 -> n=1 call above).
	allowed, _, err = limiter.Take(ctx, connectorID, 1)
	if err != nil {
		t.Fatalf("Take(1): %v", err)
	}
	if allowed {
		t.Fatal("Take(1) = allowed, want denied: the capacity-1 bucket's only token was already spent")
	}
}
