-- +goose Up
-- GET /audit-logs is WHERE organization_id = … ORDER BY created_at DESC
-- LIMIT …, and audit_logs had no indexes at all beyond the primary key.
-- EXPLAIN ANALYZE against 65,000 seeded rows across 25 organizations (one
-- "hot" org holding 5,000 of them) showed a sequential scan touching 870
-- buffers and 1.6ms execution time without this index, vs an index scan
-- touching 52 buffers and 0.03ms with it — see the Phase 4 implementation
-- report for both full plans. DESC on created_at matches the query's
-- ORDER BY so the index can satisfy both the equality filter and the sort
-- without an extra Sort node.
CREATE INDEX IF NOT EXISTS "idx_audit_logs_organization_id_created_at" ON "audit_logs" ("organization_id", "created_at" DESC);

-- +goose Down
DROP INDEX IF EXISTS "idx_audit_logs_organization_id_created_at";
