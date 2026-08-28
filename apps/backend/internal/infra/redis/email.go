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

// resetTokenTTL is how long a password-reset token stays redeemable.
// Product decision, not a deployment knob — see CLAUDE.md / the
// email-verification plan (§5, §3.1).
const resetTokenTTL = 1 * time.Hour

// resetRequestCooldown bounds how often a caller can trigger a fresh
// password-reset email for the same address. Deliberately keyed by email,
// not user id — see SetResetToken's doc comment.
const resetRequestCooldown = 15 * time.Minute

// Email wraps a Redis client with the email-verification token and resend
// helpers, mirroring Auth's shape (same package, same "hash the token
// before it becomes a key" precedent as mcp_api_keys — see
// docs/07-sheets-adapter-decisions.md §1).
type Email struct {
	client *redis.Client
	prefix string
}

// NewEmail wraps an already-connected client. prefix namespaces every key
// this type touches (config.Config.RedisKeyPrefix); see NewAuth.
func NewEmail(client *redis.Client, prefix string) *Email {
	return &Email{client: client, prefix: prefix}
}

// Each key shape is built in exactly one place so the prefix cannot be
// applied when a token is stored and forgotten when it is redeemed — which
// would silently break every verification and reset link.
func (e *Email) verifyTokenKey(tokenHash string) string {
	return e.prefix + "verify:email:" + tokenHash
}

func (e *Email) verifyResendKey(userID uuid.UUID) string {
	return e.prefix + "verify:resend:" + userID.String()
}

func (e *Email) resetTokenKey(tokenHash string) string {
	return e.prefix + "reset:password:" + tokenHash
}

func (e *Email) resetRequestKey(email string) string {
	return e.prefix + "reset:request:" + email
}

// SetVerifyToken stores tokenHash -> userID for 24h, key
// "<prefix>verify:email:<tokenHash>". tokenHash is the caller's sha256 hex digest
// of the raw token that was actually emailed — the raw token itself never
// reaches Redis.
func (e *Email) SetVerifyToken(ctx context.Context, tokenHash string, userID uuid.UUID) error {
	return e.client.Set(ctx, e.verifyTokenKey(tokenHash), userID.String(), verifyTokenTTL).Err()
}

// ConsumeVerifyToken atomically reads and deletes the key for tokenHash
// (GETDEL), so a token is redeemable exactly once with no read-then-delete
// race: two concurrent redemptions of the same token can never both
// observe found == true. found is false when the key was absent or already
// expired/consumed.
func (e *Email) ConsumeVerifyToken(ctx context.Context, tokenHash string) (userID uuid.UUID, found bool, err error) {
	val, err := e.client.GetDel(ctx, e.verifyTokenKey(tokenHash)).Result()
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
	ok, err := e.client.SetNX(ctx, e.verifyResendKey(userID), "1", verifyResendCooldown).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// SetResetToken stores tokenHash -> userID for 1h, key
// "<prefix>reset:password:<tokenHash>". tokenHash is the caller's sha256 hex digest
// of the raw token that was actually emailed — the raw token itself never
// reaches Redis, same precedent as SetVerifyToken.
func (e *Email) SetResetToken(ctx context.Context, tokenHash string, userID uuid.UUID) error {
	return e.client.Set(ctx, e.resetTokenKey(tokenHash), userID.String(), resetTokenTTL).Err()
}

// ConsumeResetToken atomically reads and deletes the key for tokenHash
// (GETDEL), so a token is redeemable exactly once with no read-then-delete
// race — same reasoning as ConsumeVerifyToken. found is false when the key
// was absent or already expired/consumed.
func (e *Email) ConsumeResetToken(ctx context.Context, tokenHash string) (userID uuid.UUID, found bool, err error) {
	val, err := e.client.GetDel(ctx, e.resetTokenKey(tokenHash)).Result()
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

// MarkResetRequested sets the 15-minute password-reset cooldown for email
// ("SET ... EX 900 NX") and reports whether it was newly set. false means
// the cooldown was already active.
//
// Keyed by email, not user id — deliberately, and unlike MarkVerifyResent.
// RequestPasswordReset runs before the caller's address is known to belong
// to a real user (that is the whole point of the endpoint being
// enumeration-safe), so there is no user id to key on yet; keying by email
// is also what makes the unknown-address path indistinguishable from the
// known one, matching the existing login:attempts:<email> convention.
func (e *Email) MarkResetRequested(ctx context.Context, email string) (bool, error) {
	ok, err := e.client.SetNX(ctx, e.resetRequestKey(email), "1", resetRequestCooldown).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}
