package redis

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const loginAttemptsTTL = 15 * time.Minute

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

// blacklistKey and loginAttemptsKey are the single place each key shape is
// built, so the prefix can never be applied on the write path and forgotten
// on the read path.
func (a *Auth) blacklistKey(token string) string { return a.prefix + "blacklist:" + token }

func (a *Auth) loginAttemptsKey(email string) string { return a.prefix + "login:attempts:" + email }

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
