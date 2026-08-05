# Target Architecture — junctera monorepo (Go + Next.js)

> Stack decisions confirmed with the project owner on 2026-07-18:
> **Backend: Go + Echo · sqlc + pgx · goose migrations. Frontend: Next.js (App Router) + shadcn/ui + Tailwind. Glue: root Makefile + docker-compose.**

## Why these choices

| Decision | Choice | Rationale |
| -------- | ------ | --------- |
| Web framework | **Echo** | Centralized `HTTPErrorHandler` maps 1:1 to the source's ERROR_MAP/globalErrorHandler pattern; built-in request-id, logging, and recover middleware cover the Elysia hooks; net/http compatible; mature. |
| DB layer | **sqlc + pgx/v5** | Hand-written SQL → generated type-safe Go; closest to Drizzle's type-safety without ORM magic; the existing plain-SQL migrations are reusable. |
| Migrations | **goose** | Runs the existing `.sql` files with minimal annotation changes (`-- +goose Up`). |
| Redis | **go-redis/v9** | Standard client; mirrors ioredis usage directly. |
| Validation | **go-playground/validator** (via Echo binder) | Struct tags replace TypeBox schemas. |
| JWT | **golang-jwt/jwt/v5** | HS256, same claims/secrets as source. |
| Logging | **log/slog** (JSON in prod, text/tint in dev) | stdlib; pino redaction rules re-implemented as a slog ReplaceAttr/middleware concern. |
| API docs | **swaggo/echo-swagger** (or hand-written OpenAPI 3 YAML served by Swagger UI) | Replaces @elysiajs/swagger at `/swagger`. |
| Frontend | **Next.js App Router + TypeScript + Tailwind + shadcn/ui + TanStack Query** | Mainstream B2B dashboard stack; shadcn gives tables/forms/dialogs for org/RBAC/audit UIs quickly. |
| Monorepo glue | **Makefile + docker-compose** | Language-neutral one-command dev (`make dev`), test, migrate targets. |

## Monorepo layout

```
junctera/
├── CLAUDE.md
├── Makefile                  # dev, test, lint, migrate, seed, sqlc, build, up/down
├── compose.yaml              # db, redis, api, web
├── docs/                     # ← these planning documents
├── apps/
│   ├── backend/
│   │   ├── cmd/api/main.go       # bootstrap: config → redis ping → db ping → server; graceful shutdown
│   │   ├── cmd/worker/main.go    # background job runner (scheduler + /health on WORKER_PORT)
│   │   ├── internal/
│   │   │   ├── config/           # env loading + validation (replaces process.env access)
│   │   │   ├── server/           # Echo setup, route mounting, error handler, middleware wiring
│   │   │   ├── middleware/       # requestid, auth (RequireAuth/RequireOrg/RequirePermission), logger
│   │   │   ├── module/           # mirrors src/modules/*
│   │   │   │   ├── auth/         #   handler.go, service.go, dto.go
│   │   │   │   ├── organization/ #   handler.go, service.go, repository = sqlc queries
│   │   │   │   ├── rbac/
│   │   │   │   ├── auditlog/
│   │   │   │   ├── subscription/
│   │   │   │   └── health/
│   │   │   ├── worker/           # Job interface, registry, interval scheduler, redis lock
│   │   │   ├── job/               # job implementations (sessioncleanup, …)
│   │   │   ├── domain/           # extension point (mirrors src/domains/)
│   │   │   ├── infra/
│   │   │   │   ├── database/     # pgx pool, sqlc generated code (db/), queries/ (*.sql)
│   │   │   │   └── redis/        # client + RedisAuth helpers (blacklist, login attempts)
│   │   │   └── shared/
│   │   │       ├── apperror/     # typed error codes → HTTP mapping (replaces ERROR_MAP)
│   │   │       └── logger/       # slog setup, redaction
│   │   ├── migrations/           # goose SQL (ported from drizzle migrations)
│   │   ├── sqlc.yaml
│   │   ├── go.mod
│   │   └── Dockerfile            # multi-stage: golang builder → distroless/alpine runner (api/worker/healthcheck)
│   └── frontend/
│       ├── app/                  # Next.js App Router
│       │   ├── (auth)/login, register
│       │   └── (dashboard)/orgs, members, roles, audit-logs, subscription, settings
│       ├── components/           # shadcn/ui
│       ├── lib/api/              # typed API client (fetch wrapper w/ token refresh, x-organization-id)
│       ├── package.json
│       └── Dockerfile
├── k8s/                      # ported manifests: api image → Go binary, web Deployment, worker Deployment
└── .github/workflows/        # ci.yml: backend job (go test w/ services), frontend job (lint+build)
```

