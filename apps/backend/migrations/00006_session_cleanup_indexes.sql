-- +goose Up
CREATE INDEX IF NOT EXISTS "idx_sessions_expires_at" ON "sessions" ("expires_at");
CREATE INDEX IF NOT EXISTS "idx_sessions_revoked_created_at" ON "sessions" ("created_at") WHERE "is_revoked" = true;

-- +goose Down
DROP INDEX IF EXISTS "idx_sessions_revoked_created_at";
DROP INDEX IF EXISTS "idx_sessions_expires_at";
