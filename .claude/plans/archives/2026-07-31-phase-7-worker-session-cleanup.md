# Phase 7 — Background Worker (session cleanup + pluggable job runner) — Implementation Plan

> **Status: ✅ All 9 steps complete (2026-07-31).** All Definition-of-done
> items verified live, not just read through: `go build`/`go vet`/`go test ./...`
> pass against real Postgres + Redis; `make worker` runs the session-cleanup
> job on schedule; `docker compose up -d --build` brought up a **healthy**
> `controlplane-worker`, whose `/health` returned the exact
> `{status, uptime, jobs[]}` shape; a real `docker stop` proved the
> SIGTERM → graceful-shutdown sequence works in the actual deployment target.
> One environment gap: `-race` isn't runnable locally (no cgo/gcc on this
> Windows host) — run it once in CI, which has a C toolchain, to confirm.
> One real bug was found and fixed mid-implementation: an integration-test
> fixture helper inserted raw `timestamp` rows using local-timezone
> `time.Time` values, silently storing the wrong wall-clock digits (the same
> pgx quirk `auth/service.go` already works around via `.UTC()`) — fixed in
> the test helper; the production job was never affected, since `Job.Run`
> never touches Go-side timestamps at all.
>
> Target executor: **Sonnet**. This plan is prescriptive: exact file paths,
> copy-paste-ready snippets, and verification commands. Read
> `docs/03-target-architecture.md` (layout + "Backend architecture notes")
> and `CLAUDE.md` before starting.
>
> No HTTP API behavior changes in this phase. `docs/02-api-contract.md` is
> **frozen** — the worker's health endpoint is an internal ops endpoint on a
> different port and process, not part of the public contract. Do not edit
> `docs/02-api-contract.md`.

## Scope

Add an **asynchronous background job runner** as a fifth binary in the Go
backend, with **session cleanup** as its first job and a `Job` interface so
future jobs plug in with one `Register(...)` line.

1. `internal/worker/` — `Job` interface, registry, interval scheduler, Redis
   distributed lock, per-run timeout, panic recovery, per-job stats.
2. `internal/job/sessioncleanup/` — first job: batched delete of expired and
   long-revoked sessions.
3. `cmd/worker/main.go` — bootstrap mirroring `cmd/api/main.go` (config →
   redis → db → run → graceful shutdown) plus a small `/health` server.
4. Migration `00006` — indexes supporting the cleanup predicate.
5. Wiring: `Makefile`, `Dockerfile`, `compose.yaml`, `k8s/worker/`, env files.
6. Tests: unit (worker + job), integration (lock vs. real Redis, cleanup vs.
   real Postgres).
7. Docs: `docs/03`, `docs/04`, `README.md`, `CLAUDE.md`.

### Non-goals (do not build these)

- **No job queue / on-demand dispatch.** Periodic jobs only. No `Enqueue`
  API, no retries-with-backoff, no dead-letter queue. If on-demand jobs are
  wanted later, that is a separate decision (asynq was considered and
  deferred).
- **No cron expressions.** `Interval() time.Duration`, not `"0 */6 * * *"`.
- **No new third-party dependencies.** Everything here is stdlib +
  already-vendored `go-redis`, `pgx`, `uuid`, `godotenv`.
- **No audit-log rows for job runs.** `audit_logs` is org-scoped and
  actor-scoped; a fleet-wide cleanup has neither. Job outcomes go to slog and
  the worker's `/health` payload.
- No changes to any handler, service, DTO, or route in `internal/module/*`
  other than the new sqlc query method used by the job.

## Decisions locked (confirmed with the owner, 2026-07-31)

1. **Placement**: inside `apps/backend` as `cmd/worker` + `internal/worker` +
   `internal/job` — one Go module, reusing `config`, `database.Store`,
   `redis`, `logger` with zero duplication. Matches the existing
   `cmd/{api,migrate,seed,healthcheck}` multi-binary pattern. Deployed as its
   own process/container/pod. **Not** a separate `apps/worker/` Go module,
   **not** a goroutine inside the API process.
