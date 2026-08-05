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
