package redis

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const loginAttemptsTTL = 15 * time.Minute

// reauthAttemptsTTL matches loginAttemptsTTL — the admin password re-auth
// limiter (docs/11-admin-panel.md D4, execution plan Task 3.2) is a
// straight port of the login limiter's shape, keyed by userId instead of
// email since the caller is already an authenticated admin.
const reauthAttemptsTTL = 15 * time.Minute

// Auth wraps a Redis client with the access-token blacklist and login
// rate-limit helpers, mirroring RedisAuth in the source app
// (src/infrastructure/redis/index.ts): same key names and TTLs.
type Auth struct {
	client *redis.Client
	prefix string
}

// NewAuth wraps an already-connected client. prefix namespaces every key
// this type touches (config.Config.RedisKeyPrefix) so a Redis instance
// shared with another application cannot collide with them; "" restores the
// unprefixed keys.
func NewAuth(client *redis.Client, prefix string) *Auth {
	return &Auth{client: client, prefix: prefix}
}

// blacklistKey, loginAttemptsKey, and banKey are the single place each key
// shape is built, so the prefix can never be applied on the write path and
// forgotten on the read path.
func (a *Auth) blacklistKey(token string) string { return a.prefix + "blacklist:" + token }

func (a *Auth) loginAttemptsKey(email string) string { return a.prefix + "login:attempts:" + email }

func (a *Auth) banKey(userID uuid.UUID) string { return a.prefix + "banned:" + userID.String() }

func (a *Auth) reauthAttemptsKey(userID uuid.UUID) string {
	return a.prefix + "admin:reauth:attempts:" + userID.String()
}

// BlacklistToken marks an access token as revoked for ttl, key
// "<prefix>blacklist:<token>".
func (a *Auth) BlacklistToken(ctx context.Context, token string, ttl time.Duration) error {
	return a.client.Set(ctx, a.blacklistKey(token), "1", ttl).Err()
}

// IsBlacklisted reports whether the given access token has been blacklisted.
func (a *Auth) IsBlacklisted(ctx context.Context, token string) (bool, error) {
	n, err := a.client.Exists(ctx, a.blacklistKey(token)).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// IncrementLoginAttempts increments the failed-login counter for email, key
// "<prefix>login:attempts:<email>". The key expires 15 minutes after the first
// increment (mirrors the source's reset-every-15-minutes window).
func (a *Auth) IncrementLoginAttempts(ctx context.Context, email string) (int64, error) {
	key := a.loginAttemptsKey(email)
	attempts, err := a.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if attempts == 1 {
		if err := a.client.Expire(ctx, key, loginAttemptsTTL).Err(); err != nil {
			return 0, err
		}
	}
	return attempts, nil
}

// ResetLoginAttempts clears the failed-login counter for email.
func (a *Auth) ResetLoginAttempts(ctx context.Context, email string) error {
	return a.client.Del(ctx, a.loginAttemptsKey(email)).Err()
}

// Ban marks a user as banned, key "<prefix>banned:<userId>", with no TTL:
// this is a fast-path cache in front of the durable users.banned_at column
// (docs/11-admin-panel.md §4/D3), and an entry that silently expired would
// let a Redis-only ban lapse behind the DB's back. Unban is the only thing
// that clears it.
func (a *Auth) Ban(ctx context.Context, userID uuid.UUID) error {
	return a.client.Set(ctx, a.banKey(userID), "1", 0).Err()
}

// Unban clears the ban cache entry for a user.
func (a *Auth) Unban(ctx context.Context, userID uuid.UUID) error {
	return a.client.Del(ctx, a.banKey(userID)).Err()
}

// IsBanned reports whether the given user is cached as banned.
func (a *Auth) IsBanned(ctx context.Context, userID uuid.UUID) (bool, error) {
	n, err := a.client.Exists(ctx, a.banKey(userID)).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// GetLoginAttempts returns the current failed-login count for email, or 0 if
// the key is absent or unparseable.
func (a *Auth) GetLoginAttempts(ctx context.Context, email string) (int, error) {
	val, err := a.client.Get(ctx, a.loginAttemptsKey(email)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	attempts, err := strconv.Atoi(val)
	if err != nil {
		return 0, nil
	}
	return attempts, nil
}

// IncrementReauthAttempts increments the failed-reauth counter for userID,
// key "<prefix>admin:reauth:attempts:<userId>". Mirrors
// IncrementLoginAttempts exactly (execution plan Task 3.2): without an
// independent limiter here, the password field on every destructive admin
// endpoint would be an online password oracle against the highest-value
// accounts in the system.
func (a *Auth) IncrementReauthAttempts(ctx context.Context, userID uuid.UUID) (int64, error) {
	key := a.reauthAttemptsKey(userID)
	attempts, err := a.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if attempts == 1 {
		if err := a.client.Expire(ctx, key, reauthAttemptsTTL).Err(); err != nil {
			return 0, err
		}
	}
	return attempts, nil
}

// ResetReauthAttempts clears the failed-reauth counter for userID, called
// after a successful re-authentication.
func (a *Auth) ResetReauthAttempts(ctx context.Context, userID uuid.UUID) error {
	return a.client.Del(ctx, a.reauthAttemptsKey(userID)).Err()
}

// GetReauthAttempts returns the current failed-reauth count for userID, or 0
// if the key is absent or unparseable — mirrors GetLoginAttempts.
func (a *Auth) GetReauthAttempts(ctx context.Context, userID uuid.UUID) (int, error) {
	val, err := a.client.Get(ctx, a.reauthAttemptsKey(userID)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	attempts, err := strconv.Atoi(val)
	if err != nil {
		return 0, nil
	}
	return attempts, nil
}
