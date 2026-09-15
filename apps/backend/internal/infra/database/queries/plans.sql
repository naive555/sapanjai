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

-- name: GetPlanByStripePriceID :one
-- Resolves the entitlement plan a Stripe Subscription is actually paying
-- for, from the Price id on its line item. This is the webhook's PRIMARY
-- plan resolution and metadata.plan_id is only the fallback, deliberately:
-- the Customer Portal lets a customer switch plans without this application
-- being involved, which changes the Price on the subscription but leaves
-- the plan_id this code stamped into metadata at checkout time frozen at
-- whatever they bought originally. Trusting metadata there would keep
-- billing them for the new plan while entitling them to the old one.
--
-- Not filtered on plan_prices.active: a plan re-priced after a customer
-- subscribed leaves that customer on the old, now-inactive Price, and their
-- renewal events must still resolve to the plan. `active` governs what may
-- be SOLD (GetActivePlanPrice), not what an existing subscription means.
SELECT p.* FROM plans p
JOIN plan_prices pp ON pp.plan_id = p.id
WHERE pp.stripe_price_id = @stripe_price_id::text;
