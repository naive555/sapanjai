// Package usagerollup folds the gateway's usage_events ledger
// (internal/module/mcp/service.go's recordUsage, written on every permitted
// tools/call) into usage_rollups, prunes usage_events rows past retention,
// and emits a cross-check against audit_logs so undercounting is detected
// rather than silent — see the plan's "the metering problem, stated
// plainly" and migration 00014's comment for the full reasoning.
//
// # The correctness trap this package exists to avoid
//
// The rollup is idempotent by construction: it recomputes a period's count
// straight from usage_events and UPSERTs onto usage_rollups' natural key
// (organization_id, period_start, tool) — see RollupUsageEvents
// (internal/infra/database/queries/usage.sql). That recomputation is correct
// only while every one of that period's raw events is still present. Once
// the prune below has removed part of a period's events, re-aggregating
// that period computes a SMALLER count and overwrites a complete rollup
// with a partial one — silently destroying durable billing history, not
// just failing loudly.
//
// So Run only ever recomputes periods that began at or after the prune's
// own cutoff — rollupSince clamps the window against USAGE_EVENTS_RETENTION
// rather than trusting rollupLookbackMonths, which is a ceiling and not a
// safety bound (see both doc comments). And it always rolls up before it
// prunes, so a run that dies between the two steps has lost nothing: the
// rollup already committed reflects a fully-present window, and the next
// run's prune sees the same (or a strictly newer) boundary.
//
// # Period = calendar month, UTC
//
// max_tool_calls_per_month (a later step, not built here) is what these
// rollups will feed, so the rollup's period must match that cap's window or
// the cap counts the wrong thing. usage_events.occurred_at and
// usage_rollups.period_start/period_end are all `timestamp without time
// zone` columns, and RollupUsageEvents buckets with
// date_trunc('month', occurred_at) — a local-timezone date_trunc would
// silently shift every customer's billing boundary, so every timestamp this
// package hands to SQL is normalised to UTC first (see rollupSince).
//
// period_end is the exclusive end of the month and is set on every UPSERT,
// but is deliberately not part of usage_rollups' unique key (migration
// 00014's comment on that constraint). reported_at is never written here:
// per decision 1 (plan §Decisions) a tool call is a cap, not a charge, so
// nothing in this table is ever reported to Stripe.
package usagerollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/worker"
)

var (
	_ rollupStore = (*database.Store)(nil)
	_ worker.Job  = (*Job)(nil)
)

// rollupLookbackMonths bounds how many trailing calendar months (the
// current, partial one included) one run recomputes.
//
// This is only a ceiling, deliberately. It is NOT what keeps the rollup
// clear of the prune — rollupSince's clamp against USAGE_EVENTS_RETENTION
// is, and that clamp is what makes the job correct for any retention
// value an operator configures. See rollupSince's doc comment for why a
// constant cannot carry that guarantee (briefly: 3 calendar months span up
// to 92 days, which overshoots the default 90-day retention on month-end
// days, and a lowered retention makes any fixed lookback unsafe).
//
// What this constant does buy, within what the clamp allows, is recovery
// headroom: one run absorbs up to ~2 months of missed rollups after a job
// outage, plus any late-landing event near a period boundary. Raising it
// is safe — the clamp still bounds the real reach — but pointless beyond
// what retention permits.
const rollupLookbackMonths = 3

// maxPruneBatches caps one run's prune sweep so a pathological backlog
// cannot hold the job lock indefinitely; the remainder drains on a later
// run, matching internal/job/sessioncleanup's maxBatches.
const maxPruneBatches = 100

// rollupStore is the subset of *database.Store this job needs, narrowed so
// unit tests can hand-mock it (same pattern as cleanupStore in
// internal/job/sessioncleanup and dispatchStore in internal/job/emaildispatch).
type rollupStore interface {
	RollupUsageEvents(ctx context.Context, since time.Time) (int64, error)
	CountUsageEventsSince(ctx context.Context, since time.Time) (int64, error)
	CountAuditLogsToolCalledSince(ctx context.Context, since time.Time) (int64, error)
	PruneUsageEvents(ctx context.Context, arg db.PruneUsageEventsParams) (int64, error)
}

// Job folds usage_events into usage_rollups, cross-checks the count against
// audit_logs, and prunes rolled-up events past retention — in that order,
// once per Interval.
type Job struct {
	store     rollupStore
	log       *slog.Logger
	interval  time.Duration
	retention time.Duration
	batchSize int32

	// now is the clock, swapped in tests so the rollup boundary
	// (rollupSince) is deterministic without depending on wall-clock time.
	now func() time.Time
}

// New builds the usage-rollup job.
func New(store rollupStore, log *slog.Logger, interval, retention time.Duration, batchSize int) *Job {
	return &Job{
		store:     store,
		log:       log,
		interval:  interval,
		retention: retention,
		batchSize: int32(batchSize),
		now:       time.Now,
	}
}

func (j *Job) Name() string            { return "usage-rollup" }
func (j *Job) Interval() time.Duration { return j.interval }