## Backend architecture notes

- **Handlers ↔ services ↔ sqlc queries** mirror the source's controller/service/repository convention. Services return typed `apperror.Error{Code}` values; the Echo `HTTPErrorHandler` maps codes → status/message per the contract table.
- **Auth middleware** replaces Elysia macros. Inject `user`, `organizationId`, `membership` into `echo.Context` (or a typed request context). Three constructors: `RequireAuth()`, `RequireOrg()`, `RequirePermission(action string)`.
- **No dynamic-import hacks**: RBAC/Org service dependencies are plain constructor injection.
- **DB**: one `pgxpool.Pool`; sqlc queries per module in `infra/database/queries/*.sql`. Transactions where the source did multi-step writes (org create + owner membership; session rotation).
- **Graceful shutdown**: `signal.NotifyContext` + `echo.Shutdown(ctx)` + pool/redis close — mirrors source SIGTERM/SIGINT handling.
- **Config**: fail fast at boot if required env is missing (source only checked REDIS_URL; improve to validate all, incl. 32-char secret minimum).
- **Background jobs**: `cmd/worker` is a separate process sharing the API's config/store/logger. Jobs implement `worker.Job` (`Name`/`Interval`/`Run`) and are registered in `cmd/worker/main.go`. The scheduler adds per-run timeouts, panic recovery, structured logging, and a Redis lock (`worker:lock:<job>`) held for ~one interval so a multi-replica fleet runs each job about once per interval. Failed runs release the lock immediately so the next tick retries. The guarantee jobs may rely on is therefore **at most one run per interval fleet-wide**, not merely "no two runs at once" — chosen so non-idempotent future jobs (mail, billing, webhooks) are safe to register without re-deriving the coordination story. Job outcomes are exposed on the worker's internal `GET /health` (`WORKER_PORT`, default 3001) — not part of the public API contract.

## Frontend scope (initial dashboard)

Pages that exercise every backend feature:
1. **Auth**: register, login, token refresh (silent), logout.
2. **Org switcher**: list my orgs, create org (sets `x-organization-id` for subsequent calls).
3. **Members**: list (needs a backend list-members endpoint — source lacks one; add or derive), invite, remove.
4. **Roles & permissions**: list/create roles, edit permission sets, assign roles to members.
5. **Audit log viewer**: filterable table (user, action, limit).
6. **Subscription**: current plan + limits display, assign plan.

API client concerns: attach Bearer token, auto-refresh on 401 (single-flight), attach `x-organization-id` from active-org state, surface `x-request-id` in error toasts.

## Dev experience (Makefile targets)

```
make up          # docker compose up db redis -d
make dev         # backend (air hot-reload) + frontend (next dev) concurrently
make migrate     # goose up
make seed        # seed default plans (free/pro/enterprise)
make sqlc        # regenerate query code
make test        # go test ./... + frontend tests
make lint        # golangci-lint + eslint
make build       # both artifacts / docker images
```

## Open questions for the planning model (Opus)

1. Should `POST /subscription/assign` gain an admin/permission guard (fixing the source oversight) or preserve bug-for-bug parity? (Recommend: fix, document as intentional deviation.)
2. ~~Add a `GET /organizations/members` list endpoint for the frontend, or keep strict parity and derive from audit logs?~~ **Resolved in Phase 6**: added. See "Deviations resolved during Phase 6" below.
3. Hash refresh tokens at rest (source stores raw JWT)? (Recommend: yes if deviation allowed; keep raw for parity otherwise.)
4. Testing depth: source has unit tests with mocked infra; Go port should decide unit (mock interfaces) vs. integration (testcontainers / CI service containers, matching source CI).
5. Whether the access-token blacklist TTL should follow `JWT_ACCESS_EXPIRES_IN` instead of hard-coded 15 min.

## Deviations resolved during Phase 4 (RBAC + subscription + audit query)

Three intentional deviations from `../controlplane-api/src/modules/{rbac,subscription}`, decided with the owner and implemented — see `.claude/plans/archives/2026-07-20-phase-4-rbac-subscription-audit.md` for full rationale:

