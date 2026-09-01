package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// adminCountKeyPrefix backs the cached-COUNT(*) key
// "<prefix>admin:count:<sha256hex(filterKey)>" — see CLAUDE.md's Redis key
// conventions and docs/11-admin-panel.md.
const adminCountKeyPrefix = "admin:count:"

// AdminCount caches short-lived COUNT(*) results behind the admin
// console's paged list endpoints (internal/module/admin). Paging through
// the same filter set reuses the cached total instead of re-running the
// count on every page load; a short TTL (admin.countCacheTTL) keeps it
// fresh enough for a 2-5 person staff console, and staleness after a
// delete is bounded and acceptable.
type AdminCount struct {
	client *redis.Client
	prefix string
}

// NewAdminCount wraps an already-connected client. prefix namespaces every
// key this type touches (config.Config.RedisKeyPrefix), matching
// NewAuth/NewRateLimiter.
func NewAdminCount(client *redis.Client, prefix string) *AdminCount {
	return &AdminCount{client: client, prefix: prefix}
}

// key is the one place this type builds a cache key, so the prefix can
// never be applied on the write path and forgotten on the read path.
// filterKey must already incorporate every filter the caller's count
// depends on — hashing it here only bounds the Redis key length, it does
// not itself provide uniqueness across filter sets.
func (a *AdminCount) key(filterKey string) string {
	sum := sha256.Sum256([]byte(filterKey))
	return a.prefix + adminCountKeyPrefix + hex.EncodeToString(sum[:])
}

// Get returns the cached count for filterKey, or ok=false on a cache miss.
func (a *AdminCount) Get(ctx context.Context, filterKey string) (count int64, ok bool, err error) {
	val, err := a.client.Get(ctx, a.key(filterKey)).Result()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		// A corrupted cache entry is treated as a miss, not an error —
		// the caller recomputes and overwrites it.
		return 0, false, nil
	}
	return n, true, nil
}

// Set caches count for filterKey for ttl.
func (a *AdminCount) Set(ctx context.Context, filterKey string, count int64, ttl time.Duration) error {
	return a.client.Set(ctx, a.key(filterKey), count, ttl).Err()
}

// UsedMemoryHuman returns Redis's own INFO memory "used_memory_human"
// value, surfaced on GET /admin/system/stats as a quick instance-health
// signal. Deliberately not namespaced by prefix — it describes the whole
// Redis instance, not this application's keyspace within it (see
// CLAUDE.md: the prefix "namespaces the keyspace, not the instance").
func (a *AdminCount) UsedMemoryHuman(ctx context.Context) (string, error) {
	info, err := a.client.Info(ctx, "memory").Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(info, "\r\n") {
		if value, found := strings.CutPrefix(line, "used_memory_human:"); found {
			return strings.TrimSpace(value), nil
		}
	}
	return "", nil
}