// Run rolls up, cross-checks, then prunes — never any other order, per the
// package doc's correctness trap.
func (j *Job) Run(ctx context.Context) (worker.Result, error) {
	if err := ctx.Err(); err != nil {
		return worker.Result{}, err
	}

	since := rollupSince(j.now(), j.retention)

	rolledUp, err := j.store.RollupUsageEvents(ctx, since)
	if err != nil {
		return worker.Result{}, fmt.Errorf("rollup usage events: %w", err)
	}

	attrs := []any{"rolled_up", rolledUp, "since", since}
	attrs = append(attrs, j.checkDrift(ctx, since)...)

	pruned, batches, drained, err := j.prune(ctx)
	attrs = append(attrs, "pruned", pruned, "prune_batches", batches, "prune_drained", drained)
	if err != nil {
		// The rollup above already committed; only the prune step failed.
		// Report it (the worker logs Result.Attrs even on error) but the
		// rolled-up count already reflects real, committed work.
		return worker.Result{Affected: rolledUp, Attrs: attrs}, err
	}

	return worker.Result{Affected: rolledUp, Attrs: attrs}, nil
}

// checkDrift compares usage_events' count against audit_logs' mcp.tool.called
// count over the same [since, now) window and logs the result. Per the
// plan's §Risks, drift is expected to be small and occasionally nonzero —
// both ledgers are written independently and neither may ever block a
// tools/call (invariant 3), so a dropped write on either side is possible by
// design. This is a signal to watch, not an alarm to page on: hence warn
// (not error) when nonzero, info when zero.
//
// A failure to even compute the counts is logged and swallowed — the
// cross-check is a diagnostic, not a correctness dependency, and must never
// stop the rollup that already committed or the prune that follows.
func (j *Job) checkDrift(ctx context.Context, since time.Time) []any {
	usageCount, usageErr := j.store.CountUsageEventsSince(ctx, since)
	auditCount, auditErr := j.store.CountAuditLogsToolCalledSince(ctx, since)
	if usageErr != nil || auditErr != nil {
		j.log.WarnContext(ctx, "usage rollup: drift cross-check unavailable",
			"error", errors.Join(usageErr, auditErr))
		return []any{"drift_error", true}
	}

	drift := auditCount - usageCount
	if drift != 0 {
		j.log.WarnContext(ctx, "usage rollup: usage_events/audit_logs drift detected",
			"usage_count", usageCount, "audit_count", auditCount, "drift", drift, "since", since)
	} else {
		j.log.InfoContext(ctx, "usage rollup: no drift between usage_events and audit_logs",
			"usage_count", usageCount, "audit_count", auditCount, "since", since)
	}

	return []any{"usage_count", usageCount, "audit_count", auditCount, "drift", drift}
}

// prune deletes usage_events rows older than retention, batch-at-a-time
// (mirroring internal/job/sessioncleanup.Run) so an aborted sweep keeps
// whatever it already deleted. It runs unconditionally on every call —
// unlike emaildispatch's hourly-throttled prune, this job's own Interval is
// already the only cadence control it needs, and PruneUsageEvents' query
// (idx_usage_events_occurred_at) is cheap at any reasonable interval.
func (j *Job) prune(ctx context.Context) (total int64, batches int, drained bool, err error) {
	params := db.PruneUsageEventsParams{
		RetentionSeconds: int32(j.retention.Seconds()),
		BatchSize:        j.batchSize,
	}

	for batches < maxPruneBatches {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return total, batches, false, ctxErr
		}

		deleted, err := j.store.PruneUsageEvents(ctx, params)
		if err != nil {
			return total, batches, false, fmt.Errorf("prune usage events: %w", err)
		}

		total += deleted
		batches++

		if deleted < int64(j.batchSize) {
			return total, batches, true, nil
		}
	}

	j.log.WarnContext(ctx, "usage rollup: prune hit batch cap, remainder deferred to next run",
		"deleted", total, "batches", batches)
	return total, batches, false, nil
}

// rollupSince returns the inclusive lower bound (UTC, month-aligned) of the
// window Run recomputes: the start of the current UTC calendar month minus
// rollupLookbackMonths-1 whole months, clamped so it never reaches back
// past the prune boundary.
//
// The clamp is the load-bearing half, and rollupLookbackMonths alone is not
// a safe bound. Three calendar months span up to 92 days (e.g. Jun+Jul+Aug),
// so on month-end days the unclamped lookback reaches ~2 days past a 90-day
// retention floor and recomputes a period the prune has already eaten into
// — the exact partial-overwrite this package exists to prevent. Worse,
// retention is USAGE_EVENTS_RETENTION, an operator-tunable env var: at 60d
// an unclamped 3-month lookback is unsafe on 363 days of the year, at 30d
// on all 365, silently and with nothing failing. A constant cannot be safe
// against a configurable boundary, so the boundary itself is what bounds us
// here.
//
// A period is safe to recompute only if all of its events are still
// present, i.e. only if the period began at or after the prune cutoff.
// Hence: take the first month-start at or after the cutoff as the floor,
// and let rollupLookbackMonths act as a ceiling on top of it.
func rollupSince(now time.Time, retention time.Duration) time.Time {
	now = now.UTC()
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	lookback := currentMonthStart.AddDate(0, -(rollupLookbackMonths - 1), 0)

	// The prune deletes occurred_at < now-retention, so the oldest period
	// still guaranteed whole is the first one starting at or after that
	// cutoff. A cutoff landing mid-month means that month is already
	// partially gone, so the floor is the *next* month start.
	cutoff := now.Add(-retention)
	cutoffMonthStart := time.Date(cutoff.Year(), cutoff.Month(), 1, 0, 0, 0, 0, time.UTC)
	floor := cutoffMonthStart
	if cutoff.After(cutoffMonthStart) {
		floor = cutoffMonthStart.AddDate(0, 1, 0)
	}

	if lookback.Before(floor) {
		return floor
	}
	return lookback
}
