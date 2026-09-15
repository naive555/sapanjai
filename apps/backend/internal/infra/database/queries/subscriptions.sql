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

-- name: GetOrgBillingSyncForUpdate :one
-- The webhook's (internal/module/billing, step 7) read of everything it
-- needs to decide whether an incoming Stripe event is newer than what is
-- already stored, taken FOR UPDATE.
--
-- FOR UPDATE, not a plain SELECT: two deliveries for the same organization
-- can be in flight at once (Stripe retries while the first attempt is still
-- running, or a subscription.updated and an invoice.paid arrive together).
-- Without the row lock both transactions would read the same watermark,
-- both would decide they are newer, and the older one could commit last.
-- The lock serializes them, so the "is this event stale" check and the
-- write that advances the watermark are one atomic step.
--
-- Deliberately does not select custom_limits or plans.limits: entitlement
-- resolution is subscription.Service.EffectiveLimits' job alone (plan
-- invariant 1), and this must not become a second answer to "what may this
-- org do".
SELECT organization_id, plan_id, stripe_customer_id, stripe_subscription_id,
       status, current_period_end, cancel_at_period_end, stripe_event_at
FROM org_subscriptions
WHERE organization_id = $1
FOR UPDATE;

-- name: FindOrgByStripeCustomerID :one
-- Maps a Stripe Customer back to its tenant. Backed by the partial unique
-- index idx_org_subscriptions_stripe_customer_id (migration 00013), so a
-- given customer id identifies exactly one organization -- which is what
-- makes it impossible for an event carrying org A's identifiers to land on
-- org B's row.
SELECT organization_id FROM org_subscriptions
WHERE stripe_customer_id = @stripe_customer_id::text;

-- name: FindOrgByStripeSubscriptionID :one
-- The subscription-id twin of FindOrgByStripeCustomerID, backed by
-- idx_org_subscriptions_stripe_subscription_id. Preferred over the customer
-- lookup when both are available: a Customer can in principle outlive and
-- outnumber its Subscriptions, while a Subscription belongs to exactly one.
SELECT organization_id FROM org_subscriptions
WHERE stripe_subscription_id = @stripe_subscription_id::text;

-- name: UpdateOrgStripeSubscription :exec
-- Writes the Stripe linkage columns of an org_subscriptions row that
-- already exists, plus the out-of-order watermark (stripe_event_at,
-- migration 00016).
--
-- An UPDATE, never an upsert, and it touches neither plan_id nor
-- custom_limits. Creating the row and moving plan_id is
-- subscription.Service.AssignPlan's job and nobody else's (plan invariant
-- 5); custom_limits is the admin override that must survive a billing event
-- (invariant 2). What is left -- customer id, subscription id, status,
-- period end, cancellation flag -- is billing's own bookkeeping, the same
-- ownership ClaimOrgStripeCustomer already has over stripe_customer_id.
--
-- Every value column is nullable-optional and COALESCEs to its current
-- value, because the handled events carry different subsets: a
-- checkout.session.completed knows the customer and subscription ids but
-- not the authoritative status (customer.subscription.created, arriving
-- alongside it, does), and an invoice.payment_failed knows the status but
-- not the period end. "Absent" must mean "leave alone", never "set to
-- NULL" -- otherwise each event would erase what the last one learned.
--
-- clear_subscription is the one exception, and it is plan decision 4:
-- "stripe_subscription_id IS NULL" is the canonical "not paying" signal, so
-- a cancellation CLEARS the column rather than leaving it pointing at a
-- dead Stripe object.
UPDATE org_subscriptions
SET
  stripe_customer_id = COALESCE(sqlc.narg('stripe_customer_id')::text, stripe_customer_id),
  stripe_subscription_id = CASE
    WHEN @clear_subscription::boolean THEN NULL
    ELSE COALESCE(sqlc.narg('stripe_subscription_id')::text, stripe_subscription_id)
  END,
  status = COALESCE(sqlc.narg('status')::text, status),
  current_period_end = COALESCE(sqlc.narg('current_period_end')::timestamp, current_period_end),
  cancel_at_period_end = COALESCE(sqlc.narg('cancel_at_period_end')::boolean, cancel_at_period_end),
  stripe_event_at = @stripe_event_at::timestamp,
  updated_at = now()
WHERE organization_id = @organization_id;