2. **Engine**: homegrown scheduler + `Job` interface. No dependency.
3. **Cleanup rule**: delete rows where `expires_at < now()` **or**
   (`is_revoked = true` **and** `created_at` older than a retention window,
   default 30d). Batched with `LIMIT`, loop until drained. The retention
   window exists so token-family reuse forensics survive a while after
   revocation.
4. **Index**: add `00006_session_cleanup_indexes.sql`. This is an agreed
   deviation from the "schema byte-identical to the source migrations" rule in
   `CLAUDE.md` — record it in `docs/03` under a new Phase 7 deviations
   section.

## Ground truth captured from the codebase

- Go module `github.com/controlplane/backend`, Go 1.26, sqlc v1.31.1.
- `database.Store` = `*pgxpool.Pool` + embedded `*db.Queries` + `WithTx`.
- `redis.New(ctx, url) (*redis.Client, error)`; `redis.Auth` in
  `internal/infra/redis/auth.go` is the house style for Redis helpers (narrow
  struct wrapping `*redis.Client`, key prefix as a string literal, TTL as a
  package const).
- `config.Load()` aggregates **all** validation problems into one error;
  `cmd/migrate` reuses it wholesale even though it only needs `DATABASE_URL`.
  The worker does the same — no separate loader.
- `applogger.New(appEnv, logLevel)` → `*slog.Logger`.
- `loadDotEnv()` is duplicated verbatim in `cmd/api` and `cmd/migrate`
  (`{"../../.env", "../.env", ".env"}`). Duplicate it once more in
  `cmd/worker` — do **not** refactor it into a shared package in this phase.
- Existing migrations are `00001…00005`, 5-digit prefix, embedded via
  `migrations/embed.go`.
- Services depend on **narrow hand-written interfaces** (see
  `authStore`/`loginLimiter` in `internal/module/auth/service.go`) with
  `var _ authStore = (*database.Store)(nil)` compile-time assertions. Follow
  that pattern exactly for the job and the worker.
- Integration tests live beside the code, skip when `DATABASE_URL`/`REDIS_URL`
  are unset (`t.Skip`), and run `goose.UpContext` against the real DB.
- CI (`.github/workflows/ci.yml`) already runs `go vet ./...`,
  `go build ./...`, `go test ./...` and a `docker build` of
  `apps/backend` — **no CI changes are needed**; new packages and the new
  binary are covered automatically. Do not edit `ci.yml` or `release.yml`.

---

## Step 1 — Migration `00006` (indexes)

**New file** `apps/backend/migrations/00006_session_cleanup_indexes.sql`:

```sql
-- +goose Up
CREATE INDEX IF NOT EXISTS "idx_sessions_expires_at" ON "sessions" ("expires_at");
CREATE INDEX IF NOT EXISTS "idx_sessions_revoked_created_at" ON "sessions" ("created_at") WHERE "is_revoked" = true;

-- +goose Down
DROP INDEX IF EXISTS "idx_sessions_revoked_created_at";
DROP INDEX IF EXISTS "idx_sessions_expires_at";
```

Notes:
- Plain (non-concurrent) `CREATE INDEX` — goose wraps migrations in a
  transaction by default and `CONCURRENTLY` cannot run inside one. The
  `sessions` table is small at this stage; if that changes, a future
  migration can use `-- +goose NO TRANSACTION` with `CONCURRENTLY`.
- Quoted identifiers to match the style of `00001…00005`.
- No `embed.go` change needed (`//go:embed *.sql` picks it up).

**Verify**: `cd apps/backend && go run ./cmd/migrate up && go run ./cmd/migrate status`

---

## Step 2 — sqlc query

**Append** to `apps/backend/internal/infra/database/queries/sessions.sql`:

```sql
-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
WHERE id IN (
    SELECT id FROM sessions
    WHERE expires_at < now()
       OR (is_revoked = true AND created_at < now() - (sqlc.arg(retention_seconds)::int * INTERVAL '1 second'))
    LIMIT sqlc.arg(batch_size)
);
```

Then `make sqlc`.

**If the `sqlc` CLI is not installed** (`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest` may be unavailable offline), hand-write the generated code in the exact house style instead. Append to
`apps/backend/internal/infra/database/db/sessions.sql.go`:

```go
const deleteExpiredSessions = `-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
WHERE id IN (
    SELECT id FROM sessions
    WHERE expires_at < now()
       OR (is_revoked = true AND created_at < now() - ($1::int * INTERVAL '1 second'))
    LIMIT $2
)
`

type DeleteExpiredSessionsParams struct {
	RetentionSeconds int32 `json:"retention_seconds"`
	BatchSize        int32 `json:"batch_size"`
}

func (q *Queries) DeleteExpiredSessions(ctx context.Context, arg DeleteExpiredSessionsParams) (int64, error) {
	result, err := q.db.Exec(ctx, deleteExpiredSessions, arg.RetentionSeconds, arg.BatchSize)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}
```

and add to the `Querier` interface in `db/querier.go`, keeping alphabetical
order (between `CreateUser` and `DeleteMembership`):

```go
	DeleteExpiredSessions(ctx context.Context, arg DeleteExpiredSessionsParams) (int64, error)
```

State clearly in the final report which path was taken (generated vs.
hand-written). If hand-written, note that the next `make sqlc` run must
reproduce it identically.

---

## Step 3 — Config

**Edit** `apps/backend/internal/config/config.go`.

Add to the `Config` struct, after the JWT block:

```go
	WorkerPort       string
	WorkerJobTimeout time.Duration

	SessionCleanupInterval  time.Duration
	SessionCleanupRetention time.Duration
	SessionCleanupBatchSize int
```

In `Load()`, after the existing defaults block:

```go
	cfg.WorkerPort = getEnv("WORKER_PORT", "3001")
```

and after the `refreshExpSeconds` block, before the `problems` check:

```go
	for _, d := range []struct {
		key      string
		fallback string
		target   *time.Duration
	}{
		{"WORKER_JOB_TIMEOUT", "5m", &cfg.WorkerJobTimeout},
		{"SESSION_CLEANUP_INTERVAL", "1h", &cfg.SessionCleanupInterval},
		{"SESSION_CLEANUP_RETENTION", "720h", &cfg.SessionCleanupRetention},
	} {
		parsed, err := time.ParseDuration(getEnv(d.key, d.fallback))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s is not a valid duration: %v", d.key, err))
		case parsed <= 0:
			problems = append(problems, d.key+" must be greater than zero")
		default:
			*d.target = parsed
		}
	}

	batchSize, err := strconv.Atoi(getEnv("SESSION_CLEANUP_BATCH_SIZE", "1000"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("SESSION_CLEANUP_BATCH_SIZE is not a valid integer: %v", err))
	case batchSize < 1 || batchSize > maxCleanupBatchSize:
		problems = append(problems, fmt.Sprintf("SESSION_CLEANUP_BATCH_SIZE must be between 1 and %d", maxCleanupBatchSize))
	default:
		cfg.SessionCleanupBatchSize = batchSize
	}
```

Add near `minSecretLen`:

```go
const maxCleanupBatchSize = 10_000
```

Careful: `err` is already declared above in `Load()` — the `batchSize, err :=`
line reuses it via a new variable on the left, which is legal since
`batchSize` is new. Keep `go vet` clean.

These vars are validated for **every** binary (api, migrate, seed, worker)
because they all call `config.Load()`. That is intentional and consistent
with how `JWT_*` is already validated by `cmd/migrate`. Since all five have
defaults, nothing breaks for existing deployments.

**Also update** `.env.example` and `.env.docker` — append after the JWT block:

```
# Background worker (cmd/worker)
WORKER_PORT=3001
WORKER_JOB_TIMEOUT=5m
SESSION_CLEANUP_INTERVAL=1h
SESSION_CLEANUP_RETENTION=720h
SESSION_CLEANUP_BATCH_SIZE=1000
```

`.env.docker` is git-ignored but present locally — update it if it exists;
if it does not, skip it and say so.

---

## Step 4 — `internal/worker/` (the runner)

Four files. Package doc goes on `worker.go`.

### 4a. `job.go`

```go
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
```

### 4b. `lock.go`

```go
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
```

Add the house-style compile-time assertion in `worker.go`:
`var _ Locker = (*RedisLock)(nil)`.

### 4c. `worker.go`

Package doc must explain the **lock-as-debounce** semantics, because it is
the one non-obvious design decision here:

