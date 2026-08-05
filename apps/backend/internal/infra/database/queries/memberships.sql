-- name: GetMembership :one
SELECT * FROM memberships
WHERE user_id = $1 AND organization_id = $2;

-- name: CreateMembership :one
INSERT INTO memberships (user_id, organization_id, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: CountMembershipsByOrg :one
SELECT count(*) FROM memberships WHERE organization_id = $1;

-- name: DeleteMembership :exec
DELETE FROM memberships
WHERE user_id = $1 AND organization_id = $2;

-- name: ListMembershipsByUser :many
SELECT
  m.id, m.user_id, m.organization_id, m.role, m.created_at,
  o.id   AS org_id,
  o.name AS org_name,
  o.slug AS org_slug,
  o.created_at AS org_created_at,
  o.updated_at AS org_updated_at
FROM memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.user_id = $1
ORDER BY m.created_at ASC;

-- name: ListOrganizationMembers :many
SELECT
  m.user_id,
  u.email,
  u.display_name,
  m.role,
  m.created_at AS joined_at
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.organization_id = $1
ORDER BY m.created_at ASC;
