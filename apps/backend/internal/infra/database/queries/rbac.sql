-- name: CreateRole :one
INSERT INTO roles (organization_id, name, description)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRoleByID :one
SELECT * FROM roles WHERE id = $1;

-- name: ListRolesByOrg :many
SELECT * FROM roles
WHERE organization_id = $1
ORDER BY created_at ASC;

-- name: DeletePermissionsByRole :exec
DELETE FROM permissions WHERE role_id = $1;

-- name: CreatePermission :exec
INSERT INTO permissions (role_id, action)
VALUES ($1, $2);

-- name: ListPermissionsByRoleIDs :many
SELECT * FROM permissions
WHERE role_id = ANY($1::uuid[])
ORDER BY created_at ASC;

-- name: AssignMemberRole :exec
INSERT INTO member_roles (membership_id, role_id)
VALUES ($1, $2)
ON CONFLICT (membership_id, role_id) DO NOTHING;

-- name: ListPermissionActionsByUserOrg :many
SELECT DISTINCT p.action
FROM memberships m
JOIN member_roles mr ON mr.membership_id = m.id
JOIN roles r         ON r.id = mr.role_id
JOIN permissions p   ON p.role_id = r.id
WHERE m.user_id = $1 AND m.organization_id = $2;
