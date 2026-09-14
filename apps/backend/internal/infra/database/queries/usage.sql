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