1. **`RBACService.createRole` transactionality.** Source runs the role insert and `setPermissions` as two separate, non-atomic awaits, so a mid-write crash can leave a role with zero permissions. The Go port wraps both (and `updatePermissions`'s delete+insert) in one `store.WithTx`, per this file's "transactions where the source did multi-step writes" note and `CLAUDE.md`'s "multi-step writes run in transactions" rule.
2. **Malformed uuids in RBAC/subscription bodies.** Source passes the raw string straight to a Drizzle query, so an invalid uuid throws and surfaces as a 500. The Go port validates `userId`/`roleId`/`planId` body fields with `validate:"required,uuid"`, returning 422 "Validation failed" instead. (A malformed `:roleId` **path** param — no body to validate — still maps to 404 "Role not found", consistent with how the org module treats a malformed `:userId` path param as 404 "Member not found".)
3. **`POST /subscription/assign` response body.** Source returns an empty 200 body (`void`). The Go port returns `{"success":true}`, matching the response shape of every other write endpoint (`invite`, `removeMember`, `rbac/assign`) for API consistency.

Not treated as deviations (kept bug-for-bug): duplicate actions in a `permissions[]` array still hit the `(role_id, action)` unique constraint and surface as 500, same as source; a well-formed but nonexistent `planId` still fails at the plan foreign key (500) rather than a pre-checked `404`, since the contract has no `PLAN_NOT_FOUND` code.

## Deviations resolved during Phase 6 (frontend)

1. **New `GET /organizations/members` endpoint.** The source app has no member-
   roster endpoint (Open question #2, above); the frontend members page needs
   one, and deriving a live roster from audit logs was rejected as fragile
   (audit logs record invite/remove *events*, not current membership state,
   and aren't guaranteed complete — `role.created`/`role.assigned` are defined
   but not written). Added `GET /organizations/members` (org-guarded, same
   guard level as `invite`/`removeMember`): returns
   `[{ userId, email, displayName, role, joinedAt }]` for the active org,
   ordered by membership creation time. See `docs/02-api-contract.md`
   Organizations table and `.claude/plans/plan.md` Step 1 for the full
   implementation (sqlc query `ListOrganizationMembers` joining `memberships`
   + `users`).
2. **Frontend token/networking model.** The browser never talks to the Go API
   directly. Next.js `next.config.ts` proxies `/api/:path*` → `BACKEND_URL`
   via `rewrites()`, so all frontend requests are same-origin and **no CORS
   middleware was added to the backend**. Access token is held in memory
   (browser tab only); refresh token in `localStorage`; the API client
   single-flights concurrent 401s through one `/auth/refresh` call and retries
   the original request once. This was chosen over a cookie-based BFF (more
   secure against XSS, but substantially more implementation for this phase)
   and over direct cross-origin calls (would require adding + maintaining
   CORS policy on the backend for no behavioral benefit, since the frontend
   and backend are always deployed together here).
3. **New `GET /plans` endpoint.** The Phase 6 plan's original Step 10 draft
   assumed the frontend could hardcode the seeded plans' ids, but
   `plans.id` is `uuid PRIMARY KEY DEFAULT gen_random_uuid()`
   (`migrations/00005_plans_org_subscriptions.sql`) — genuinely random per
   database, not stable across dev/CI/prod. With no `GET /plans` endpoint,
   the frontend had no way to discover any plan id at all unless an org
   already had one assigned (which only reveals the *current* plan, not the
   other choices). Confirmed with the owner (three options: add the
   endpoint, a raw-UUID text input, or a view-only page with no assign
   flow) — decided to add `GET /plans` (`RequireAuth` only; plans are
   global, not org-scoped): returns `[{ id, name, limits, createdAt }]`.
   See `docs/02-api-contract.md` Subscription table and
   `.claude/plans/plan.md` Step 10.

## Deviations resolved during Phase 7 (background worker)

1. **New indexes on `sessions` (migration `00006`).** `CLAUDE.md` requires
   the schema to stay byte-identical to the source app's Drizzle
   migrations. The session-cleanup job scans by `expires_at` and by
   `created_at` for revoked rows, neither of which the source schema
   indexes (the source has no cleanup job at all). Agreed with the owner
   on 2026-07-31: add `idx_sessions_expires_at` and a partial
   `idx_sessions_revoked_created_at`. Indexes only — no table, column, or
   constraint differs, so a source-app database snapshot still runs
   unchanged under the Go app and vice versa.
2. **Revoked-session retention window.** Revoked-but-unexpired sessions are
   kept for `SESSION_CLEANUP_RETENTION` (default 30d) rather than deleted
   on revocation, so refresh-token reuse detection still recognises a
   replayed token as a family reuse instead of an unknown session.
