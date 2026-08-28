-- Queries backing internal/module/admin, the cross-org platform staff
-- console (docs/11-admin-panel.md). Every query here deliberately has NO
-- organization_id predicate the way the tenant-facing queries do — that is
-- the whole point of /admin, not an oversight. See the module's own
-- doc comment for the authorization boundary (RequirePlatformRole, not
-- RequireOrg/RequirePermission).
--
-- None of these ever select connectors.encrypted_config or
-- mcp_api_keys.key_hash; column lists are explicit rather than SELECT *
-- specifically to keep it that way as the schema evolves.

-- name: AdminGetOrganizationByID :one
SELECT * FROM organizations WHERE id = $1;

-- name: AdminListOrganizations :many
-- search matches name or slug (case-insensitive substring). member/
-- connector/mcp-key counts are correlated subqueries rather than a
-- three-way JOIN + GROUP BY, which would multiply the plan row per
-- member*connector*key combination. plan_name is NULL for an org with no
-- subscription row.
SELECT
  o.id, o.name, o.slug, o.created_at,
  (SELECT count(*) FROM memberships m WHERE m.organization_id = o.id) AS member_count,
  (SELECT count(*) FROM connectors c WHERE c.organization_id = o.id) AS connector_count,
  (SELECT count(*) FROM mcp_api_keys k WHERE k.organization_id = o.id) AS mcp_key_count,
  p.name AS plan_name
FROM organizations o
LEFT JOIN org_subscriptions s ON s.organization_id = o.id
LEFT JOIN plans p ON p.id = s.plan_id
WHERE (sqlc.narg('search')::text IS NULL
       OR o.name ILIKE '%' || sqlc.narg('search')::text || '%'
       OR o.slug ILIKE '%' || sqlc.narg('search')::text || '%')
ORDER BY o.created_at ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: AdminCountOrganizations :one
-- Mirrors AdminListOrganizations's WHERE exactly — the pair behind
-- admin.Service.cachedCount for GET /admin/organizations.
SELECT count(*) FROM organizations o
WHERE (sqlc.narg('search')::text IS NULL
       OR o.name ILIKE '%' || sqlc.narg('search')::text || '%'
       OR o.slug ILIKE '%' || sqlc.narg('search')::text || '%');

-- name: AdminListUsers :many
-- search matches email or display_name. role is nullable text taking
-- 'superadmin', 'support', 'none' (meaning platform_role IS NULL), or NULL
-- (no filter) — a single text param rather than a separate bool so the
-- three-way choice stays one WHERE clause. banned is a nullable bool.
SELECT
  u.id, u.email, u.display_name, u.is_verified, u.platform_role, u.banned_at, u.created_at,
  (SELECT count(*) FROM memberships m WHERE m.user_id = u.id) AS org_count
FROM users u
WHERE (sqlc.narg('search')::text IS NULL
       OR u.email ILIKE '%' || sqlc.narg('search')::text || '%'
       OR u.display_name ILIKE '%' || sqlc.narg('search')::text || '%')
  AND (sqlc.narg('role')::text IS NULL
       OR (sqlc.narg('role')::text = 'none' AND u.platform_role IS NULL)
       OR (sqlc.narg('role')::text <> 'none' AND u.platform_role = sqlc.narg('role')::text))
  AND (sqlc.narg('banned')::bool IS NULL
       OR (sqlc.narg('banned')::bool AND u.banned_at IS NOT NULL)
       OR (NOT sqlc.narg('banned')::bool AND u.banned_at IS NULL))
ORDER BY u.created_at ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: AdminCountUsers :one
-- Mirrors AdminListUsers's WHERE exactly.
SELECT count(*) FROM users u
WHERE (sqlc.narg('search')::text IS NULL
       OR u.email ILIKE '%' || sqlc.narg('search')::text || '%'
       OR u.display_name ILIKE '%' || sqlc.narg('search')::text || '%')
  AND (sqlc.narg('role')::text IS NULL
       OR (sqlc.narg('role')::text = 'none' AND u.platform_role IS NULL)
       OR (sqlc.narg('role')::text <> 'none' AND u.platform_role = sqlc.narg('role')::text))
  AND (sqlc.narg('banned')::bool IS NULL
       OR (sqlc.narg('banned')::bool AND u.banned_at IS NOT NULL)
       OR (NOT sqlc.narg('banned')::bool AND u.banned_at IS NULL));

