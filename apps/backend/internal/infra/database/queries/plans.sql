-- name: UpsertPlan :exec
INSERT INTO plans (name, limits)
VALUES ($1, $2)
ON CONFLICT (name) DO NOTHING;

-- name: GetPlanByName :one
SELECT * FROM plans WHERE name = $1;

-- name: ListPlans :many
SELECT * FROM plans ORDER BY created_at ASC;

-- name: GetPlanByID :one
-- The tenant-facing twin of AdminGetPlanByID (queries/admin.sql), which is
-- reachable only from the superadmin console. internal/module/billing needs
-- to resolve the plan a checkout is for -- and to check is_public before
-- selling it -- without reaching into an Admin*-prefixed query it is not
-- entitled to use.
SELECT * FROM plans WHERE id = $1;

-- name: GetActivePlanPrice :one
-- Resolves the one Stripe Price a checkout should charge. Per plan decision
-- 3 there is exactly one active THB monthly Price per plan today; currency
-- and interval are parameters rather than constants so a second currency is
-- a plan_prices row plus a Stripe Price, not a migration and not a query
-- change. Newest-first so re-pricing a plan is "insert the new row, then
-- deactivate the old one", with no window in which neither is selectable.
SELECT * FROM plan_prices
WHERE plan_id = @plan_id
  AND currency = @currency
  AND "interval" = @billing_interval
  AND active = true
ORDER BY created_at DESC
LIMIT 1;