```go
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
```

```go
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
```

Note `context.WithoutCancel` (Go 1.21+) on the release path so a lock is
still released when the shutdown signal is what killed the run.

### 4d. `health.go` (worker's ops endpoint)

Plain `net/http`, not Echo — the worker has no routing, middleware, or
error-handler needs, and pulling Echo in would imply an API surface that
does not exist.

```go
package worker

import (
	"encoding/json"
	"net/http"
	"time"
)

// HealthHandler serves GET /health for the worker process: the same
// {status, uptime} shape as the API's health module, plus per-job stats.
// It is an internal ops endpoint (WORKER_PORT), not part of the public API
// contract in docs/02-api-contract.md.
func HealthHandler(w *Worker, startedAt time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"status": "ok",
			"uptime": time.Since(startedAt).Seconds(),
			"jobs":   w.Stats(),
		})
	})
	return mux
}
```

---

## Step 5 — `internal/job/sessioncleanup/service.go`

```go
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

	"github.com/controlplane/backend/internal/infra/database"
	"github.com/controlplane/backend/internal/infra/database/db"
	"github.com/controlplane/backend/internal/worker"
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
```

Note the `retention.Seconds()` → `int32` conversion: 30d = 2,592,000, well
inside int32. `SESSION_CLEANUP_RETENTION` is validated `> 0` in config;
absurdly large values (>68 years) would overflow — not worth guarding, but
do not lower the validation to allow negatives.

---

## Step 6 — `cmd/worker/main.go`

Mirror `cmd/api/main.go` closely, including the `loadDotEnv` duplication.

```go
// Command worker runs background jobs: load config, connect to Redis and
// Postgres, start the job scheduler plus an internal /health server, and
// shut down gracefully on SIGINT/SIGTERM — mirroring cmd/api's bootstrap.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/controlplane/backend/internal/config"
	"github.com/controlplane/backend/internal/infra/database"
	"github.com/controlplane/backend/internal/infra/redis"
	"github.com/controlplane/backend/internal/job/sessioncleanup"
	applogger "github.com/controlplane/backend/internal/shared/logger"
	"github.com/controlplane/backend/internal/worker"
)

func main() {
	startedAt := time.Now()
	loadDotEnv()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	log := applogger.New(cfg.AppEnv, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rdb, err := redis.New(ctx, cfg.RedisURL)
	if err != nil {
		log.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	log.Info("Redis connected")

	pool, err := database.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	log.Info("Database connected")

	store := database.NewStore(pool)

	w := worker.New(worker.NewRedisLock(rdb), log, cfg.WorkerJobTimeout)
	// Register future jobs here — one line each.
	w.Register(sessioncleanup.New(
		store, log,
		cfg.SessionCleanupInterval,
		cfg.SessionCleanupRetention,
		cfg.SessionCleanupBatchSize,
	))

	health := &http.Server{
		Addr:              ":" + cfg.WorkerPort,
		Handler:           worker.HealthHandler(w, startedAt),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("worker health server listening", "addr", health.Addr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("worker health server error", "error", err)
		}
	}()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	<-ctx.Done()
	log.Info("shutdown signal received, shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := health.Shutdown(shutdownCtx); err != nil {
		log.Error("error shutting down health server", "error", err)
	}

	select {
	case <-done:
	case <-shutdownCtx.Done():
		log.Warn("timed out waiting for in-flight jobs")
	}

	pool.Close()
	if err := rdb.Close(); err != nil {
		log.Error("error closing redis", "error", err)
	}

	log.Info("shutdown complete")
}

// loadDotEnv loads environment variables from a .env file if one is found,
// mirroring cmd/api's lookup so `go run ./cmd/worker` works the same way
// regardless of the working directory it's invoked from.
func loadDotEnv() {
	for _, path := range []string{"../../.env", "../.env", ".env"} {
		if err := godotenv.Load(path); err == nil {
			return
		}
	}
}
```