-- name: AdminCountActiveSessionsByUser :one
SELECT count(*) FROM sessions
WHERE user_id = $1 AND is_revoked = false AND expires_at > now();

-- name: AdminListConnectors :many
-- Cross-org connector metadata only — no encrypted_config column in this
-- SELECT, ever (docs/11-admin-panel.md §7). Also used, filtered by
-- organization_id alone, to populate the connector list nested in
-- GET /admin/organizations/:orgId (admin.Service.OrganizationDetail).
SELECT
  c.id, c.organization_id, o.name AS organization_name, c.name, c.type, c.status,
  c.last_health_check_at, c.created_at
FROM connectors c
JOIN organizations o ON o.id = c.organization_id
WHERE (sqlc.narg('organization_id')::uuid IS NULL OR c.organization_id = sqlc.narg('organization_id')::uuid)
  AND (sqlc.narg('type')::text IS NULL OR c.type = sqlc.narg('type')::text)
  AND (sqlc.narg('status')::text IS NULL OR c.status = sqlc.narg('status')::text)
  AND (sqlc.narg('search')::text IS NULL OR c.name ILIKE '%' || sqlc.narg('search')::text || '%')
ORDER BY c.created_at ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: AdminCountConnectors :one
-- Mirrors AdminListConnectors's WHERE exactly.
SELECT count(*) FROM connectors c
WHERE (sqlc.narg('organization_id')::uuid IS NULL OR c.organization_id = sqlc.narg('organization_id')::uuid)
  AND (sqlc.narg('type')::text IS NULL OR c.type = sqlc.narg('type')::text)
  AND (sqlc.narg('status')::text IS NULL OR c.status = sqlc.narg('status')::text)
  AND (sqlc.narg('search')::text IS NULL OR c.name ILIKE '%' || sqlc.narg('search')::text || '%');

-- name: AdminListMCPKeys :many
-- Cross-org MCP key metadata only — no key_hash column in this SELECT,
-- ever (docs/11-admin-panel.md §7). search matches the key's own name or
-- its owner's email. Also used, filtered by organization_id alone, to
-- populate the MCP key list nested in GET /admin/organizations/:orgId.
SELECT
  k.id, k.organization_id, o.name AS organization_name, k.user_id, u.email AS user_email,
  k.name, k.scopes, k.last_used_at, k.expires_at, k.revoked_at, k.created_at
FROM mcp_api_keys k
JOIN organizations o ON o.id = k.organization_id
JOIN users u ON u.id = k.user_id
WHERE (sqlc.narg('organization_id')::uuid IS NULL OR k.organization_id = sqlc.narg('organization_id')::uuid)
  AND (sqlc.narg('user_id')::uuid IS NULL OR k.user_id = sqlc.narg('user_id')::uuid)
  AND (sqlc.narg('search')::text IS NULL
       OR k.name ILIKE '%' || sqlc.narg('search')::text || '%'
       OR u.email ILIKE '%' || sqlc.narg('search')::text || '%')
ORDER BY k.created_at ASC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: AdminCountMCPKeys :one
-- Mirrors AdminListMCPKeys's WHERE exactly.
SELECT count(*) FROM mcp_api_keys k
JOIN users u ON u.id = k.user_id
WHERE (sqlc.narg('organization_id')::uuid IS NULL OR k.organization_id = sqlc.narg('organization_id')::uuid)
  AND (sqlc.narg('user_id')::uuid IS NULL OR k.user_id = sqlc.narg('user_id')::uuid)
  AND (sqlc.narg('search')::text IS NULL
       OR k.name ILIKE '%' || sqlc.narg('search')::text || '%'
       OR u.email ILIKE '%' || sqlc.narg('search')::text || '%');

