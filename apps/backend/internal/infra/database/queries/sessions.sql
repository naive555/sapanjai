-- name: CreateSession :one
INSERT INTO sessions (user_id, refresh_token, family, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetSessionByRefreshToken :one
SELECT * FROM sessions WHERE refresh_token = $1;

-- name: RevokeSessionByID :exec
UPDATE sessions SET is_revoked = true WHERE id = $1;

-- name: RevokeSessionFamily :exec
UPDATE sessions SET is_revoked = true WHERE family = $1;

-- name: RevokeAllUserSessions :exec
UPDATE sessions SET is_revoked = true
WHERE user_id = $1 AND is_revoked = false;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions
WHERE id IN (
    SELECT id FROM sessions
    WHERE expires_at < now()
       OR (is_revoked = true AND created_at < now() - (sqlc.arg(retention_seconds)::int * INTERVAL '1 second'))
    LIMIT sqlc.arg(batch_size)
);
