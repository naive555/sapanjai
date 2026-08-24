-- name: CreateAuditLog :exec
INSERT INTO audit_logs (organization_id, user_id, action, metadata)
VALUES ($1, $2, $3, $4);

-- name: QueryAuditLogs :many
-- actions is a nullable text[]: NULL (no ?action= given at all) matches
-- every row, same as before repeatable action filtering was added. An
-- empty (non-NULL) array must never reach this query — `action = ANY('{}')`
-- is always false and would silently return zero rows instead of
-- "unfiltered" — so callers must pass a nil slice, not an empty one, when
-- no action filter applies; see auditlog.Service.Query.
--
-- since is a nullable timestamp (audit_logs.created_at has no time zone),
-- compared with >= so a since value equal to a row's created_at includes
-- that row (inclusive lower bound). Callers must normalize any RFC3339
-- input to UTC before binding it here, since the column stores naive
-- UTC wall-clock values.
SELECT * FROM audit_logs
WHERE organization_id = sqlc.arg('organization_id')
  AND (sqlc.narg('user_id')::uuid IS NULL OR user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('actions')::text[] IS NULL OR action = ANY(sqlc.narg('actions')::text[]))
  AND (sqlc.narg('since')::timestamp IS NULL OR created_at >= sqlc.narg('since')::timestamp)
ORDER BY created_at DESC
LIMIT sqlc.arg('lim');
