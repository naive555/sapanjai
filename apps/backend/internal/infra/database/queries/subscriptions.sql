-- name: GetOrgSubscriptionWithPlan :one
SELECT s.custom_limits, p.limits AS plan_limits
FROM org_subscriptions s
JOIN plans p ON p.id = s.plan_id
WHERE s.organization_id = $1;

-- name: GetOrgSubscription :one
SELECT
  s.id, s.organization_id, s.plan_id, s.custom_limits, s.created_at, s.updated_at,
  p.id         AS plan_pid,
  p.name       AS plan_name,
  p.limits     AS plan_plimits,
  p.created_at AS plan_created_at
FROM org_subscriptions s
JOIN plans p ON p.id = s.plan_id
WHERE s.organization_id = $1;

-- name: UpsertOrgSubscription :exec
INSERT INTO org_subscriptions (organization_id, plan_id)
VALUES ($1, $2)
ON CONFLICT (organization_id)
DO UPDATE SET plan_id = EXCLUDED.plan_id, updated_at = now();

-- name: GetOrgBillingRef :one
-- The billing module's (internal/module/billing) narrow read of an org's
-- Stripe linkage. Deliberately separate from GetOrgSubscription, which
-- exists to serve GET /subscription and whose row shape the frontend and
-- internal/module/admin both depend on -- widening it with Stripe columns
-- would push billing state into every caller of the entitlement read path.
-- Note what is NOT selected: custom_limits and plans.limits. Entitlement
-- resolution stays subscription.Service.EffectiveLimits' job alone
-- (plan invariant 1), and nothing here is allowed to become a second
-- answer to "what may this org do".
SELECT organization_id, plan_id, stripe_customer_id, stripe_subscription_id, status
FROM org_subscriptions
WHERE organization_id = $1;

-- name: ClaimOrgStripeCustomer :one
-- Records the org's lazily-created Stripe Customer (plan decision 4), and
-- is the Postgres half of billing's no-duplicate-Customer guarantee: the
-- "stripe_customer_id IS NULL" predicate makes the claim conditional, so
-- of two concurrent checkout attempts exactly one UPDATE matches a row and
-- the other returns zero rows (pgx.ErrNoRows) and re-reads the winner's id.
--
-- Deliberately an UPDATE, never an upsert. Creating an org_subscriptions
-- row is subscription.Service.AssignPlan's job and nobody else's (plan
-- invariant 5); an org with no row yet resolves to "unlimited" through
-- EffectiveLimits, so inserting one here would silently change that org's
-- entitlements as a side effect of a billing click. Rows are created by
-- the webhook (step 7) when a subscription actually starts.
UPDATE org_subscriptions
SET stripe_customer_id = @stripe_customer_id::text, updated_at = now()
WHERE organization_id = @organization_id AND stripe_customer_id IS NULL
RETURNING stripe_customer_id;
