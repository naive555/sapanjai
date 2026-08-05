// Package redis owns the Redis client. Phase 0 only opens and pings the
// client — the RedisAuth-equivalent helpers (blacklist, login attempts)
// land in Phase 2.
package redis

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// New parses the given Redis URL, constructs a client, and verifies
// connectivity with a PING, mirroring the redis.connect() boot check in
// the source app's src/index.ts.
func New(ctx context.Context, redisURL string) (*redis.Client, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	client := redis.NewClient(opts)

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return client, nil
}
