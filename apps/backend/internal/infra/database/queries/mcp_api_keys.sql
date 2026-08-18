-- name: CreateMCPKey :one
INSERT INTO mcp_api_keys (organization_id, user_id, name, key_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetMCPKeyByName :one
SELECT * FROM mcp_api_keys WHERE organization_id = $1 AND name = $2;

-- name: GetMCPKeyByHash :one
-- Looks up a presented PAT by its SHA-256 hash (internal/middleware.RequireMCPKey).
-- key_hash carries a unique index (migration 00008), so this is a single
-- indexed read — no Redis cache in front of it, per Decision 1.
SELECT * FROM mcp_api_keys WHERE key_hash = $1;

-- name: ListMCPKeysByOrg :many
SELECT * FROM mcp_api_keys WHERE organization_id = $1 ORDER BY created_at ASC;

-- name: RevokeMCPKey :execrows
UPDATE mcp_api_keys SET revoked_at = now() WHERE id = $1 AND organization_id = $2;

-- name: StampMCPKeyLastUsed :exec
-- Best-effort bookkeeping: called after a successful RequireMCPKey
-- authentication. A failure to stamp must never fail the MCP request, so
-- the caller logs and swallows any error from this query.
UPDATE mcp_api_keys SET last_used_at = now() WHERE id = $1;
