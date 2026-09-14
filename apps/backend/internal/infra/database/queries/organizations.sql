-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = $1;

-- name: CreateOrganization :one
INSERT INTO organizations (name, slug)
VALUES ($1, $2)
RETURNING *;

-- name: GetOrganizationByID :one
-- The tenant-facing twin of AdminGetOrganizationByID (queries/admin.sql),
-- which is reachable only from the superadmin console. Added for
-- internal/module/billing, which names an org's lazily-created Stripe
-- Customer after the organization rather than after whichever member
-- happened to click "upgrade" first — a Stripe dashboard full of
-- personal names for company subscriptions is a support problem later.
-- Callers are already org-scoped by RequireOrg/RequirePermission before
-- this runs; it performs no authorization of its own.
SELECT * FROM organizations WHERE id = $1;
