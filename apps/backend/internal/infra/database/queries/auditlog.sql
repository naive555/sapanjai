-- name: CreateAuditLog :exec
INSERT INTO audit_logs (organization_id, user_id, action, metadata)
VALUES ($1, $2, $3, $4);

-- name: QueryAuditLogs :many
SELECT * FROM audit_logs
WHERE organization_id = sqlc.arg('organization_id')
  AND (sqlc.narg('user_id')::uuid IS NULL OR user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action'))
ORDER BY created_at DESC
LIMIT sqlc.arg('lim');
