package admin

import (
	"context"
	"time"
)

// countCacheTTL bounds how stale a cached COUNT(*) can be behind an admin
// list endpoint or the system-stats page. Paging through the same filter
// set reuses the cached total instead of re-counting on every page; a
// short TTL keeps it fresh enough for a 2-5 person staff console, and
// staleness after a delete is bounded and acceptable (execution plan
// Task 2.2, ported from ../agritech/apps/api/src/modules/superadmin/count-cache.ts).
const countCacheTTL = 30 * time.Second

// countCache is the subset of *redis.AdminCount this service depends on,
// narrowed so unit tests can hand-mock it without a real Redis client.
type countCache interface {
	Get(ctx context.Context, filterKey string) (int64, bool, error)
	Set(ctx context.Context, filterKey string, count int64, ttl time.Duration) error
	UsedMemoryHuman(ctx context.Context) (string, error)
}

// cachedCount returns the cached count for filterKey if present, otherwise
// computes it via compute and caches the result for countCacheTTL.
// filterKey MUST incorporate every filter compute's result depends on — a
// key that ignores one serves the wrong total for that filter combination,
// and the bug then looks like a pagination bug, not a cache bug (see
// internal/infra/redis.AdminCount's doc comment). It must NOT incorporate
// limit/offset: the total does not depend on the page being viewed, and
// folding pagination into the key would defeat the whole point of caching
// it — every page of the same filtered search would miss independently.
//
// A cache read/write failure falls back to compute directly rather than
// failing the request: an admin list is allowed to be slow when Redis is
// unavailable, never broken.
func (s *Service) cachedCount(ctx context.Context, filterKey string, compute func() (int64, error)) (int64, error) {
	if n, ok, err := s.cache.Get(ctx, filterKey); err == nil && ok {
		return n, nil
	}

	count, err := compute()
	if err != nil {
		return 0, err
	}

	_ = s.cache.Set(ctx, filterKey, count, countCacheTTL)
	return count, nil
}
