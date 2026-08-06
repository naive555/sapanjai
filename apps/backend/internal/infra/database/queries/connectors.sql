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
