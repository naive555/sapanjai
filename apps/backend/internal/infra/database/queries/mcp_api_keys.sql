-- name: CreateMCPKey :one
-- scopes ($6) is nullable: a nil slice binds NULL (no independent
-- restriction, the key rides the creator's live grant), a non-empty slice
-- narrows it. mcpkey.Service.Create is responsible for never passing a
-- non-nil empty slice here — that would silently mint a key that permits
-- nothing.
INSERT INTO mcp_api_keys (organization_id, user_id, name, key_hash, expires_at, scopes)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMCPKeyByName :one
SELECT * FROM mcp_api_keys WHERE organization_id = $1 AND name = $2;

-- name: GetMCPKeyByHash :one
-- Looks up a presented PAT by its SHA-256 hash (internal/middleware.RequireMCPKey).
-- key_hash carries a unique index (migration 00008), so this is a single
-- indexed read — no Redis cache in front of it, per Decision 1.
--
-- Joined against users for owner_banned_at: an MCP PAT has no expiry of its
-- own, so a banned owner's key would otherwise keep authenticating forever
-- (docs/11-admin-panel.md §4). Extending this query rather than adding a
-- sibling keeps exactly one gateway auth path.
SELECT mcp_api_keys.*, users.banned_at AS owner_banned_at
FROM mcp_api_keys
JOIN users ON users.id = mcp_api_keys.user_id
WHERE mcp_api_keys.key_hash = $1;

-- name: ListMCPKeysByOrg :many
SELECT * FROM mcp_api_keys WHERE organization_id = $1 ORDER BY created_at ASC;

-- name: RevokeMCPKey :execrows
UPDATE mcp_api_keys SET revoked_at = now() WHERE id = $1 AND organization_id = $2;

-- name: StampMCPKeyLastUsed :exec
-- Best-effort bookkeeping: called after a successful RequireMCPKey
-- authentication. A failure to stamp must never fail the MCP request, so
-- the caller logs and swallows any error from this query.
UPDATE mcp_api_keys SET last_used_at = now() WHERE id = $1;
