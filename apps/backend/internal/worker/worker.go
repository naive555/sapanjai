// Package worker runs registered background jobs on a fixed interval,
// coordinated across replicas by a Redis lock.
//
// Lock semantics: the lock for a job is acquired with a TTL just under the
// job's interval and is deliberately NOT released after a successful run —
// it expires on its own. That makes the lock a fleet-wide debounce: with N
// replicas each ticking independently, the job still runs about once per
// interval overall rather than N times. A run that fails (returns an error
// or panics) DOES release the lock immediately, so the next tick on any
// replica can retry rather than waiting out the interval.
//
// Do not "simplify" this into a release-on-success lock. That weaker
// contract (at most one CONCURRENT run) is fine for an idempotent job like
// session cleanup, where an extra run deletes nothing, but the scheduler is
// a shared extension point: a future job that sends mail, bills a customer,
// or posts a webhook would fire once per replica per interval under those
// semantics. The stronger contract — at most one run per interval, fleet
// wide — is what jobs are allowed to assume here.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

var _ Locker = (*RedisLock)(nil)

// Worker owns a set of jobs and their schedules.
type Worker struct {
	locker     Locker
	log        *slog.Logger
	jobTimeout time.Duration
	owner      string // unique per process, for lock ownership

	jobs []Job

	mu    sync.Mutex
	stats map[string]*JobStats
}

// JobStats is the per-job execution summary exposed by Stats (and hence by
// the worker's /health endpoint).
type JobStats struct {
	Name       string     `json:"name"`
	Runs       int64      `json:"runs"`
	Failures   int64      `json:"failures"`
	Skipped    int64      `json:"skipped"` // lock held by another replica
	LastRunAt  *time.Time `json:"lastRunAt"`
	LastDurMs  int64      `json:"lastDurationMs"`
	LastError  string     `json:"lastError,omitempty"`
	LastAffect int64      `json:"lastAffected"`
}

func New(locker Locker, log *slog.Logger, jobTimeout time.Duration) *Worker {
	return &Worker{
		locker:     locker,
		log:        log,
		jobTimeout: jobTimeout,
		owner:      uuid.NewString(),
		stats:      make(map[string]*JobStats),
	}
}

// Register adds a job. Call before Run. Panics on a duplicate name, since
// that is a wiring bug that would silently make two jobs share a lock.
func (w *Worker) Register(j Job) {
	for _, existing := range w.jobs {
		if existing.Name() == j.Name() {
			panic("worker: duplicate job name " + j.Name())
		}
	}
	w.jobs = append(w.jobs, j)
	w.stats[j.Name()] = &JobStats{Name: j.Name()}
}

// Run starts one goroutine per job and blocks until ctx is cancelled and
// every in-flight run has finished.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, j := range w.jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			w.loop(ctx, j)
		}(j)
	}
	w.log.Info("worker started", "jobs", len(w.jobs), "owner", w.owner)
	wg.Wait()
	w.log.Info("worker stopped")
}

// loop runs a job once at startup and then every Interval until ctx ends.
func (w *Worker) loop(ctx context.Context, j Job) {
	ticker := time.NewTicker(j.Interval())
	defer ticker.Stop()

	w.tryRun(ctx, j)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tryRun(ctx, j)
		}
	}
}

// tryRun takes the lock and executes one run, recording stats either way.
func (w *Worker) tryRun(ctx context.Context, j Job) {
	if ctx.Err() != nil {
		return
	}

	name := j.Name()
	acquired, err := w.locker.Acquire(ctx, name, w.owner, lockTTL(j.Interval()))
	if err != nil {
		w.log.Error("job lock failed", "job", name, "error", err)
		w.record(name, func(s *JobStats) { s.Failures++; s.LastError = err.Error() })
		return
	}
	if !acquired {
		w.log.Debug("job skipped, lock held elsewhere", "job", name)
		w.record(name, func(s *JobStats) { s.Skipped++ })
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, w.jobTimeout)
	defer cancel()

	started := time.Now()
	res, runErr := safeRun(runCtx, j)
	elapsed := time.Since(started)

	if runErr != nil {
		// Free the lock so the next tick can retry instead of waiting out
		// the whole interval.
		if relErr := w.locker.Release(context.WithoutCancel(ctx), name, w.owner); relErr != nil {
			w.log.Error("job lock release failed", "job", name, "error", relErr)
		}
		w.log.Error("job failed", "job", name, "duration", elapsed.String(), "error", runErr)
		w.record(name, func(s *JobStats) {
			s.Runs++
			s.Failures++
			s.LastError = runErr.Error()
			s.LastDurMs = elapsed.Milliseconds()
			now := started
			s.LastRunAt = &now
		})
		return
	}

	attrs := append([]any{"job", name, "affected", res.Affected, "duration", elapsed.String()}, res.Attrs...)
	w.log.Info("job completed", attrs...)
	w.record(name, func(s *JobStats) {
		s.Runs++
		s.LastError = ""
		s.LastAffect = res.Affected
		s.LastDurMs = elapsed.Milliseconds()
		now := started
		s.LastRunAt = &now
	})
}

// safeRun converts a panic in job code into an error so one bad job cannot
// take the worker process down.
func safeRun(ctx context.Context, j Job) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return j.Run(ctx)
}

// lockTTL holds the lock for slightly less than a full interval, so a
// single-replica deployment is never skipped by its own not-yet-expired
// lock on the following tick.
func lockTTL(interval time.Duration) time.Duration {
	ttl := interval - interval/10
	if ttl <= 0 {
		return interval
	}
	return ttl
}

func (w *Worker) record(name string, mutate func(*JobStats)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.stats[name]; ok {
		mutate(s)
	}
}

// Stats returns a copy of every job's execution summary.
func (w *Worker) Stats() []JobStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]JobStats, 0, len(w.stats))
	for _, j := range w.jobs {
		if s, ok := w.stats[j.Name()]; ok {
			out = append(out, *s)
		}
	}
	return out
}
