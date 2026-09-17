-- +goose Up
-- Step 2 of docs/12-billing-and-metering.md (not yet written; see
-- .claude/plans/2026-09-13-billing-and-usage-metering.md): the usage ledger.
--
-- Per the plan's "the metering problem, stated plainly": mcp.tool.called
-- audit rows look like a ready-made usage ledger but are not one —
-- auditlog.Service.Record is best-effort and returns nothing on failure
-- (CLAUDE.md: "audit-log writes are best-effort, never fail the request"),
-- so a dropped insert there is silently unbilled usage with no
-- reconciliation path. usage_events is a dedicated, independently-written
-- ledger; usage_rollups is what step 4's job folds it into, cross-checked
-- against the audit_logs count for the same window so the two counters
-- disagreeing is the detection mechanism for undercounting (invariant 3
-- forbids ever failing a tools/call to make counting more accurate).
CREATE TABLE "usage_events" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"organization_id" uuid NOT NULL,
	"connector_id" uuid,
	"mcp_key_id" uuid,
	-- Tool name only, per CLAUDE.md's "column names are recorded; the
	-- values filtered on are not" gateway rule — never tool arguments.
	"tool" text NOT NULL,
	"occurred_at" timestamp DEFAULT now() NOT NULL,
	"quantity" integer DEFAULT 1 NOT NULL
);

-- organization_id cascades from organizations, matching connectors
-- (00007) and mcp_api_keys (00008): an org being deleted genuinely ends
-- its billing relationship, so its usage ledger goes with it.
--
-- connector_id and mcp_key_id do NOT cascade from their owning tables,
-- unlike every other org-scoped child table so far — this is deliberate,
-- not an oversight. If connector_id cascaded, deleting a connector would
-- erase its usage rows, which (a) hands a customer a one-click quota
-- reset by deleting and recreating a connector, defeating step 5's cap
-- outright, and (b) destroys the audit_logs cross-check this whole table
-- exists to enable. ON DELETE SET NULL preserves the row — and the
-- count it represents — while letting the FK go stale-safe; both columns
-- are nullable for exactly this reason.
ALTER TABLE "usage_events" ADD CONSTRAINT "usage_events_organization_id_organizations_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organizations"("id") ON DELETE cascade ON UPDATE no action;
ALTER TABLE "usage_events" ADD CONSTRAINT "usage_events_connector_id_connectors_id_fk" FOREIGN KEY ("connector_id") REFERENCES "public"."connectors"("id") ON DELETE set null ON UPDATE no action;
ALTER TABLE "usage_events" ADD CONSTRAINT "usage_events_mcp_key_id_mcp_api_keys_id_fk" FOREIGN KEY ("mcp_key_id") REFERENCES "public"."mcp_api_keys"("id") ON DELETE set null ON UPDATE no action;

-- The plan names this as the rollup job's (step 4) only access pattern —
-- fold usage_events for one org within a period into usage_rollups — and
-- it doubles as step 5's "calls this org made this month" cap count
-- (subscription.Service.EnforceLimit), so one index serves both readers.
CREATE INDEX IF NOT EXISTS "idx_usage_events_organization_id_occurred_at" ON "usage_events" ("organization_id","occurred_at");

-- Step 4's retention prune is cross-org — it deletes by age alone, with no
-- organization_id predicate — so the composite index above cannot serve it
-- (organization_id leads). Same reasoning migration 00011 recorded when it
-- added idx_audit_logs_created_at alongside 00009's (organization_id,
-- created_at), and the same pattern as idx_sessions_expires_at (00006) and
-- idx_email_outbox_prune (00010): every job that prunes by age gets an
-- index it can actually use. This is the largest table in the schema (one
-- row per tool call), so a seq scan here is the one that matters.
CREATE INDEX IF NOT EXISTS "idx_usage_events_occurred_at" ON "usage_events" ("occurred_at");

