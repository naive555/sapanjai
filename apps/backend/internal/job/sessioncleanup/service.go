// Package sessioncleanup removes session rows that can no longer be used:
// expired ones, and revoked ones past the forensics retention window.
//
// Revoked-but-unexpired rows are kept for RetentionWindow so that
// refresh-token reuse detection (auth service, Phase 2) still recognises a
// replayed token as a family reuse rather than an unknown session.
package sessioncleanup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/junctera/backend/internal/infra/database"
	"github.com/junctera/backend/internal/infra/database/db"
	"github.com/junctera/backend/internal/worker"
)

var (
	_ cleanupStore = (*database.Store)(nil)
	_ worker.Job   = (*Job)(nil)
)

// maxBatches caps one run so a pathologically large backlog cannot hold the
// job lock indefinitely; the remainder is drained on the next interval.
const maxBatches = 100

// cleanupStore is the subset of *database.Store this job needs, narrowed so
// unit tests can hand-mock it (same pattern as authStore in module/auth).
type cleanupStore interface {
	DeleteExpiredSessions(ctx context.Context, arg db.DeleteExpiredSessionsParams) (int64, error)
}

// Job deletes stale sessions in batches.
type Job struct {
	store     cleanupStore
	log       *slog.Logger
	interval  time.Duration
	retention time.Duration
	batchSize int32
}

func New(store cleanupStore, log *slog.Logger, interval, retention time.Duration, batchSize int) *Job {
	return &Job{
		store:     store,
		log:       log,
		interval:  interval,
		retention: retention,
		batchSize: int32(batchSize),
	}
}

func (j *Job) Name() string            { return "session-cleanup" }
func (j *Job) Interval() time.Duration { return j.interval }

// Run deletes in LIMIT-sized batches until a batch comes back short (the
// table is drained), maxBatches is hit, or ctx ends. Each batch is its own
// statement, so an aborted run still keeps the rows it already deleted.
func (j *Job) Run(ctx context.Context) (worker.Result, error) {
	params := db.DeleteExpiredSessionsParams{
		RetentionSeconds: int32(j.retention.Seconds()),
		BatchSize:        j.batchSize,
	}

	var total int64
	var batches int

	for batches < maxBatches {
		if err := ctx.Err(); err != nil {
			return worker.Result{Affected: total, Attrs: []any{"batches", batches, "drained", false}}, err
		}

		deleted, err := j.store.DeleteExpiredSessions(ctx, params)
		if err != nil {
			return worker.Result{Affected: total}, fmt.Errorf("delete expired sessions: %w", err)
		}

		total += deleted
		batches++

		if deleted < int64(j.batchSize) {
			return worker.Result{
				Affected: total,
				Attrs:    []any{"batches", batches, "drained", true},
			}, nil
		}
	}

	j.log.Warn("session cleanup hit batch cap, remainder deferred to next run",
		"deleted", total, "batches", batches)
	return worker.Result{Affected: total, Attrs: []any{"batches", batches, "drained", false}}, nil
}
