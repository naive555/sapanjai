-- +goose Up
ALTER TABLE "users" ADD COLUMN "platform_role" text;
ALTER TABLE "users" ADD CONSTRAINT "users_platform_role_check"
  CHECK ("platform_role" IS NULL OR "platform_role" IN ('superadmin', 'support'));
ALTER TABLE "users" ADD COLUMN "banned_at" timestamp;
ALTER TABLE "users" ADD COLUMN "ban_reason" text;
CREATE INDEX IF NOT EXISTS "idx_users_platform_role"
  ON "users" ("platform_role") WHERE "platform_role" IS NOT NULL;
-- Cross-org audit queries have no organization_id predicate, so the
-- (organization_id, created_at) index from 00009 cannot serve them.
CREATE INDEX IF NOT EXISTS "idx_audit_logs_created_at" ON "audit_logs" ("created_at" DESC);

-- +goose Down
DROP INDEX IF EXISTS "idx_audit_logs_created_at";
DROP INDEX IF EXISTS "idx_users_platform_role";
ALTER TABLE "users" DROP COLUMN "ban_reason";
ALTER TABLE "users" DROP COLUMN "banned_at";
ALTER TABLE "users" DROP CONSTRAINT "users_platform_role_check";
ALTER TABLE "users" DROP COLUMN "platform_role";