-- Retention: raw usage_events are pruned once rolled up; usage_rollups is
-- the durable record that survives (unbounded — it is small, one row per
-- org/period/tool, and is the thing invoices and the frontend usage meter
-- read). Raw events only need to live long enough to (a) be folded into a
-- rollup and (b) leave a forensic window for the audit_logs drift
-- cross-check and support disputes that reference a specific tool call
-- rather than just a period total. 90 days (2160h) covers roughly three
-- monthly billing cycles of investigation headroom, well past any
-- plausible rollup-job outage, without keeping a per-call ledger forever.
-- Step 4's job reads this as USAGE_EVENTS_RETENTION (default "2160h"),
-- following the SESSION_CLEANUP_RETENTION ("720h") / EMAIL_OUTBOX_RETENTION
-- ("168h") naming convention in internal/config/config.go — not wired up
-- here; that plumbing belongs to step 4 with the job that reads it.
CREATE TABLE "usage_rollups" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"organization_id" uuid NOT NULL,
	"period_start" timestamp NOT NULL,
	"period_end" timestamp NOT NULL,
	"tool" text NOT NULL,
	-- "count" is a SQL reserved word; call_count is unambiguous everywhere
	-- (including hand-written queries the audit-log cross-check will
	-- need in step 4) rather than requiring "count" to be quoted at every
	-- use site the way plan_prices.sql quotes "interval" (00013).
	"call_count" integer NOT NULL,
	-- Stays NULL under decision 1 (plan §Decisions): a tool call is a cap,
	-- not a charge, so nothing here is ever reported to Stripe. The
	-- column exists only as the hook if that decision reopens; step 4
	-- does not populate it and no reporting job should be built against
	-- it until then.
	"reported_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL
);

-- Matches usage_events: an org being deleted ends its billing
-- relationship, so its rollups (the durable billing-facing record) go
-- with it rather than becoming orphaned history for an org that no
-- longer exists.
ALTER TABLE "usage_rollups" ADD CONSTRAINT "usage_rollups_organization_id_organizations_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organizations"("id") ON DELETE cascade ON UPDATE no action;

-- Natural key for step 4's idempotent upsert (ON CONFLICT DO UPDATE):
-- re-running the rollup for the same org/period/tool must update the
-- existing row, not double-count into a second one. Deliberately
-- excludes period_end: if period_end were part of the key, a rollup
-- re-run with a shifted window boundary for what is meant to be the same
-- period (e.g. a job re-deployed with a different bucket size, or a
-- manual backfill with a slightly different period_end) would silently
-- INSERT a duplicate row for that period instead of conflicting with the
-- existing one. Keying on (organization_id, period_start, tool) alone
-- means any such mismatch collapses onto the existing row instead of
-- doubling the period's history — the count is overwritten by the
-- re-run's own total, which is the idempotent outcome the rollup wants.
ALTER TABLE "usage_rollups" ADD CONSTRAINT "usage_rollups_organization_id_period_start_tool_unique" UNIQUE("organization_id","period_start","tool");

-- +goose Down
ALTER TABLE "usage_rollups" DROP CONSTRAINT "usage_rollups_organization_id_period_start_tool_unique";
ALTER TABLE "usage_rollups" DROP CONSTRAINT "usage_rollups_organization_id_organizations_id_fk";
DROP TABLE "usage_rollups";
DROP INDEX IF EXISTS "idx_usage_events_occurred_at";
DROP INDEX IF EXISTS "idx_usage_events_organization_id_occurred_at";
ALTER TABLE "usage_events" DROP CONSTRAINT "usage_events_mcp_key_id_mcp_api_keys_id_fk";
ALTER TABLE "usage_events" DROP CONSTRAINT "usage_events_connector_id_connectors_id_fk";
ALTER TABLE "usage_events" DROP CONSTRAINT "usage_events_organization_id_organizations_id_fk";
DROP TABLE "usage_events";
