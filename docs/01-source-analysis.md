# Source Analysis — controlplane-api (Node/Bun original)

> Source location: `c:/Users/non_n/Desktop/Jobs/challenges/controlplane-api`
> This document describes the existing system that is being migrated to Go. Read this before planning any migration work.

## What it is

A **multi-tenant B2B SaaS platform backend template**: auth, organization management, RBAC, audit logging, and subscription/plan limit enforcement as a reusable core. Business domains are meant to be added on top (`src/domains/` is intentionally empty).

## Tech stack (source)

| Concern    | Technology                                            |
| ---------- | ----------------------------------------------------- |
| Runtime    | Bun >= 1.3                                            |
| Framework  | ElysiaJS (plugin/macro architecture)                  |
| Database   | PostgreSQL 16 + Drizzle ORM (SQL migrations via drizzle-kit) |
| Cache      | Redis 7 (ioredis) — token blacklist + login rate limiting |
| Validation | TypeBox schemas (Elysia `t.*`)                        |
| Logging    | pino (+ pino-pretty in dev), redacts authorization header and passwords |
| API docs   | @elysiajs/swagger at `/swagger`                       |
| Deploy     | Dockerfile (bun slim), docker-compose (api/db/redis), k8s manifests (namespace, configmap, secret, api Deployment+Ingress+Service, postgres StatefulSet, redis Deployment) |
| CI         | GitHub Actions: test job (real postgres+redis services, `bun test --coverage`) + typecheck job; release.yml |

## Source layout

```
src/
├── index.ts            # Bootstrap: connect redis → ping db → mount plugins → listen; graceful shutdown on SIGTERM/SIGINT
├── modules/            # Platform core
│   ├── auth/           # index.ts (routes), service.ts, model.ts, plugin.ts (requireAuth/requireOrg/requirePermission macros)
│   ├── organization/   # index.ts, service.ts, repository.ts, model.ts
│   ├── rbac/           # index.ts, service.ts, repository.ts, model.ts
│   ├── audit-log/      # index.ts, service.ts, model.ts
│   ├── subscription/   # index.ts, service.ts
│   └── health/         # index.ts, service.ts, model.ts
├── domains/            # Empty — extension point for business domains
├── hooks/              # requestId hook: read x-request-id or generate UUID, echo back in response header
├── infrastructure/
│   ├── database/       # Drizzle client, schema/ (11 table files), migrations/ (5 SQL files), seed.ts (default plans)
│   ├── redis/          # ioredis client + RedisAuth helpers (blacklist, login attempts)
│   └── logger/         # pino config + Elysia logger plugin
└── shared/
    ├── errors/         # ERROR_MAP (service error string → [status, message]) + globalErrorHandler
    └── common/         # tiny utils
```

Module convention: `index.ts` = controller (routes), `service.ts` = business logic (static-method classes), `repository.ts` = DB queries, `model.ts` = TypeBox request/response schemas.

## Database schema (10 tables)

All PKs are `uuid DEFAULT gen_random_uuid()`. All FKs cascade on delete unless noted.

| Table               | Columns (beyond id/timestamps)                                                                 | Notes |
| ------------------- | ---------------------------------------------------------------------------------------------- | ----- |
| `users`             | email (unique), password_hash, display_name?, is_verified (default false)                       | bcrypt cost 12 |
| `sessions`          | user_id FK, refresh_token (unique), family (uuid), is_revoked (default false), expires_at       | refresh-token rotation with family-based reuse detection |
| `organizations`     | name, slug (unique)                                                                             | slug: `^[a-z0-9-]+$` |
| `memberships`       | user_id FK, organization_id FK, role enum('owner','admin','member', default 'member')           | unique(user_id, organization_id) |
| `roles`             | organization_id FK, name, description?                                                          | custom per-org RBAC roles |
| `permissions`       | role_id FK, action (e.g. `project:create`)                                                      | unique(role_id, action) |
| `member_roles`      | membership_id FK, role_id FK                                                                    | unique(membership_id, role_id) |
| `audit_logs`        | organization_id? (no FK), user_id? (no FK), action, metadata jsonb                              | immutable, append-only; write failures are logged, never thrown |
| `plans`             | name (unique), limits jsonb `Record<string, number>`                                            | seeded: free {max_members:5, max_roles:3}, pro {50,20}, enterprise {-1,-1} |
| `org_subscriptions` | organization_id FK (unique), plan_id FK, custom_limits jsonb?                                   | custom_limits override plan limits per-key |

