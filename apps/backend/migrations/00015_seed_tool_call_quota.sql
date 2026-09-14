-- +goose Up
-- Step 5 of docs/12-billing-and-metering.md (not yet written; see
-- .claude/plans/2026-09-13-billing-and-usage-metering.md): give every
-- already-seeded default plan a max_tool_calls_per_month limit.
--
-- Trap this migration exists to close: cmd/seed's UpsertPlan is
-- `INSERT ... ON CONFLICT (name) DO NOTHING`
-- (internal/infra/database/queries/plans.sql), so adding
-- max_tool_calls_per_month to cmd/seed/main.go's defaultPlans has NO effect
-- on any database where "free"/"pro"/"enterprise" already exist -- which is
-- every environment that has ever run `make seed`, this one included.
-- Without this migration, every existing install would silently keep an
-- unlimited tool-call quota forever, because
-- subscription.Service.EnforceLimit treats a missing limit key as
-- unlimited by design (its own doc comment: "No subscription, no limit for
-- key, or a limit of -1 ... all pass").
--
-- This is a data migration, not a rerun of the seed, and it is careful in
-- two directions:
--   1. It merges the key into "limits" only where the key is not already
--      present (`NOT (limits ? 'max_tool_calls_per_month')`), so it can
--      never clobber a value an operator already set by hand through the
--      superadmin plan-CRUD route (PATCH /admin/plans/:id,
--      docs/11-admin-panel.md) -- CLAUDE.md's ground rule that a webhook
--      or migration must not stomp an admin override, applied here too.
--   2. It only touches the three plan rows this codebase itself seeds by
--      name. A custom plan an operator created under a different name is
--      left untouched -- it keeps whatever limits it has, unlimited tool
--      calls included, which is the same "missing key = unlimited"
--      behavior every other limit key already has, and is preferable to
--      guessing a cap for a plan this migration knows nothing about.
--
-- Values are provisional pricing input, not an engineering decision --
-- flagged as such in the step-5 report: with zero customers there is no
-- observed usage distribution to calibrate against (decision 1's own
-- reasoning). They match cmd/seed/main.go's defaultPlans exactly, so a
-- fresh install (seed inserts the row for the first time, this migration
-- then finds the key already present and no-ops) and an existing install
-- (this migration adds the key, seed's UpsertPlan then no-ops on the
-- already-existing row) converge on the same numbers either way.
UPDATE "plans" SET "limits" = "limits" || '{"max_tool_calls_per_month": 1000}'::jsonb
WHERE "name" = 'free' AND NOT ("limits" ? 'max_tool_calls_per_month');

UPDATE "plans" SET "limits" = "limits" || '{"max_tool_calls_per_month": 20000}'::jsonb
WHERE "name" = 'pro' AND NOT ("limits" ? 'max_tool_calls_per_month');

UPDATE "plans" SET "limits" = "limits" || '{"max_tool_calls_per_month": -1}'::jsonb
WHERE "name" = 'enterprise' AND NOT ("limits" ? 'max_tool_calls_per_month');

-- +goose Down
-- Best-effort inverse: removes the key from the three named default plans.
-- Not a perfect inverse of Up (Up is conditional; this is not), matching
-- goose's own convention elsewhere in this tree of a Down that undoes the
-- shape of a migration rather than reconstructing exactly which rows Up's
-- WHERE clause touched.
UPDATE "plans" SET "limits" = "limits" - 'max_tool_calls_per_month'
WHERE "name" IN ('free', 'pro', 'enterprise');