The 10s shutdown budget (vs. the API's 5s) allows an in-flight delete batch
to finish. Jobs still get their ctx cancelled at the signal, so the cleanup
loop exits after its current batch commits.

---

## Step 7 — Tests

Match existing conventions: table-driven where natural, `t.Skip` for
integration tests missing env, hand-written mocks (no mocking library).

### 7a. `internal/worker/worker_test.go` (unit, no infra)

Fakes:

```go
type fakeJob struct {
	name     string
	interval time.Duration
	runs     chan struct{}  // buffered; each Run sends
	fn       func(ctx context.Context) (worker.Result, error)
}

type fakeLocker struct {
	mu       sync.Mutex
	acquire  bool
	acqErr   error
	released int
}
```

Cases (use `interval: 20 * time.Millisecond` and channel receives with a
`select` + `time.After(2 * time.Second)` timeout — no `time.Sleep`-based
assertions, so the tests stay fast and non-flaky):

1. **Runs at startup** — one receive on `runs` arrives before an interval
   elapses.
2. **Runs repeatedly** — three receives arrive, then cancel ctx.
3. **Lock held elsewhere → skipped** — `acquire: false`; assert no run
   happened and `Stats()[0].Skipped > 0`.
4. **Lock error → recorded, no run** — `acqErr` set; `Failures == 1`,
   `LastError` non-empty.
5. **Job error → lock released** — job returns an error; assert
   `fakeLocker.released == 1`, `Failures == 1`, `LastError` set.
6. **Panic recovered** — job panics; assert the worker keeps running (a
   second run still happens), `Failures >= 1`, `LastError` contains
   `"panic:"`.
7. **Success does not release** — job succeeds; `fakeLocker.released == 0`.
8. **Per-run timeout** — `jobTimeout: 10ms`, job blocks on `<-ctx.Done()`
   and returns `ctx.Err()`; assert it returns rather than hanging.
9. **Cancel stops Run** — `Run(ctx)` returns after cancel (guard the whole
   test with a timeout).
10. **Duplicate Register panics** — `defer func(){ recover() }()` assertion.
11. **`lockTTL`** — table: `1h → 54m`, `20ms → 18ms`, tiny values never
    return `<= 0`.

Put the fakes in the test file; the package is `worker` (internal test
package) so `lockTTL` is reachable — or split: `worker_test.go` in package
`worker` for `lockTTL`, everything else via the exported API.

### 7b. `internal/job/sessioncleanup/service_test.go` (unit, mock store)

```go
type mockStore struct {
	returns []int64
	errs    []error
	calls   []db.DeleteExpiredSessionsParams
}
```

Cases:

1. **Single short batch** — store returns `7` with `batchSize=1000`; assert
   one call, `Result.Affected == 7`, `Attrs` contain `drained true`.
2. **Multi-batch drain** — returns `1000, 1000, 42`; assert three calls,
   `Affected == 2042`.
3. **Params correct** — `retention: 720h`, `batchSize: 500` →
   `RetentionSeconds == 2_592_000`, `BatchSize == 500`.
4. **Store error propagates** — second call errors; assert error wraps
   `"delete expired sessions"` and `Affected` reflects the first batch.
5. **Batch cap** — always return exactly `batchSize`; assert exactly
   `maxBatches` calls and `drained false`.
6. **Ctx cancelled mid-loop** — cancel after the first batch; assert the
   returned error is `context.Canceled` and the partial count is reported.
7. **Interface compliance** — `var _ worker.Job = (*Job)(nil)` (compile-time,
   already in the source file) plus `Name() == "session-cleanup"`.

### 7c. `internal/job/sessioncleanup/service_integration_test.go` (real Postgres)

Skip unless `DATABASE_URL` is set; run `goose.UpContext(ctx, sqlDB, ".")`
with `migrations.FS`, same as `internal/server/auth_integration_test.go`.

Seed one user (uuid-suffixed email so runs stay isolated), then four
sessions via direct `INSERT` (raw SQL, so `created_at`/`expires_at` can be
backdated — `CreateSession` cannot set `created_at`):

| Fixture | expires_at | is_revoked | created_at | Expected |
| --- | --- | --- | --- | --- |
| expired | `now() - 1h` | false | `now()` | **deleted** |
| revoked-old | `now() + 24h` | true | `now() - 60d` | **deleted** |
| revoked-recent | `now() + 24h` | true | `now() - 1d` | kept |
| active | `now() + 24h` | false | `now()` | kept |

Run `New(store, log, time.Hour, 30*24*time.Hour, 1000).Run(ctx)`, then assert
by id which rows survive. Assert `Affected >= 2` (other tests' rows may also
be collected — do **not** assert an exact global count). `t.Cleanup` deletes
the seeded user (cascade removes its sessions).

Also add a **batching** case: insert 5 expired sessions, run with
`batchSize: 2`, assert all 5 are gone and `Attrs` report more than one batch.

### 7d. `internal/worker/lock_integration_test.go` (real Redis)

Skip unless `REDIS_URL` is set. Use a uuid-suffixed job name per test so
parallel/repeat runs do not collide; `t.Cleanup` releases.

1. Acquire → `true`; second acquire with a different owner → `false`.
2. Release by the true owner → next acquire succeeds.
3. Release by a **different** owner → no-op; the original holder's lock
   survives (assert a third-party acquire still returns `false`).
4. TTL expiry: acquire with `50ms`, wait, acquire again → `true`.

---

## Step 8 — Build, run, and deploy wiring

### 8a. `Makefile`

Add `worker` to `.PHONY` and this target after `api:`:

```make
## Run the background worker (requires `make up` first and a .env file)
worker:
	cd apps/backend && go run ./cmd/worker
```

Also extend the `build:` target's backend line:

```make
	cd apps/backend && go build -o bin/api ./cmd/api && go build -o bin/worker ./cmd/worker
```

And update the `dev:` echo block to mention the third terminal:

```
	@echo "  make worker # background jobs"
```

### 8b. `apps/backend/Dockerfile`

Add after the healthcheck build line:

```dockerfile
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/worker ./cmd/worker
```

and in the runner stage, after the healthcheck copy:

```dockerfile
COPY --from=builder /out/worker ./worker
```

One image, two entrypoints — the compose/k8s worker overrides
`ENTRYPOINT` to `/app/worker`. Leave the API's `ENTRYPOINT` and
`HEALTHCHECK` unchanged.

### 8c. `apps/backend/cmd/healthcheck/main.go`

Three-line change so the same probe binary can target the worker's port:

```go
	port := os.Getenv("HEALTHCHECK_PORT")
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "3000"
	}
```

Update the package doc to mention it probes `HEALTHCHECK_PORT` when set.
Backward compatible: the API container sets neither and still gets 3000/`PORT`.

### 8d. `compose.yaml`

Add after the `api:` service (before `web:`):

```yaml
  worker:
    build:
      context: ./apps/backend
    container_name: controlplane-worker
    entrypoint: ["/app/worker"]
    env_file:
      - .env.docker
    environment:
      # The worker's internal ops endpoint; also what the image's
      # HEALTHCHECK probes, overriding the API's PORT from .env.docker.
      WORKER_PORT: "3001"
      HEALTHCHECK_PORT: "3001"
    depends_on:
      db:
        condition: service_healthy
      redis:
        condition: service_healthy
    restart: unless-stopped
```

No `ports:` mapping — the health endpoint is internal. Do not add the worker
to `web`'s `depends_on`.

### 8e. `k8s/worker/deployment.yaml` (new directory, Deployment only)

No Service and no Ingress — nothing routes to the worker.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controlplane-worker
  namespace: controlplane
spec:
  replicas: 1
  selector:
    matchLabels:
      app: controlplane-worker
  template:
    metadata:
      labels:
        app: controlplane-worker
    spec:
      containers:
        - name: controlplane-worker
          image: controlplane-api:latest
          imagePullPolicy: Never
          command: ["/app/worker"]
          ports:
            - containerPort: 3001
          env:
            - name: WORKER_PORT
              value: "3001"
          envFrom:
            - configMapRef:
                name: controlplane-config
            - secretRef:
                name: controlplane-secret
          livenessProbe:
            httpGet:
              path: /health
              port: 3001
            initialDelaySeconds: 10
            periodSeconds: 30
          readinessProbe:
            httpGet:
              path: /health
              port: 3001
            initialDelaySeconds: 5
            periodSeconds: 10
          resources:
            requests:
              memory: 64Mi
              cpu: 50m
            limits:
              memory: 256Mi
              cpu: 250m
```

`replicas: 1` is the sane default; the Redis lock makes `> 1` safe, so note
that in `k8s/README.md`. The explicit `env: WORKER_PORT` overrides the
shared configmap's `PORT` for the health server. Add a short worker section
to `k8s/README.md` mirroring how the api/redis/postgres sections read.

### 8f. CI

**No changes.** `go vet ./...` / `go build ./...` / `go test ./...` already
cover the new packages and binary, and the `docker` job already builds the
image that now carries the worker. Do not touch `.github/workflows/*`.

---

## Step 9 — Documentation

### `docs/03-target-architecture.md`

1. In the **Monorepo layout** tree, under `apps/backend/`, add:
   ```
   │   ├── cmd/worker/main.go    # background job runner (scheduler + /health on WORKER_PORT)
   ```
   and under `internal/`:
   ```
   │   │   ├── worker/           # Job interface, registry, interval scheduler, redis lock
   │   │   ├── job/              # job implementations (sessioncleanup, …)
   ```
   Also add `k8s/worker/` to the `k8s/` line's comment.

2. Add a **Background jobs** bullet to "Backend architecture notes":
   > - **Background jobs**: `cmd/worker` is a separate process sharing the
   >   API's config/store/logger. Jobs implement
   >   `worker.Job` (`Name`/`Interval`/`Run`) and are registered in
   >   `cmd/worker/main.go`. The scheduler adds per-run timeouts, panic
   >   recovery, structured logging, and a Redis lock
   >   (`worker:lock:<job>`) held for ~one interval so a multi-replica
   >   fleet runs each job about once per interval. Failed runs release the
   >   lock immediately so the next tick retries. The guarantee jobs may rely
   >   on is therefore **at most one run per interval fleet-wide**, not merely
   >   "no two runs at once" — chosen so non-idempotent future jobs (mail,
   >   billing, webhooks) are safe to register without re-deriving the
   >   coordination story. Job outcomes are exposed on the worker's internal
   >   `GET /health` (`WORKER_PORT`, default 3001) — not part of the public
   >   API contract.

3. Add a new section after "Deviations resolved during Phase 6":

   ```markdown
   ## Deviations resolved during Phase 7 (background worker)

   1. **New indexes on `sessions` (migration `00006`).** `CLAUDE.md` requires
      the schema to stay byte-identical to the source app's Drizzle
      migrations. The session-cleanup job scans by `expires_at` and by
      `created_at` for revoked rows, neither of which the source schema
      indexes (the source has no cleanup job at all). Agreed with the owner
      on 2026-07-31: add `idx_sessions_expires_at` and a partial
      `idx_sessions_revoked_created_at`. Indexes only — no table, column, or
      constraint differs, so a source-app database snapshot still runs
      unchanged under the Go app and vice versa.
   2. **Revoked-session retention window.** Revoked-but-unexpired sessions are
      kept for `SESSION_CLEANUP_RETENTION` (default 30d) rather than deleted
      on revocation, so refresh-token reuse detection still recognises a
      replayed token as a family reuse instead of an unknown session.
   ```

### `docs/04-migration-plan.md`

Append to "Suggested phases" (the file contains the phase list twice — a
lead-in copy and the tail; update **both** occurrences to stay consistent):

```markdown
### Phase 7 — Background worker
Fifth binary `cmd/worker` in the Go module: `worker.Job` interface + interval
scheduler + Redis job lock (`worker:lock:<job>`), internal `/health` on
`WORKER_PORT`. First job: session cleanup (batched delete of expired sessions
and revoked ones past a 30d retention window) with supporting indexes
(migration `00006`). Compose `worker` service and `k8s/worker/` Deployment.
Not in the source app — new capability, not a port.
```

### `README.md`

- Add `make worker` to the commands table/list with "background job runner
  (session cleanup); third terminal alongside `make api` / `make web`".
- Add the five new env vars to the environment section with their defaults.
- One paragraph on adding a job: implement `worker.Job` in
  `internal/job/<name>/`, register it in `cmd/worker/main.go`.

### `CLAUDE.md`

- **Status**: change the opening to "Phase 7 (background worker) complete" and
  append a sentence describing `cmd/worker`, the `Job` interface, the session
  cleanup job, and its env vars.
- **Layout** block: add `worker/` and `job/` to the `apps/backend/` line.
- **Ground rules**: add
  > - Background jobs implement `worker.Job` and are registered in
  >   `cmd/worker/main.go`. Job runs are guarded by a Redis lock
  >   (`worker:lock:<job>`, TTL ≈ one interval) so multiple replicas run a job
  >   about once per interval; a failed run releases the lock for an immediate
  >   retry. Job failures never crash the worker (panics are recovered).
- **Redis keys** line: add `worker:lock:<jobName>` (TTL ≈ job interval).
- **Commands**: add `make worker`.
- **Environment**: add the five new vars.

---

## Verification

Run from the repo root unless noted.

```bash
# 1. Build everything, including the new binary
cd apps/backend && go build ./... && go vet ./...

# 2. Migration applies and rolls back cleanly
go run ./cmd/migrate up
go run ./cmd/migrate status          # 00006 present
go run ./cmd/migrate down            # drops the indexes
go run ./cmd/migrate up

# 3. Unit tests (no infra needed — integration ones self-skip)
go test ./internal/worker/... ./internal/job/... -v

# 4. Full suite with infra (from repo root: make up first)
DATABASE_URL=... REDIS_URL=... go test ./...

# 5. Lint
make lint

# 6. Live smoke test — terminal 1
make up && make migrate
SESSION_CLEANUP_INTERVAL=10s make worker
#   expect: "Redis connected" → "Database connected" →
#           "worker health server listening addr=:3001" →
#           "worker started jobs=1" →
#           "job completed job=session-cleanup affected=0 batches=1 drained=true"
#           …repeating every ~10s (each tick re-acquires: TTL is 9s)

# terminal 2
curl -s localhost:3001/health
#   expect: {"status":"ok","uptime":…,"jobs":[{"name":"session-cleanup","runs":…}]}

# 7. Lock behaviour — start a SECOND worker in terminal 3 with the same
#    interval; expect its log to show "job skipped, lock held elsewhere"
#    (LOG_LEVEL=debug) and its /health Skipped counter to climb while the
#    first worker's Runs counter climbs.

# 8. Graceful shutdown: Ctrl-C the worker →
#    "shutdown signal received" → "worker stopped" → "shutdown complete", exit 0.

# 9. Containerised
docker compose up -d --build
docker compose ps          # controlplane-worker healthy
docker compose logs worker
docker compose down
```

## Definition of done

1. `go build ./...`, `go vet ./...`, `make lint`, and `go test ./...` (with
   Postgres + Redis available) all pass.
2. `make worker` runs the session cleanup on schedule and logs one
   `job completed` line per run with `affected`/`batches`/`drained`.
3. `curl localhost:3001/health` returns `{status, uptime, jobs[]}` with live
   per-job counters.
4. Two concurrent workers produce **one** run per interval overall (the
   second logs a skip).
5. The integration test proves the exact fixture matrix: expired and
   revoked-old rows deleted; revoked-recent and active rows kept.
6. `docker compose up -d --build` brings up a healthy `controlplane-worker`.
7. Migration `00006` applies and rolls back cleanly.
8. Adding a second job requires exactly one new package under `internal/job/`
   and one `w.Register(...)` line — verify by reading `cmd/worker/main.go`,
   no other file should need touching.
9. `docs/03`, `docs/04`, `README.md`, and `CLAUDE.md` updated as specified;
   `docs/02-api-contract.md` untouched.

## Commit plan

Small, reviewable commits in this order (each should build):

1. `feat(backend): add session cleanup indexes migration` — Step 1.
2. `feat(backend): add DeleteExpiredSessions query` — Step 2 (note in the
   body whether sqlc regenerated or the code was hand-written).
3. `feat(backend): add worker config` — Step 3 + env files.
4. `feat(backend): add pluggable background job runner` — Step 4.
5. `feat(backend): add session cleanup job` — Step 5.
6. `feat(backend): add cmd/worker binary` — Step 6.
7. `test(backend): cover worker scheduler and session cleanup` — Step 7.
8. `build: ship worker in image, compose, k8s, and Makefile` — Step 8.
9. `docs: record Phase 7 background worker` — Step 9.
