package worker

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// lockKeyPrefix namespaces worker locks alongside the auth keys documented
// in CLAUDE.md ("blacklist:<token>", "login:attempts:<email>").
const lockKeyPrefix = "worker:lock:"

// Locker guards a named job so only one replica runs it per interval.
type Locker interface {
	// Acquire attempts to take the lock for owner, expiring after ttl.
	Acquire(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	// Release drops the lock only if owner still holds it.
	Release(ctx context.Context, name, owner string) error
}

// RedisLock implements Locker with SET NX EX plus a compare-and-delete
// release, so a replica can never drop a lock another replica has since
// taken over after an expiry.
type RedisLock struct {
	client *redis.Client
}

func NewRedisLock(client *redis.Client) *RedisLock {
	return &RedisLock{client: client}
}

func (l *RedisLock) Acquire(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	return l.client.SetNX(ctx, lockKeyPrefix+name, owner, ttl).Result()
}

var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

func (l *RedisLock) Release(ctx context.Context, name, owner string) error {
	return releaseScript.Run(ctx, l.client, []string{lockKeyPrefix + name}, owner).Err()
}
