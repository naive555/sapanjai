-- name: CreateConnector :one
INSERT INTO connectors (organization_id, name, type, encrypted_config)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetConnector :one
SELECT * FROM connectors WHERE id = $1 AND organization_id = $2;

-- name: GetConnectorByName :one
SELECT * FROM connectors WHERE organization_id = $1 AND name = $2;

-- name: ListConnectorsByOrg :many
SELECT * FROM connectors WHERE organization_id = $1 ORDER BY created_at ASC;

-- name: CountConnectorsByOrg :one
SELECT count(*) FROM connectors WHERE organization_id = $1;

-- name: UpdateConnector :one
UPDATE connectors
SET name = $3, status = $4, encrypted_config = $5, updated_at = now()
WHERE id = $1 AND organization_id = $2
RETURNING *;

-- name: UpdateConnectorHealth :one
UPDATE connectors
SET status = $3, last_health_check_at = now(), updated_at = now()
WHERE id = $1 AND organization_id = $2
RETURNING *;

-- name: DeleteConnector :execrows
DELETE FROM connectors WHERE id = $1 AND organization_id = $2;

-- name: ListConnectorsForHealthCheck :many
-- Cross-org sweep for the connector-health background job
-- (internal/job/connectorhealth) -- deliberately not organization-scoped,
-- unlike every other query in this file. encrypted_config is NOT selected:
-- the job re-reads and decrypts each connector through connector.Service so
-- decrypted config never leaves the service that owns it (CLAUDE.md).
-- Ordered oldest-checked-first (nulls, i.e. never checked, first) so a
-- backlog drains in the order it went stale rather than round-robining.
SELECT id, organization_id, type, status
FROM connectors
ORDER BY last_health_check_at ASC NULLS FIRST
LIMIT sqlc.arg(batch_size);
