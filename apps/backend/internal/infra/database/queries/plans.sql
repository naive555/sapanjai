-- name: UpsertPlan :exec
INSERT INTO plans (name, limits)
VALUES ($1, $2)
ON CONFLICT (name) DO NOTHING;

-- name: GetPlanByName :one
SELECT * FROM plans WHERE name = $1;

-- name: ListPlans :many
SELECT * FROM plans ORDER BY created_at ASC;