Migrations live in `src/infrastructure/database/migrations/*.sql` (plain SQL — **directly reusable** by goose/golang-migrate).

## Core behaviors (must be preserved exactly)

### Auth flow
- **Register**: reject duplicate email (`EMAIL_TAKEN` → 409). bcrypt hash (cost 12). Insert user → audit `user.register` → issue token pair + create session with a fresh random `family` UUID.
- **Login**: Redis rate limit — max 5 failed attempts per email per 15 min (`TOO_MANY_ATTEMPTS` → 429; counter increments on bad password, resets on success). Wrong email or password → `INVALID_CREDENTIALS` → 401. Audit `user.login`.
- **Tokens**: access JWT (secret `JWT_ACCESS_SECRET`, TTL `JWT_ACCESS_EXPIRES_IN` default 15m, claims: `sub`, `email`); refresh JWT (secret `JWT_REFRESH_SECRET`, session row TTL `JWT_REFRESH_EXPIRES_IN` seconds, default 7d, claim: `sub`). Note: on `/refresh`, the new access token only carries `sub` (no email) — source quirk.
- **Refresh rotation**: verify refresh JWT → find session by token. Missing → `INVALID_REFRESH_TOKEN`. **If session already revoked → reuse detected → revoke the entire token family** (`REFRESH_TOKEN_REUSE`). Expired → `REFRESH_TOKEN_EXPIRED`. Otherwise revoke old session, insert new session in same family, return new pair.
- **Logout**: blacklist the presented access token in Redis for 15 min (`blacklist:<token>`), find session by refresh token, revoke **all** of that user's sessions.

### Request guards (Elysia macros → Go middleware)
- `requireAuth`: Bearer token → check Redis blacklist → verify JWT → inject `user {id, email}`.
- `requireOrg`: requireAuth + require `x-organization-id` header (400 if missing) + membership lookup (403 if not a member) → inject `organizationId`, `membership`.
- `requirePermission(action)`: requireAuth + org header + `RBACService.hasPermission` (403 with message `Missing permission: <action>`) → also injects membership.

### RBAC permission resolution
`hasPermission(userId, orgId, action)` gathers the user's permission strings via membership → member_roles → roles → permissions. Grants if any of: literal `*` (owner), exact `action` match, or wildcard `<resource>:*` matching `<resource>:<verb>`.

Membership role (`owner/admin/member`) is separate from custom RBAC roles: invite/remove-member checks use membership role directly (member = forbidden; owner cannot be removed).

### Subscription limits
`enforceLimit(orgId, key, currentCount)`: no subscription → unlimited; limit `-1` → unlimited; else `currentCount >= limit` → `LIMIT_EXCEEDED` (403). Effective limits = plan.limits merged with custom_limits (custom wins). Currently enforced only on member invite (`max_members`).

### Error handling pattern
Services throw `Error('CODE')`. Controllers map via `mapServiceError` using the ERROR_MAP (see 02-api-contract.md for the full table). Global handler: unknown route → 404 "Route not found", validation → 422 "Validation failed", parse → 400 "Invalid request body", unhandled → 500.

### Request ID
`x-request-id` read from request or generated (UUID), echoed back in the response header.

## Testing approach (source)

Bun test runner with **mocked infrastructure** (`mock.module` for db/redis — no real DB needed) for unit tests: auth service, subscription service, audit-log service, request-id hook, common utils. CI additionally runs against real postgres/redis service containers.

## Known quirks / notes for the port

- `requirePermission` uses dynamic imports (circular-dependency workaround) — irrelevant in Go, structure cleanly instead.
- Access token TTL on logout blacklist is hard-coded 15 min, independent of `JWT_ACCESS_EXPIRES_IN`.
- `subscription POST /assign` has no admin/permission check beyond `requireOrg` (any member can assign a plan) — likely an oversight; decide whether to preserve or fix (flag it).
- Audit-log writes swallow errors by design (logging must never break the request).
- Login rate limiting counts by email only (not IP).
- `sessions.refresh_token` stores the raw JWT (not hashed).
- README's k8s section contains Thai comments; the platform is a template, keep docs bilingual-agnostic.
