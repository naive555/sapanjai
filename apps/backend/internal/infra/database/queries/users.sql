-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name)
VALUES ($1, $2, $3)
RETURNING *;

-- name: MarkUserVerified :exec
UPDATE users SET is_verified = true, updated_at = now() WHERE id = $1;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- name: SetUserPlatformRole :exec
-- $2 is nullable: NULL revokes platform staff status (cmd/grantadmin's
-- "-role none"), 'superadmin'/'support' grants it. Enforced by the
-- users_platform_role_check CHECK constraint from migration 00011.
UPDATE users SET platform_role = $2, updated_at = now() WHERE id = $1;

-- name: SetUserBan :exec
-- Both $2 and $3 are nullable; an unban passes NULL/NULL. users.banned_at
-- is the durable source of truth behind the Redis banned:<userId> cache
-- (see internal/infra/redis/auth.go and internal/middleware.Guards.verify).
UPDATE users SET banned_at = $2, ban_reason = $3, updated_at = now() WHERE id = $1;

-- name: CountSuperadmins :one
SELECT count(*) FROM users WHERE platform_role = 'superadmin';
