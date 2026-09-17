-- name: CreateUsageEvent :exec
-- One row per billable MCP tool call, written from the gateway's hot path
-- (internal/module/mcp/service.go) immediately alongside the mcp.tool.called
-- audit row -- see migration 00014's comment on usage_events. connector_id
-- and mcp_key_id are nullable FKs (ON DELETE SET NULL) so a later connector
-- or key deletion never erases the count it represents. quantity is not
-- taken as a parameter: it defaults to 1, matching "one row = one billable
-- tool call" -- this is never the rate limiter's N-upstream-request charge.
INSERT INTO usage_events (organization_id, connector_id, mcp_key_id, tool)
VALUES ($1, $2, $3, $4);

-- name: RollupUsageEvents :execrows
-- Folds usage_events into usage_rollups for every period whose bucket start
-- (UTC calendar month, date_trunc('month', occurred_at)) falls on or after
-- `since`. internal/job/usagerollup is the only caller, and it is the one
-- that must keep `since` inside the still-fully-present window -- see that
-- package's rollupLookbackMonths comment for why a re-aggregation must never
-- reach a period that a prior prune has partially emptied.
--
-- The ON CONFLICT target is usage_rollups' natural key from migration 00014
-- (organization_id, period_start, tool) -- deliberately excluding
-- period_end, per that migration's comment -- so re-running this over the
-- same window is idempotent: it recomputes each period's true count from
-- usage_events and overwrites the existing rollup rather than adding to it.
-- reported_at is never set here (decision 1: a cap, not a charge -- nothing
-- is ever reported to Stripe from this table).
INSERT INTO usage_rollups (organization_id, period_start, period_end, tool, call_count)
SELECT
    organization_id,
    date_trunc('month', occurred_at) AS period_start,
    date_trunc('month', occurred_at) + INTERVAL '1 month' AS period_end,
    tool,
    count(*) AS call_count
FROM usage_events
WHERE occurred_at >= sqlc.arg('since')::timestamp
GROUP BY organization_id, date_trunc('month', occurred_at), tool
ON CONFLICT (organization_id, period_start, tool)
DO UPDATE SET
    call_count = EXCLUDED.call_count,
    period_end = EXCLUDED.period_end;

-- name: CountUsageEventsSince :one
-- The usage_events side of the rollup job's audit_logs cross-check. Bounded
-- by the same `since` the rollup itself uses, and served by
-- idx_usage_events_occurred_at (00014) -- the same index the prune query
-- uses, both leading on occurred_at with no organization_id predicate.
SELECT count(*) FROM usage_events
WHERE occurred_at >= sqlc.arg('since')::timestamp;

-- name: CountUsageEventsForOrgSince :one
-- Step 5 of docs/12-billing-and-metering.md (not yet written; see
-- .claude/plans/2026-09-13-billing-and-usage-metering.md): the current
-- tool-call count subscription.Service.EnforceLimit checks against
-- max_tool_calls_per_month before a tools/call dispatches
-- (internal/module/mcp/service.go). Counts usage_events directly rather
-- than reading usage_rollups, because the rollup for the current, still-open
-- month can be up to USAGE_ROLLUP_INTERVAL stale -- counting raw events
-- matches the cap's window exactly instead of undercounting by up to one
-- interval. Callers pass the start of the current UTC calendar month as
-- `since`, the same boundary internal/job/usagerollup buckets on
-- (date_trunc('month', occurred_at)), so the cap and the rollup agree on
-- what "this month" means. Served by
-- idx_usage_events_organization_id_occurred_at (00014), which leads on
-- organization_id -- unlike CountUsageEventsSince below, which has no
-- organization_id predicate and is served by the occurred_at-only index
-- instead.
SELECT count(*) FROM usage_events
WHERE organization_id = $1
  AND occurred_at >= sqlc.arg('since')::timestamp;

-- name: CountAuditLogsToolCalledSince :one
-- The audit_logs side of the same cross-check. "Two independent counters
-- disagreeing is the detection mechanism" (plan's "the metering problem,
-- stated plainly") for the undercounting invariant 3 makes structural: a
-- dropped usage_events write (logged at error, internal/module/mcp's
-- recordUsage) or a dropped audit_logs write (best-effort,
-- auditlog.Service.Record) shows up here as nonzero drift rather than
-- silently. created_at has no time zone -- `since` must already be UTC wall
-- clock, matching every other naive-timestamp comparison in this codebase
-- (see auditlog.sql's QueryAuditLogs comment). Served by
-- idx_audit_logs_created_at (00011), which leads on created_at alone --
-- the same shape idx_audit_logs_organization_id_created_at (00009) cannot
-- serve since this query has no organization_id predicate.
SELECT count(*) FROM audit_logs
WHERE action = 'mcp.tool.called'
  AND created_at >= sqlc.arg('since')::timestamp;

-- name: ListUsageRollupsForOrgPeriod :many
-- Step 9 of .claude/plans/2026-09-13-billing-and-usage-metering.md: the
-- per-tool breakdown behind GET /billing/usage's `byTool` field
-- (internal/module/billing). Reads usage_rollups, NOT usage_events, unlike
-- CountUsageEventsForOrgSince above -- deliberately, and asymmetrically.
-- CountUsageEventsForOrgSince has to be exact because it is the number the
-- gateway's own quota check (internal/module/mcp/service.go:305) enforces
-- against; this is a per-tool breakdown a customer finds informative, not
-- the enforced number, so it is allowed to lag by up to
-- USAGE_ROLLUP_INTERVAL for the still-open current month
-- (internal/job/usagerollup) rather than paying the cost of grouping the
-- full month's usage_events on every page load of the usage meter. Callers
-- pass the same UTC-calendar-month `period_start` the rollup job buckets
-- on, matching CountUsageEventsForOrgSince's `since`. Served by the unique
-- index the natural key (organization_id, period_start, tool) already
-- creates (migration 00014) -- no new index needed.
SELECT tool, call_count FROM usage_rollups
WHERE organization_id = $1 AND period_start = $2
ORDER BY tool ASC;

-- name: PruneUsageEvents :execrows
-- Deletes usage_events rows older than retention, batch-at-a-time like
-- PruneEmailOutbox (email_outbox.sql) and DeleteExpiredSessions
-- (sessions.sql). Deliberately has no organization_id predicate -- this is
-- the query idx_usage_events_occurred_at (00014) exists for; adding one
-- would defeat that index on this table, which is the largest in the
-- schema (one row per tool call).
DELETE FROM usage_events
WHERE id IN (
    SELECT id FROM usage_events
    WHERE occurred_at < now() - (sqlc.arg('retention_seconds')::int * INTERVAL '1 second')
    LIMIT sqlc.arg('batch_size')
);
