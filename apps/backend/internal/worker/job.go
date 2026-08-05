package worker

import (
	"context"
	"time"
)

// Job is the contract every background job implements. Registering a new
// job with a Worker is the only wiring a future job needs — the scheduler
// handles timing, distributed locking, timeouts, panic recovery, and
// logging uniformly.
type Job interface {
	// Name identifies the job in logs, stats, and its Redis lock key. It
	// must be stable across deploys and unique within the worker.
	Name() string

	// Interval is how often the job should run. The scheduler also runs the
	// job once at startup (subject to the lock), so a fresh fleet does not
	// idle for a full interval before the first run.
	Interval() time.Duration

	// Run performs one execution. It must respect ctx cancellation (the
	// scheduler cancels on per-run timeout and on shutdown) and should be
	// safe to abort part-way — work is expected to be committed
	// incrementally rather than in one all-or-nothing transaction.
	Run(ctx context.Context) (Result, error)
}

// Result summarises one job run for logging and the worker's health
// payload. Attrs are extra key/value pairs (slog style) merged into the
// completion log line.
type Result struct {
	Affected int64
	Attrs    []any
}
