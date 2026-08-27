package redis

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// verifyTokenTTL is how long a verification token stays redeemable. Product
// decision, not a deployment knob — see CLAUDE.md / the email-verification
// plan (§2.1).
const verifyTokenTTL = 24 * time.Hour

// verifyResendCooldown bounds how often a caller can trigger a fresh
// verification email for the same user.
const verifyResendCooldown = 5 * time.Minute

// Email wraps a Redis client with the email-verification token and resend
// helpers, mirroring Auth's shape (same package, same "hash the token
// before it becomes a key" precedent as mcp_api_keys — see
// docs/07-sheets-adapter-decisions.md §1).
type Email struct {
	client *redis.Client
}

// NewEmail wraps an already-connected client.
func NewEmail(client *redis.Client) *Email {
	return &Email{client: client}
}

// SetVerifyToken stores tokenHash -> userID for 24h, key
// "verify:email:<tokenHash>". tokenHash is the caller's sha256 hex digest
// of the raw token that was actually emailed — the raw token itself never
// reaches Redis.
func (e *Email) SetVerifyToken(ctx context.Context, tokenHash string, userID uuid.UUID) error {
	return e.client.Set(ctx, "verify:email:"+tokenHash, userID.String(), verifyTokenTTL).Err()
}

// ConsumeVerifyToken atomically reads and deletes the key for tokenHash
// (GETDEL), so a token is redeemable exactly once with no read-then-delete
// race: two concurrent redemptions of the same token can never both
// observe found == true. found is false when the key was absent or already
// expired/consumed.
func (e *Email) ConsumeVerifyToken(ctx context.Context, tokenHash string) (userID uuid.UUID, found bool, err error) {
	val, err := e.client.GetDel(ctx, "verify:email:"+tokenHash).Result()
	if errors.Is(err, redis.Nil) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}

	parsed, err := uuid.Parse(val)
	if err != nil {
		return uuid.Nil, false, err
	}
	return parsed, true, nil
}

// MarkVerifyResent sets the 5-minute resend cooldown for userID ("SET ...
// EX 300 NX") and reports whether it was newly set. false means the
// cooldown was already active — one round trip instead of an
// EXISTS-then-SET pair, and free of the race between them.
func (e *Email) MarkVerifyResent(ctx context.Context, userID uuid.UUID) (bool, error) {
	ok, err := e.client.SetNX(ctx, "verify:resend:"+userID.String(), "1", verifyResendCooldown).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}
