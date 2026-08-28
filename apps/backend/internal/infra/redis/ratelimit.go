package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// rateLimitKeyPrefix backs the bucket key "<prefix>mcp:ratelimit:<connectorId>" —
// see CLAUDE.md's Redis key conventions.
const rateLimitKeyPrefix = "mcp:ratelimit:"

// bucketIdleTTL bounds how long an unused bucket's Redis hash survives.
// The window a bucket needs to remember is always capacity/refillRate =
// 60s (refillRate is defined as capacity/60 below), so 2x that is ample
// headroom against clock jitter while still reclaiming keys for connectors
// that go quiet — nothing here depends on the configured capacity.
const bucketIdleTTL = 2 * time.Minute

// tokenBucketScript implements an atomic token-bucket check-and-take:
// refill by elapsed time since the bucket's own last-touched timestamp (up
// to capacity), then take n tokens if enough are available. Run as a single
// EVAL/EVALSHA so concurrent replicas hitting the same connector never
// interleave a read with another's write — the alternative, a GET-then-SET
// from Go, would let two replicas both read "5 tokens available" and both
// admit a request that should have been serialized.
//
// The script reads its own clock via Redis's TIME command rather than
// trusting a client-supplied timestamp: every caller (any API replica)
// agrees on one clock this way, so refill can't be skewed by drift between
// hosts or, worse, gamed by a caller passing a favorable time.
//
// KEYS[1] = bucket key
// ARGV[1] = capacity (max tokens, float)
// ARGV[2] = refill rate (tokens/second, float)
// ARGV[3] = requested (tokens this call wants to take, integer >= 1)
// ARGV[4] = idle TTL in seconds, for the key itself
//
// Returns {allowed (0/1), retryAfterSeconds (integer, ceil'd, 0 when allowed)}.
var tokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local requested = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local time_parts = redis.call('TIME')
local now = tonumber(time_parts[1]) + (tonumber(time_parts[2]) / 1000000)

local tokens = capacity
local last = now

local bucket = redis.call('HMGET', key, 'tokens', 'ts')
if bucket[1] and bucket[2] then
  tokens = tonumber(bucket[1])
  last = tonumber(bucket[2])
end

local elapsed = now - last
if elapsed < 0 then
  elapsed = 0
end
tokens = math.min(capacity, tokens + elapsed * refill_rate)

local allowed = 0
local retry_after = 0

if tokens >= requested then
  tokens = tokens - requested
  allowed = 1
else
  local deficit = requested - tokens
  if refill_rate > 0 then
    retry_after = math.ceil(deficit / refill_rate)
  else
    -- A zero refill rate never recovers on its own; report a bucket-sized
    -- wait rather than a nonsensical infinite one.
    retry_after = ttl
  end
end

redis.call('HSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('EXPIRE', key, ttl)

return {allowed, retry_after}
`)

// RateLimiter enforces a per-connector token bucket, one bucket per
// upstream connector ("<prefix>mcp:ratelimit:<connectorId>"), sized in whole
// tokens-per-minute and refilled continuously (capacity/60 tokens/second).
//
// It counts upstream Google API requests, not MCP tool calls (see
// internal/module/mcp/service.go's dispatch-time check and
// docs/07-sheets-adapter-decisions.md step 4) — Take's n parameter is how a
// caller charges more than the floor-of-1 per tools/call: step 7's paged
// scan will call Take once per page fetched, mid-scan, charging n=1 each
// time rather than the whole scan's page count up front.
type RateLimiter struct {
	client          *redis.Client
	prefix          string
	capacity        float64
	refillPerSecond float64
}

// NewRateLimiter builds a RateLimiter with a bucket capacity of
// perMinute tokens, refilling to full over 60 seconds. perMinute <= 0 falls
// back to 60 (config.Config.MCPRateLimitPerMin's own default) rather than
// building a bucket that can never admit anything — defends callers that
// construct a Config literal directly (every integration test's
// setupTestServer does) without routing through config.Load's validation.
//
// prefix namespaces the bucket keys (config.Config.RedisKeyPrefix); see
// NewAuth.
func NewRateLimiter(client *redis.Client, perMinute int, prefix string) *RateLimiter {
	if perMinute <= 0 {
		perMinute = 60
	}
	capacity := float64(perMinute)
	return &RateLimiter{
		client:          client,
		prefix:          prefix,
		capacity:        capacity,
		refillPerSecond: capacity / 60,
	}
}

// Take attempts to charge n tokens (minimum 1) against connectorID's
// bucket. allowed reports whether the request is admitted; when it is not,
// retryAfter is the whole-second wait (rounded up) before enough tokens
// would be available, suitable for a caller to state directly in a
// RATE_LIMITED message. err is a genuine infra failure (Redis unreachable,
// malformed script result) — distinct from allowed == false, which is the
// ordinary "bucket empty" outcome and never an error.
func (r *RateLimiter) Take(ctx context.Context, connectorID string, n int) (allowed bool, retryAfter time.Duration, err error) {
	if n < 1 {
		n = 1
	}

	key := r.prefix + rateLimitKeyPrefix + connectorID
	res, err := tokenBucketScript.Run(ctx, r.client, []string{key},
		r.capacity, r.refillPerSecond, n, int(bucketIdleTTL.Seconds()),
	).Result()
	if err != nil {
		return false, 0, fmt.Errorf("rate limit take: %w", err)
	}

	vals, ok := res.([]interface{})
	if !ok || len(vals) != 2 {
		return false, 0, fmt.Errorf("rate limit take: unexpected script result %#v", res)
	}
	allowedN, ok1 := vals[0].(int64)
	retrySeconds, ok2 := vals[1].(int64)
	if !ok1 || !ok2 {
		return false, 0, fmt.Errorf("rate limit take: unexpected script result types %#v", vals)
	}
	if retrySeconds < 0 {
		retrySeconds = 0
	}

	return allowedN == 1, time.Duration(retrySeconds) * time.Second, nil
}
