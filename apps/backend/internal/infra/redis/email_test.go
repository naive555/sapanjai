package redis_test

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	appredis "github.com/sapanjai/backend/internal/infra/redis"
)

// testKeyPrefix namespaces every key this package's tests write. It is
// uuid-suffixed per run for the same reason uniqueTokenHash and
// newTestLimiter's connector id are: the REDIS_URL under test may be a
// shared instance, and a run must not observe or clobber another's keys.
// It also exercises the prefixing itself — a helper that dropped the prefix
// on one side of a set/get pair would fail here rather than in production.
var testKeyPrefix = "test:" + uuid.NewString() + ":"

// newTestEmail skips unless REDIS_URL is set (matches
// internal/server's setupTestServer / ratelimit_test.go's newTestLimiter
// convention) and returns an Email helper backed by the real Redis
// instance plus a fresh, uuid-suffixed token hash so concurrent test runs
// never collide on a key.
func newTestEmail(t *testing.T) *appredis.Email {
	t.Helper()

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		t.Skip("REDIS_URL not set; skipping redis email test")
	}

	ctx := context.Background()
	client, err := appredis.New(ctx, redisURL)
	if err != nil {
		t.Fatalf("appredis.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	return appredis.NewEmail(client, testKeyPrefix)
}

func uniqueTokenHash(prefix string) string {
	return prefix + "-" + uuid.NewString()
}

func TestEmail_SetAndConsumeVerifyToken(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()
	tokenHash := uniqueTokenHash("verify")
	userID := uuid.New()

	if err := e.SetVerifyToken(ctx, tokenHash, userID); err != nil {
		t.Fatalf("SetVerifyToken: %v", err)
	}

	got, found, err := e.ConsumeVerifyToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("ConsumeVerifyToken: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true for a freshly set token")
	}
	if got != userID {
		t.Fatalf("userID = %v, want %v", got, userID)
	}
}

func TestEmail_ConsumeVerifyToken_SingleUse(t *testing.T) {
	// GETDEL semantics: a second consume of the same token, after the first
	// already succeeded, must report not-found — never the same userID
	// twice.
	e := newTestEmail(t)
	ctx := context.Background()
	tokenHash := uniqueTokenHash("verify-single-use")
	userID := uuid.New()

	if err := e.SetVerifyToken(ctx, tokenHash, userID); err != nil {
		t.Fatalf("SetVerifyToken: %v", err)
	}

	if _, found, err := e.ConsumeVerifyToken(ctx, tokenHash); err != nil || !found {
		t.Fatalf("first ConsumeVerifyToken: found=%v err=%v", found, err)
	}

	got, found, err := e.ConsumeVerifyToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("second ConsumeVerifyToken: %v", err)
	}
	if found {
		t.Fatalf("second ConsumeVerifyToken found = true (userID %v), want false — token must be single-use", got)
	}
}

func TestEmail_ConsumeVerifyToken_UnknownToken(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()

	_, found, err := e.ConsumeVerifyToken(ctx, uniqueTokenHash("never-set"))
	if err != nil {
		t.Fatalf("ConsumeVerifyToken: %v", err)
	}
	if found {
		t.Fatal("found = true for a token that was never set")
	}
}

func TestEmail_MarkVerifyResent_CooldownBlocksSecondCall(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()
	userID := uuid.New()

	first, err := e.MarkVerifyResent(ctx, userID)
	if err != nil {
		t.Fatalf("first MarkVerifyResent: %v", err)
	}
	if !first {
		t.Fatal("first MarkVerifyResent = false, want true (no prior cooldown)")
	}

	second, err := e.MarkVerifyResent(ctx, userID)
	if err != nil {
		t.Fatalf("second MarkVerifyResent: %v", err)
	}
	if second {
		t.Fatal("second MarkVerifyResent = true, want false while the cooldown is active")
	}
}

func TestEmail_MarkVerifyResent_IndependentPerUser(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()

	okA, err := e.MarkVerifyResent(ctx, uuid.New())
	if err != nil || !okA {
		t.Fatalf("user A MarkVerifyResent: ok=%v err=%v", okA, err)
	}
	okB, err := e.MarkVerifyResent(ctx, uuid.New())
	if err != nil || !okB {
		t.Fatalf("user B MarkVerifyResent: ok=%v err=%v", okB, err)
	}
}

func TestEmail_SetAndConsumeResetToken(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()
	tokenHash := uniqueTokenHash("reset")
	userID := uuid.New()

	if err := e.SetResetToken(ctx, tokenHash, userID); err != nil {
		t.Fatalf("SetResetToken: %v", err)
	}

	got, found, err := e.ConsumeResetToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("ConsumeResetToken: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true for a freshly set token")
	}
	if got != userID {
		t.Fatalf("userID = %v, want %v", got, userID)
	}
}

func TestEmail_ConsumeResetToken_SingleUse(t *testing.T) {
	// GETDEL semantics: a second consume of the same token, after the first
	// already succeeded, must report not-found — never the same userID
	// twice.
	e := newTestEmail(t)
	ctx := context.Background()
	tokenHash := uniqueTokenHash("reset-single-use")
	userID := uuid.New()

	if err := e.SetResetToken(ctx, tokenHash, userID); err != nil {
		t.Fatalf("SetResetToken: %v", err)
	}

	if _, found, err := e.ConsumeResetToken(ctx, tokenHash); err != nil || !found {
		t.Fatalf("first ConsumeResetToken: found=%v err=%v", found, err)
	}

	got, found, err := e.ConsumeResetToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("second ConsumeResetToken: %v", err)
	}
	if found {
		t.Fatalf("second ConsumeResetToken found = true (userID %v), want false — token must be single-use", got)
	}
}

func TestEmail_ConsumeResetToken_UnknownToken(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()

	_, found, err := e.ConsumeResetToken(ctx, uniqueTokenHash("reset-never-set"))
	if err != nil {
		t.Fatalf("ConsumeResetToken: %v", err)
	}
	if found {
		t.Fatal("found = true for a token that was never set")
	}
}

func TestEmail_MarkResetRequested_CooldownBlocksSecondCall(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()
	email := uniqueTokenHash("reset-cooldown") + "@example.com"

	first, err := e.MarkResetRequested(ctx, email)
	if err != nil {
		t.Fatalf("first MarkResetRequested: %v", err)
	}
	if !first {
		t.Fatal("first MarkResetRequested = false, want true (no prior cooldown)")
	}

	second, err := e.MarkResetRequested(ctx, email)
	if err != nil {
		t.Fatalf("second MarkResetRequested: %v", err)
	}
	if second {
		t.Fatal("second MarkResetRequested = true, want false while the cooldown is active")
	}
}

func TestEmail_MarkResetRequested_IndependentPerEmail(t *testing.T) {
	e := newTestEmail(t)
	ctx := context.Background()

	okA, err := e.MarkResetRequested(ctx, uniqueTokenHash("reset-a")+"@example.com")
	if err != nil || !okA {
		t.Fatalf("email A MarkResetRequested: ok=%v err=%v", okA, err)
	}
	okB, err := e.MarkResetRequested(ctx, uniqueTokenHash("reset-b")+"@example.com")
	if err != nil || !okB {
		t.Fatalf("email B MarkResetRequested: ok=%v err=%v", okB, err)
	}
}
