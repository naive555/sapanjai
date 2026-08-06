// Package database owns the Postgres connection pool and the sqlc-generated
// Store built on top of it.
package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// New creates a pgx connection pool for the given URL and verifies
// connectivity with a ping, mirroring the `SELECT 1` boot check in the
// source app's src/index.ts.
func New(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// Store bundles the pgx pool with the sqlc-generated Queries and provides a
// transaction helper. Handlers/services depend on *Store (or the db.Querier
// interface, for mocking in unit tests) rather than the raw pool.
type Store struct {
	Pool *pgxpool.Pool
	*db.Queries
}

// NewStore wraps an already-connected pool with sqlc queries.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool, Queries: db.New(pool)}
}

// WithTx runs fn inside a transaction, passing a *db.Queries bound to the
// transaction. Commits on nil error, otherwise rolls back. Used for
// multi-step writes such as refresh-token rotation (Phase 2) and org create +
// owner membership (Phase 3).
func (s *Store) WithTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	return nil
}