-- name: AdminQueryAuditLogs :many
-- Cross-org: unlike QueryAuditLogs (internal/infra/database/queries/auditlog.sql),
-- which mandates organization_id as a tenant-isolation guarantee, this one
-- deliberately carries no such predicate — see admin.sql's file header.
-- Two queries, two guarantees; do not widen QueryAuditLogs to cover this.
--
-- action_patterns is a nullable text[] of LIKE patterns: the handler turns
-- a bare action into a literal (with '%'/'_' escaped) and a trailing '*'
-- into a '<prefix>%' pattern, so "admin.*" matches by prefix while an exact
-- action matches by equality (LIKE with no wildcard characters behaves as
-- equality). `LIKE ANY (array)` matches if any pattern in the array
-- matches.
--
-- from/to are nullable naive timestamps — audit_logs.created_at has no time
-- zone (see QueryAuditLogs's comment); callers must normalize any RFC3339
-- input to UTC before binding here, same as there.
SELECT
  a.id, a.organization_id, a.user_id, a.action, a.metadata, a.created_at,
  o.name AS organization_name, u.email AS user_email
FROM audit_logs a
LEFT JOIN organizations o ON o.id = a.organization_id
LEFT JOIN users u ON u.id = a.user_id
WHERE (sqlc.narg('organization_id')::uuid IS NULL OR a.organization_id = sqlc.narg('organization_id')::uuid)
  AND (sqlc.narg('user_id')::uuid IS NULL OR a.user_id = sqlc.narg('user_id')::uuid)
  AND (sqlc.narg('action_patterns')::text[] IS NULL OR a.action LIKE ANY(sqlc.narg('action_patterns')::text[]))
  AND (sqlc.narg('from')::timestamp IS NULL OR a.created_at >= sqlc.narg('from')::timestamp)
  AND (sqlc.narg('to')::timestamp IS NULL OR a.created_at <= sqlc.narg('to')::timestamp)
ORDER BY a.created_at DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: AdminCountAuditLogs :one
-- Mirrors AdminQueryAuditLogs's WHERE exactly.
SELECT count(*) FROM audit_logs a
WHERE (sqlc.narg('organization_id')::uuid IS NULL OR a.organization_id = sqlc.narg('organization_id')::uuid)
  AND (sqlc.narg('user_id')::uuid IS NULL OR a.user_id = sqlc.narg('user_id')::uuid)
  AND (sqlc.narg('action_patterns')::text[] IS NULL OR a.action LIKE ANY(sqlc.narg('action_patterns')::text[]))
  AND (sqlc.narg('from')::timestamp IS NULL OR a.created_at >= sqlc.narg('from')::timestamp)
  AND (sqlc.narg('to')::timestamp IS NULL OR a.created_at <= sqlc.narg('to')::timestamp);

-- The remaining queries back GET /admin/system/stats. Each is cached
-- individually under its own fixed filter key (admin.Service.SystemStats) —
-- there is no user-supplied filter, but they still go through
-- admin.Service.cachedCount for the same "don't COUNT(*) on every staff
-- page load" reason as the paged lists above.

-- name: AdminCountAllOrganizations :one
SELECT count(*) FROM organizations;

-- name: AdminCountAllUsers :one
SELECT count(*) FROM users;

-- name: AdminCountAllConnectors :one
SELECT count(*) FROM connectors;

-- name: AdminCountAllMCPKeys :one
SELECT count(*) FROM mcp_api_keys;

-- name: AdminCountActiveMCPKeys :one
SELECT count(*) FROM mcp_api_keys
WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now());

-- name: AdminCountActiveSessions :one
SELECT count(*) FROM sessions WHERE is_revoked = false AND expires_at > now();

-- name: AdminCountAllAuditLogs :one
SELECT count(*) FROM audit_logs;

-- name: AdminCountEmailOutboxByStatus :many
-- A rising 'failed' count is the single best early warning that Resend or
-- the EMAIL_FROM domain is misconfigured (CLAUDE.md's Background worker
-- bullet).
SELECT status, count(*) AS status_count FROM email_outbox GROUP BY status;

-- name: AdminCountUsersSince :one
SELECT count(*) FROM users WHERE created_at >= sqlc.arg('since');

-- name: AdminCountOrganizationsSince :one
SELECT count(*) FROM organizations WHERE created_at >= sqlc.arg('since');

-- name: AdminPlanBreakdown :many
-- LEFT JOIN so a plan with zero subscribers still shows a 0 row rather
-- than disappearing from the breakdown entirely.
SELECT p.name AS plan_name, count(s.id) AS org_count
FROM plans p
LEFT JOIN org_subscriptions s ON s.plan_id = p.id
GROUP BY p.name
ORDER BY p.name ASC;
