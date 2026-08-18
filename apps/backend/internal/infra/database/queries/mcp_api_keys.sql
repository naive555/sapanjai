-- name: CreateMCPKey :one
INSERT INTO mcp_api_keys (organization_id, user_id, name, key_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMCPKeyByName :one
SELECT * FROM mcp_api_keys WHERE organization_id = $1 AND name = $2;

-- name: ListMCPKeysByOrg :many
SELECT * FROM mcp_api_keys WHERE organization_id = $1 ORDER BY created_at ASC;

-- name: RevokeMCPKey :execrows
UPDATE mcp_api_keys SET revoked_at = now() WHERE id = $1 AND organization_id = $2;
