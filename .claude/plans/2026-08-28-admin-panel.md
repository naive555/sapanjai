# Admin Panel (platform staff console) — execution plan

> **Status:** DRAFT — not started. Owner-approved scope decisions are in §0.2; do not re-litigate them.
>
> **Read first:** `CLAUDE.md`, `docs/02-api-contract.md` (conventions + the endpoint table), `internal/middleware/auth.go`, `internal/module/mcpkey/` (the cleanest end-to-end module to copy), `internal/module/auditlog/service.go`.
>
> **Reference implementation:** `../agritech/.claude/plans/archives/20260527-13-superadmin.md` (the original build) and `20260704-superadmin-improvements.md` (the follow-up that fixed what the original got wrong). This plan folds the follow-up's corrections into the first build rather than repeating agritech's two-pass path — see §0.3.

---

## 0. Framing

### 0.1 What this is

An internal console for platform staff (2–5 people) at `/admin`, sitting **outside** the tenant boundary. Every existing route is org-scoped by `RequireOrg`/`RequirePermission`; admin routes are the deliberate exception — they read and write across all organizations, guarded by a new `RequirePlatformRole` and by a `users.platform_role` column that no tenant-facing code path can set.

The product being supported is an MCP gateway. A support ticket here is almost always *"my MCP client returns 401"* or *"the sheets connector says unhealthy"*. A console that can only see orgs and users cannot answer either question, so cross-org read views of connectors, MCP keys and gateway audit traffic are in v1 — that is the part agritech never needed and the part that pays for itself.

### 0.2 Locked decisions (owner-approved 2026-08-28)

| # | Decision | Rationale |
|---|---|---|
| D1 | **`platform_role` is never a JWT claim.** `RequirePlatformRole` reads `users.platform_role` fresh from the DB on every admin request. | Agritech shipped it as a claim and had to bolt on a Redis override key because demoting an admin did nothing for a full access-token lifetime. This repo already made the opposite call for `is_verified` (CLAUDE.md: *"deliberately not a JWT claim, which would go stale"*). Admin traffic is a handful of people; one indexed PK lookup per request is free, revocation is instant, and there is no second source of truth to own. |
| D2 | **Two platform roles: `superadmin`, `support`.** | `finance` in agritech only ever gated a plan-list read. A third value is one `CHECK` constraint change later, not a redesign. |
| D3 | **Bans are durable: `users.banned_at` / `users.ban_reason` are the source of truth**, Redis `banned:<userId>` is a fast-path cache. | Agritech shipped Redis-only and found that a Redis flush silently unbans everyone. |
| D4 | **Password re-auth on destructive ops** — delete org, grant/revoke `platform_role`, ban. Per-operation, not a sudo window. | Agritech added this in a follow-up. Cheaper to build in now. Per-op rather than a 5-minute Redis sudo window because the friction *is* the feature on delete-org. |
| D5 | **Impersonation ships**, read-only and non-refreshable. See §4. | Highest-value support tool for an MCP product (reproducing a tenant's `tools/list` is otherwise impossible), and the risk is containable because the token is method-restricted at the guard, not per-route. |
| D6 | **IP allowlist + TOTP step-up ship**, in a final phase that can be cut without touching phases 1–5. See §6. | Both are `/admin`-only concerns and touch no tenant route. |
| D7 | **Route prefix is `/admin`.** | Agritech used `/superadmin` only because `/admin/queues` already existed there. No collision here. |

### 0.3 Where this plan deliberately diverges from agritech

Agritech's follow-up plan lists seven security findings against its own first build. Six are designed out here from the start:

1. *Stale platform role in JWT* → D1, no claim at all.
2. *Redis-only bans* → D3, DB column is truth.
3. *No self-ban guard* → §3.4, guard in the service.
4. *Privileged accounts bannable/deletable directly* → §3.4, `TARGET_IS_PLATFORM_STAFF`.
5. *`changePlatformRole` doesn't verify the target exists* → §3.4, load-then-update.
6. *No re-authentication for destructive ops* → D4.
7. *No IP / user-agent in admin audit metadata* → §3.1, `adminAuditContext` captured in the handler.

Three things agritech got right and this plan copies unchanged: the short-TTL cached `COUNT(*)` for paged list endpoints, English-only admin UI (no i18n keys for staff-only screens), and a visually distinct admin layout so nobody confuses the console with the tenant app.

Two things agritech did that this plan **does not** copy:
- **Seeding a superadmin with a fixed password** (`admin@agrisense.internal` / `superadmin123`). Replaced by a promote-only bootstrap (§1.6) that can never create an account.
- **Hard user deletion + a `deleted_users` archive.** Deferred — see §8.

### 0.4 Hard non-goals

- **No admin endpoint ever returns decrypted connector `config`.** CLAUDE.md: *"Decrypted config must never leave the owning service — no response DTO field, no log line, no audit metadata."* The admin connector views are metadata only (`id, organization_id, name, type, status, last_health_check_at, created_at`). If a support engineer needs to know whether a credential is valid, the answer is the health-check result, not the credential.
- **No admin endpoint returns `mcp_api_keys.key_hash`**, or anything from which a PAT could be reconstructed.
- **No CORS middleware.** Admin pages are same-origin `/api/admin/*` through the existing runtime proxy, exactly like every other page.
- **No new secret for impersonation or file links.** Existing key material only.

---

## Phase 0 — Pre-flight

### Task 0.1 — Baseline green

Run and record results; do not start until all pass:
```
make lint && make test
cd apps/frontend && pnpm lint && pnpm exec tsc --noEmit && pnpm test
```

**Done when:** all green, with the backend test count recorded in the phase notes.

### Task 0.2 — Write the design doc

**Create:** `docs/11-admin-panel.md`.

Content: §0 of this plan (framing, the seven-finding table, non-goals), the role/permission matrix from §2.1, the impersonation threat model from §4.1, and the "explicitly deferred" list from §8. This is the document a future reader is pointed at from `CLAUDE.md`; this plan file is the throwaway execution script.

**Done when:** doc exists and `docs/` numbering is contiguous (`11-` follows `10-transactional-email.md`).

> **GATE — stop and confirm with the owner before Phase 1.**

---

## Phase 1 — Schema, platform-role guard, durable bans

### Task 1.1 — Migration `00011_platform_roles_bans.sql`

Additive-forward only; never edit an applied migration.

```sql
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
```

Down migration drops all four in reverse.

**Why a `CHECK` and not a Postgres enum:** adding a value to an enum is a migration with awkward transaction semantics; a `CHECK` is one `ALTER`. The Go side is the real type boundary.

**Done when:** `make migrate` applies and rolls back cleanly against a scratch DB.

### Task 1.2 — sqlc queries

**Edit:** `internal/infra/database/queries/users.sql`. Add:

- `GetUserByID` already exists and now returns the new columns via `SELECT *` — no change needed, but **regenerate**.
- `SetUserPlatformRole :exec` — `UPDATE users SET platform_role = $2, updated_at = now() WHERE id = $1`.
- `SetUserBan :exec` — `UPDATE users SET banned_at = $2, ban_reason = $3, updated_at = now() WHERE id = $1` (both nullable; unban passes NULL/NULL).
- `CountSuperadmins :one` — `SELECT count(*) FROM users WHERE platform_role = 'superadmin'`.

**Edit:** `internal/infra/database/queries/mcp_api_keys.sql`. The lookup used by `RequireMCPKey` must surface the key owner's ban state — see Task 1.5. Add `banned_at` to the existing by-hash lookup via a join on `users`, or add a sibling query; prefer extending the existing one so there is exactly one gateway auth path.

Run `make sqlc`. Requires the `sqlc` CLI.

**Done when:** `make sqlc` regenerates cleanly and `go build ./...` passes.

### Task 1.3 — Widen the guards' store dependency

**Edit:** `internal/middleware/auth.go`.

`Guards.store` is currently the one-method `membershipStore` interface. Add `GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)` to it and rename the interface to `userStore` (it is no longer only about memberships). `*database.Store` already satisfies it; only the test doubles in `auth_test.go` need the extra method.

**Done when:** `make test` green with the widened interface.

### Task 1.4 — Ban enforcement in `verify()`

**Edit:** `internal/middleware/auth.go`, `internal/infra/redis/auth.go`.

Redis gains `Ban(ctx, userID)` (`SET banned:<userId> 1` — no TTL), `Unban(ctx, userID)` (`DEL`), and `IsBanned(ctx, userID) (bool, error)`.

In `Guards.verify`, after the JWT is verified and the subject extracted, check `IsBanned`. **Pipeline it with the existing blacklist read** (`redis.Pipeline`) so the request cost stays at one Redis round-trip, not two — the blacklist check is keyed by token and the ban check by user id, but the user id is only known after signature verification, so the honest ordering is: blacklist check → verify signature → ban check. Two round-trips is acceptable if pipelining forces an ugly restructure; do not contort `verify()` for it, and record which you chose.

Banned → `401 "Account suspended"`. (401, not 403: the credential is no longer usable, and the frontend's 401 path already clears the session cleanly.)

**Durability caveat to write into the code comment:** the DB column is truth, Redis is cache, and nothing re-primes the cache from the DB on a Redis flush except a login attempt (Task 1.5). Worst case after a flush is that a banned user's *already-issued* access token keeps working until it expires — bounded by `JWT_ACCESS_EXPIRES_IN` (≤15 min), because banning also revokes every session so the refresh path is already dead. That bound is the reason no reaper job is needed.

### Task 1.5 — Ban enforcement at login and at the MCP gateway

**Edit:** `internal/module/auth/service.go`. After the bcrypt comparison succeeds in `Login`, if `user.BannedAt` is set: re-prime Redis (`Ban`) and return `apperror.New(apperror.AccountSuspended)`. Re-priming is what makes a Redis flush self-heal.

**Edit:** `internal/middleware/mcpkey.go`. This is the more important half. An MCP PAT has **no expiry of its own** — a banned user's key would otherwise keep working forever, which is a strictly worse hole than the JWT one. `RequireMCPKey` already does a DB lookup of the key by hash; with Task 1.2's join it now has the owner's `banned_at` for free. Reject a banned owner with the same indistinguishable 401 the gateway already returns for revoked/expired/unknown keys — do **not** add a distinguishing message.

**Done when:** integration tests in §7 covering both paths pass.

### Task 1.6 — Promote-only bootstrap

**Create:** `apps/backend/cmd/grantadmin/main.go`. Usage: `go run ./cmd/grantadmin -email user@example.com -role superadmin` (and `-role none` to revoke).

It **looks up an existing user by email and updates `platform_role`**. It never creates a user and never sets a password. If the email is unknown it exits non-zero with "no such user — register normally first, then grant".

Add `make admin-grant EMAIL=... ROLE=...` to the root Makefile.

**Why not seed:** agritech seeded `admin@agrisense.internal` with the literal password `superadmin123`. A seed that mints a privileged account with a known credential is a production incident waiting for someone to run `make seed` against the wrong `DATABASE_URL`. Promotion of an account that already proved mailbox control is the safe shape.

### Task 1.7 — `RequirePlatformRole` guard

**Create:** `internal/middleware/platform.go`.

```go
// RequirePlatformRole guards a route with RequireAuth plus a users.platform_role
// in the allowed set, read fresh from the database on every request.
//
// The role is deliberately NOT a JWT claim: a claim would keep a demoted or
// revoked staff account privileged for a full access-token lifetime, and the
// same reasoning already governs is_verified (see GET /auth/me). Admin traffic
// is a handful of internal users, so the extra indexed primary-key lookup per
// request is not worth a cached second source of truth.
//
// It injects nothing org-scoped: platform staff typically hold no membership
// anywhere, so RequireOrg's x-organization-id contract does not apply.
func (g *Guards) RequirePlatformRole(roles ...string) echo.MiddlewareFunc
```

Order inside: `verify()` (which now also rejects bans) → `GetUserByID` → role check → `c.Set(ctxPlatformRole, role)`. A user id with no row (deleted mid-session) is 401, not 500. A role not in `roles` is `403 "Insufficient permissions"` — reuse `apperror.Forbidden`'s existing message so the console cannot be probed for which roles exist.

Add `PlatformRole(c echo.Context) string` alongside the existing `UserID`/`OrgID` getters.

### Task 1.8 — Error codes

**Edit:** `internal/shared/apperror/apperror.go`. Add and map:

| Code | Status | Message |
|---|---|---|
| `ACCOUNT_SUSPENDED` | 403 | `Account suspended` |
| `REAUTH_FAILED` | 403 | `Password confirmation failed` |
| `CANNOT_TARGET_SELF` | 403 | `Cannot perform this action on your own account` |
| `TARGET_IS_PLATFORM_STAFF` | 409 | `Demote this account before banning or deleting it` |
| `SUPERADMIN_LIMIT` | 409 | `Too many superadmin accounts` |
| `PLAN_IN_USE` | 409 | `Plan has active subscriptions` |
| `IMPERSONATION_READ_ONLY` | 403 | `Impersonated sessions are read-only` |
| `CANNOT_IMPERSONATE_STAFF` | 403 | `Cannot impersonate a platform staff account` |

(`ACCOUNT_SUSPENDED` is 403 in the table but Task 1.4 returns 401 from the *guard*. That is intentional and must be documented in the contract: the guard rejects an already-issued token as unusable (401), while `POST /auth/login` refuses a banned credential (403). Do not unify them.)

### Task 1.9 — Phase 1 verification

`make lint && make test`. New unit tests: guard allows/denies per role, banned user rejected at `verify()`, banned owner rejected at the MCP gateway, `grantadmin` refuses an unknown email.

> **GATE — stop and confirm before Phase 2.**

---

## Phase 2 — Admin module: read surfaces

Module shape follows the repo convention exactly: `handler → service → sqlc queries`, services return `apperror` codes, no HTTP concerns in the service.

**Create:** `internal/module/admin/{handler.go,service.go,dto.go,countcache.go}` and `internal/infra/database/queries/admin.sql`.

### Task 2.1 — Role/permission matrix

Write this table into `docs/11-admin-panel.md` and implement it exactly.

| Route | superadmin | support |
|---|---|---|
| `GET /admin/me` | ✓ | ✓ |
| `GET /admin/organizations` · `/:orgId` | ✓ | ✓ |
| `GET /admin/users` · `/:userId` | ✓ | ✓ |
| `GET /admin/connectors` | ✓ | ✓ |
| `GET /admin/mcp-keys` | ✓ | ✓ |
| `GET /admin/audit-logs` | ✓ | ✓ |
| `GET /admin/system/stats` | ✓ | ✓ |
| `GET /admin/plans` | ✓ | ✓ |
| `POST /admin/organizations/:orgId/plan` | ✓ | — |
| `PUT /admin/organizations/:orgId/limits` | ✓ | — |
| `DELETE /admin/organizations/:orgId` | ✓ | — |
| `PATCH /admin/users/:userId/platform-role` | ✓ | — |
| `PATCH /admin/users/:userId/ban` | ✓ | — |
| `POST /admin/plans` · `PUT` · `DELETE` | ✓ | — |
| `POST /admin/users/:userId/impersonate` | ✓ | ✓ |

The split is: **support reads everything, mutates nothing** (except starting an impersonation, which is itself read-only). One rule, easy to hold in your head, and it means a compromised support account cannot destroy anything.

### Task 2.2 — Pagination + cached counts

**Create:** `internal/module/admin/countcache.go`, ported from `../agritech/apps/api/src/modules/superadmin/count-cache.ts`:

```go
// Admin list endpoints page through slow-growing tables (users, audit_logs,
// email_outbox). The per-request COUNT(*) is the expensive half, so cache it
// briefly per filter set: paging through the same filter reuses the total
// instead of re-counting every page. A short TTL keeps it fresh enough for a
// staff console; staleness after a delete is bounded and acceptable.
const countCacheTTL = 30 * time.Second
func (s *Service) cachedCount(ctx context.Context, key string, compute func() (int64, error)) (int64, error)
```

Redis namespace: `admin:count:<sha256hex(filterKey)>`. **The cache key must incorporate every filter** — a key that ignores a filter serves the wrong total and the bug looks like a pagination bug, not a cache bug. Add this namespace to CLAUDE.md's Redis key list in Phase 7.

All list endpoints share one query shape: `?limit=` (1–100, default 50), `?offset=`, `?search=`, plus per-route filters, returning `{ items: [...], total: <int> }`. Note this differs from the tenant-side `GET /audit-logs`, which returns a bare array — do not "fix" that route to match.

### Task 2.3 — `GET /admin/me`

Returns `{ id, email, displayName, platformRole }` for the calling staff account. Trivial, but it is what the frontend layout guard calls, and having it separate from `/auth/me` keeps the tenant DTO from growing an admin-only field on the wire for every tenant user.

> Exception: `/auth/me` **does** also gain `platformRole` (nullable) in Task 5.2 — the tenant app needs it to decide whether to render the "Admin" nav entry at all. `/admin/me` is the guard's call; `/auth/me` is the nav's.

### Task 2.4 — Organizations

`GET /admin/organizations` — `?search=` matches name or slug (`ILIKE`). Each item: `id, name, slug, createdAt, memberCount, connectorCount, mcpKeyCount, planName`. Left-join `org_subscriptions` + `plans`; counts as correlated subqueries.

`GET /admin/organizations/:orgId` — org row, plan + effective limits (`custom_limits` overriding `plans.limits`, same precedence the subscription service already implements — **call into `subscription.Service` rather than reimplementing the merge**), member roster with user email/displayName/role, connector list (metadata only, §0.4), MCP key list (metadata only, no hash), and the org's 20 most recent audit entries.

### Task 2.5 — Users

`GET /admin/users` — `?search=` matches email **or** display_name; `?role=superadmin|support|none`; `?banned=true|false`. Item: `id, email, displayName, isVerified, platformRole, bannedAt, createdAt, orgCount`.

`GET /admin/users/:userId` — user row + every membership with org name/slug/role + active session count + `bannedAt`/`banReason`.

Never return `password_hash`. The sqlc `db.User` struct contains it — the DTO mapping must be explicit field-by-field, not a struct embed. This is the same hazard as CLAUDE.md's "log individual fields, never a whole struct" rule, applied to serialization.

### Task 2.6 — Connectors and MCP keys (the sapanjai-specific half)

`GET /admin/connectors` — cross-org, `?organizationId=`, `?type=`, `?status=`. Item: `id, organizationId, organizationName, name, type, status, lastHealthCheckAt, createdAt`. **No `encrypted_config`, no decrypted config, not even a "config present" boolean beyond what `status` already implies.**

`GET /admin/mcp-keys` — cross-org, `?organizationId=`, `?userId=`. Item: `id, organizationId, organizationName, userId, userEmail, name, scopes, lastUsedAt, expiresAt, revokedAt, createdAt`. No `key_hash`.

These two views are what turn *"my MCP client 401s"* from unanswerable into a 10-second lookup: is the key revoked, is it expired, is its owner banned, is its connector's health check failing, and has the connector's rate-limit bucket been hit (visible via the `mcp.ratelimit.hit` audit action).

### Task 2.7 — Cross-org audit logs

`GET /admin/audit-logs` — all filters optional: `?organizationId=`, `?userId=`, `?action=` (repeatable, and a trailing `*` means prefix match so `admin.*` works), `?from=`, `?to=` (RFC3339, normalized to UTC before binding — `audit_logs.created_at` is `timestamp` without time zone, and the existing `QueryAuditLogs` comment already explains this trap), `?limit=` (max 200), `?offset=`.

New sqlc query `AdminQueryAuditLogs` in `admin.sql` — **do not** widen the existing `QueryAuditLogs`, whose mandatory `organization_id` predicate is a tenant-isolation guarantee. Two queries, two guarantees.

Items join `users.email` and `organizations.name` so the console shows names, not bare UUIDs.

### Task 2.8 — System stats

`GET /admin/system/stats` — all counts through `cachedCount`:
- `organizations`, `users`, `connectors`, `mcpKeys` (total and active), `sessions` (active), `auditLogs`
- `emailOutbox` grouped by status (`pending` / `sent` / `failed`) — a rising `failed` count is the single best early warning that Resend or the `EMAIL_FROM` domain is misconfigured
- `usersLast7d`, `organizationsLast7d`
- plan breakdown: `GROUP BY plan name, COUNT(orgs)`
- Redis `INFO memory` → `used_memory_human`

### Task 2.9 — Handler registration

**Edit:** `internal/server/server.go`. One line beside the existing module registrations:
```go
admin.NewHandler(adminSvc).Register(e.Group("/admin"), guards)
```
The admin service needs `store`, `redis`, `auditSvc`, `subSvc`, and the token service (Phase 4). Keep the constructor's dependency list explicit — no service locator.

### Task 2.10 — Phase 2 verification

`make lint && make test`, plus integration tests: each read route returns 200 for both roles, 401 unauthenticated, 403 for a tenant user with no `platform_role`, and — the one that matters — **no admin response body anywhere contains `encrypted_config`, `password_hash`, or `key_hash`**. Write that as one table-driven test that walks every admin GET and asserts the raw JSON contains none of those substrings; it will catch the next person who adds a field carelessly.

> **GATE — stop and confirm before Phase 3.**

---

## Phase 3 — Admin module: mutations

### Task 3.1 — Audit actions and request context

**Edit:** `internal/module/auditlog/service.go`. Add:

```
admin.plan.assigned · admin.limits.set · admin.org.deleted
admin.platform_role.changed · admin.user.banned · admin.user.unbanned
admin.plan.created · admin.plan.updated · admin.plan.deleted
admin.impersonation.started
```

Every admin mutation records one, best-effort (CLAUDE.md: log failures, never fail the request) — with a caveat: for `admin.org.deleted` the audit write should be attempted **before** the destructive statement, because a best-effort write that fails after the org is gone leaves no trace of who deleted it. Note this asymmetry in the code comment.

Metadata carries `ip` and `userAgent`, captured in the **handler** (`c.RealIP()`, `c.Request().UserAgent()`) and passed to the service as an explicit `AdminContext` struct — never read from `echo.Context` inside a service.

`c.RealIP()` trusts `X-Forwarded-For` unconditionally unless `e.IPExtractor` is configured. Set it in `server.New` (Task 6.1) — until then the recorded IP is attacker-controlled and the audit metadata must not be treated as evidence. Say so in the doc.

### Task 3.2 — Password re-auth helper

**In:** `internal/module/admin/service.go`.

```go
// reauth re-verifies the calling admin's own password before a destructive
// operation. It exists so that a session left open on an unlocked laptop, or a
// stolen access token, is not by itself sufficient to delete an organization.
func (s *Service) reauth(ctx context.Context, adminID uuid.UUID, password string) error
```

Loads the admin's row, `bcrypt.CompareHashAndPassword`, returns `apperror.ReauthFailed` on mismatch.

**Rate-limit it.** Redis `admin:reauth:attempts:<userId>`, max 5 per 15 minutes, mirroring the existing login limiter in `internal/infra/redis/auth.go` — reuse that code path rather than writing a second limiter. Without this, the `password` field on five endpoints is an online password oracle against the highest-value accounts in the system. Agritech's version has no such limit.

**Redaction hazard:** the request DTOs now carry a `Password` field. CLAUDE.md's centralized redaction matches attribute *keys*, so `slog.Any("body", req)` would serialize it in full. The existing rule ("log individual fields, never a whole request/body struct") already covers this; add `password` to the DTO doc comment as a reminder and make sure no admin handler logs a bound struct.

### Task 3.3 — Organization mutations

- `POST /admin/organizations/:orgId/plan` — body `{ planId }`. Delegates to `subscription.Service`'s existing upsert; does not reimplement it. Audit `admin.plan.assigned`.
- `PUT /admin/organizations/:orgId/limits` — body `{ customLimits: {...} }` (null clears). Audit `admin.limits.set`.
- `DELETE /admin/organizations/:orgId` — body `{ confirm: "<slug>", password }`. `confirm` must equal the org's slug. Cascades: memberships, connectors, MCP keys, `org_subscriptions` all `ON DELETE cascade`; `audit_logs.organization_id` has **no FK** (see `00004`), so audit rows survive the org — which is correct and should be stated, not "fixed". Audit `admin.org.deleted` (before the delete, per 3.1).

### Task 3.4 — User mutations

`PATCH /admin/users/:userId/platform-role` — body `{ role: "superadmin"|"support"|null, password }`.
Guards, in order: `reauth` → load target (missing → `USER_NOT_FOUND`, *not* a silent no-op) → `CANNOT_TARGET_SELF` if target is the caller → if promoting to `superadmin`, `CountSuperadmins` and refuse past a cap of 10 (`SUPERADMIN_LIMIT`).
After the update: **revoke every session for the target** so a demotion also ends their tenant sessions. No Redis override key is needed — that was agritech's workaround for the JWT claim this design does not have (D1). Audit `admin.platform_role.changed`.

`PATCH /admin/users/:userId/ban` — body `{ banned: bool, reason?: string, password }`.
Guards: `reauth` → load target → `CANNOT_TARGET_SELF` → `TARGET_IS_PLATFORM_STAFF` if the target has any `platform_role` (demote first).
Ban: set `banned_at`/`ban_reason`, `redis.Ban`, revoke all sessions. **Also revoke nothing in `mcp_api_keys`** — the gateway already refuses a banned owner (Task 1.5), and revoking keys is irreversible whereas a ban is not. Unban: clear both columns, `redis.Unban`; the user re-logs in normally and their MCP keys resume working. Audit `admin.user.banned` / `admin.user.unbanned`.

### Task 3.5 — Plan CRUD

`POST /admin/plans` (`{ name, limits }`), `PUT /admin/plans/:planId`, `DELETE /admin/plans/:planId`.
Delete refuses with `PLAN_IN_USE` if any `org_subscriptions` row references it (`plans` is referenced with `ON DELETE no action`, so the DB would reject it anyway — the check exists to return a 409 with a real message instead of a 500 from a constraint violation).
`limits` is free-form `jsonb`; validate that values are integers and that the keys the code actually enforces (`max_members`, `max_roles`, `max_connectors`) are present, with `-1` meaning unlimited per `cmd/seed`.

### Task 3.6 — Phase 3 verification

Integration tests per §7. In particular: every destructive route rejects a wrong password with 403 `REAUTH_FAILED` *before* doing anything, and rejects the 6th attempt in 15 minutes with `TOO_MANY_ATTEMPTS`.

> **GATE — stop and confirm before Phase 4.**

---

## Phase 4 — Impersonation

### Task 4.1 — Threat model (write into `docs/11-admin-panel.md` first)

What it is: a staff member obtains a short-lived token that authenticates **as** a tenant user, so they can see exactly what that user sees — which orgs, which connectors, which `tools/list` output.

What makes it containable:

1. **Read-only, enforced at the guard, not per route.** A token carrying `imp: true` is rejected on any request whose method is not `GET`/`HEAD`/`OPTIONS`, inside `Guards.verify()`. A new route added next year is covered automatically; a per-route allowlist would not be.
2. **Short-lived and non-refreshable.** 10-minute TTL, no refresh token, no `sessions` row. It cannot be extended — only re-issued, which writes a fresh audit entry.
3. **Staff cannot be impersonated.** A target with any `platform_role` is refused (`CANNOT_IMPERSONATE_STAFF`), so impersonation cannot be a privilege-escalation ladder.
4. **A reason is mandatory** (min 10 chars) and lands in the audit metadata alongside the actor, target, IP and user agent.
5. **The MCP gateway is untouched.** PATs are authenticated by `RequireMCPKey`, which never sees a JWT; there is no way to impersonate into the gateway.

Residual risk, stated plainly: a staff member can read any tenant's data, including connector *metadata* and audit history. They cannot read connector credentials (§0.4 holds — nothing decrypts config for a response) and cannot change anything. The control is detection, not prevention: every start is audited with a reason.

### Task 4.2 — Token claims

**Edit:** `internal/module/auth/token.go`. Extend `accessClaims`:

```go
type accessClaims struct {
    Email string `json:"email,omitempty"`
    // Actor is the platform-staff user id on behalf of whom this token was
    // issued (RFC 8693 "act"). Present only on impersonation tokens.
    Actor string `json:"act,omitempty"`
    // Impersonated marks the token read-only at the guard.
    Impersonated bool `json:"imp,omitempty"`
    jwt.RegisteredClaims
}
```

Add `SignImpersonationToken(targetID uuid.UUID, targetEmail string, actorID uuid.UUID, ttl time.Duration)`.

This is a claim, and D1 says roles are not claims — the distinction is that `imp`/`act` describe *this token*, are immutable for its lifetime, and expire in 10 minutes. There is no "stale" state to go wrong. `platform_role` describes a mutable DB row, which is exactly why it is not a claim.

`VerifyAccessToken` currently returns `(uuid.UUID, string, error)`. Widen it to return a small struct rather than a third and fourth positional value — `middleware.tokenVerifier`, `auth`'s tests, and the MCP principal resolver all touch this signature, so change it once, deliberately.

### Task 4.3 — Guard enforcement

**Edit:** `internal/middleware/auth.go`. In `verify()`, after a successful parse: if `Impersonated` and `c.Request().Method` is not a safe method → `403 IMPERSONATION_READ_ONLY`. Store the actor id in context (`ctxActorID`) and expose `ActorID(c)`.

`RequirePlatformRole` must reject impersonation tokens outright regardless of method — an impersonated token authenticates as a tenant user who by construction has no `platform_role`, so this falls out for free, but assert it in a test so a future refactor cannot break it silently.

### Task 4.4 — Endpoint

`POST /admin/users/:userId/impersonate` — `RequirePlatformRole("superadmin", "support")`, body `{ reason }` (min 10 chars). No password re-auth: it is read-only, and requiring a password for a routine support action trains staff to type their password reflexively, which is worse for the ops that *do* need it.

Refuses: unknown user, banned user, any user with a `platform_role`.

Returns `{ accessToken, expiresIn: 600, user: { id, email, displayName } }`. **No refresh token.** Audit `admin.impersonation.started` with `{ target_user_id, reason, ip, user_agent }`.

### Task 4.5 — Phase 4 verification

Tests: `GET` under an impersonation token succeeds and is scoped to the target's orgs; every non-safe method returns 403; the token is rejected by `/auth/refresh` (no session row exists) and by every `/admin` route; impersonating staff is refused; the audit entry carries the reason.

> **GATE — stop and confirm before Phase 5.**

---

## Phase 5 — Frontend console

Stack and conventions are the existing ones: App Router, shadcn/ui, TanStack Query, the runtime proxy at `app/api/[...path]/route.ts` (which needs **no change** — `/api/admin/*` already forwards). Admin screens are **English-only, unstyled by the tenant theme's accent**, matching agritech's convention for staff-only UI.

### Task 5.1 — Route group and layout

**Create:** `app/(admin)/admin/layout.tsx` and the pages below.

The layout calls `GET /admin/me`; a 401/403 redirects to `/overview`. Client-side gating is cosmetic — the server is the authority — but a staff member landing on a wall of failed queries is a bad console.

Visual treatment: distinct accent (amber/red), a persistent "PLATFORM ADMIN — you are outside the tenant boundary" strip, and a "Back to app" link. The single worst failure mode of a console like this is a staff member believing they are in their own org.

### Task 5.2 — Session plumbing

**Edit:** `lib/api/endpoints.ts` — add `platformRole: "superadmin" | "support" | null` to `MeResponse`; add the admin endpoint functions and their types.
**Edit:** `lib/auth/use-session.tsx` — expose `platformRole`, `isPlatformStaff`, and `impersonating` state.
**Edit:** `app/(dashboard)/layout.tsx` — conditionally render an "Admin" nav entry when `isPlatformStaff`.

**Impersonation client model:** the admin's own access token is in memory and their refresh token in `localStorage` (unchanged). Starting an impersonation **replaces the in-memory access token only** and sets an `impersonating` flag; the admin's refresh token stays untouched in `localStorage`. Exiting = drop the impersonation token and run the normal single-flight refresh, which restores the admin session from the untouched refresh token. Nothing new is persisted, and closing the tab ends the impersonation by itself.

Impersonation must **suppress the 401-refresh retry** in `lib/api/client.ts` — a 401 under an impersonation token means "expired, 10 minutes are up", and the existing single-flight refresh would silently swap the admin's own session back in under a UI still claiming to be the tenant. That is the one genuinely dangerous frontend bug available here. Handle it explicitly: on 401 while impersonating, exit impersonation and toast.

Render a full-width, unmissable banner while impersonating: *"Viewing as user@example.com · read-only · expires in 9:34 · Exit"*.

### Task 5.3 — Pages

| Path | Contents |
|---|---|
| `/admin` | Overview: stat tiles from `/admin/system/stats`, plan breakdown, email-outbox health, recent `admin.*` activity |
| `/admin/organizations` | Searchable paged table → detail |
| `/admin/organizations/[orgId]` | Org info, plan + effective limits editor, member roster (linking to user detail), connectors, MCP keys, recent audit; danger zone (slug + password confirm) |
| `/admin/users` | Searchable paged table, role filter, banned filter, Banned/Verified badges → detail |
| `/admin/users/[userId]` | User info, memberships (linking to org detail), sessions count, platform-role control, ban control (both password-confirmed), "Impersonate" |
| `/admin/plans` | Plan cards with limits editor, create, delete (disabled when in use) |
| `/admin/audit-logs` | All filters, org/user/action/date-range, paged |
| `/admin/system` | Full stats, 30s `refetchInterval` |

Reuse `components/data-table.tsx`, `page-header.tsx`, `copyable-id.tsx`, `role-badge.tsx`, `connector-status.tsx` rather than writing admin variants.

### Task 5.4 — Phase 5 verification

`pnpm lint && pnpm exec tsc --noEmit && pnpm test`, plus a component test for the impersonation-401 path (5.2) — it is the one piece of frontend logic here with a real failure mode.

> **GATE — stop and confirm before Phase 6.**

---

## Phase 6 — Hardening: IP allowlist + TOTP step-up

Self-contained; can be cut or deferred without touching phases 1–5.

### Task 6.1 — Trusted proxy configuration (prerequisite, do this first)

**Edit:** `internal/server/server.go`. Set `e.IPExtractor = echo.ExtractIPFromXFFHeader(echo.TrustLoopback(true), echo.TrustPrivateNet(true))` — or `ExtractIPFromRealIPHeader` depending on the deployment (`docs/09-railway-deploy.md`).

Without this, `c.RealIP()` returns whatever `X-Forwarded-For` says, which makes **both** the audit metadata from §3.1 and the allowlist below attacker-controlled. This task is load-bearing for the other two; do not skip it because "we already log the IP".

### Task 6.2 — IP allowlist

**Edit:** `internal/config`, `internal/middleware/platform.go`.

`ADMIN_IP_ALLOWLIST` — comma-separated CIDRs, parsed at boot (a malformed entry fails startup, not the first request). **Empty/unset disables the check**, which is the required default for local development.

Applied to the `/admin` group *before* authentication — an off-network request should not even reach the password/TOTP surface. Rejection is `404 "Route not found"`, not 403: the console's existence is not something an off-network scanner needs confirmed.

Document loudly in `.env.example` and `docs/09-railway-deploy.md` that a wrong value locks staff out of the console with no in-app recovery path — recovery is an env change and a redeploy.

### Task 6.3 — TOTP step-up

**Migration `00012_user_totp.sql`:**
```sql
CREATE TABLE "user_totp" (
  "user_id" uuid PRIMARY KEY REFERENCES "users"("id") ON DELETE cascade,
  "secret_encrypted" jsonb NOT NULL,
  "recovery_codes" text[] NOT NULL,
  "confirmed_at" timestamp,
  "created_at" timestamp DEFAULT now() NOT NULL
);
```

- `secret_encrypted` is sealed with `internal/shared/envelope` under `CONNECTOR_MASTER_KEY` — the same envelope machinery connectors use, so rotation is already solved and no new secret is introduced. (The env var's name is now a slight misnomer; rename is a separate, wider change — note it, don't do it here.)
- `recovery_codes` holds SHA-256 hashes of ten 128-bit codes, not bcrypt. Same reasoning CLAUDE.md already records for MCP PATs: these are CSPRNG output, not low-entropy passwords.
- Dependency: `github.com/pquerna/otp`.

**Flow (step-up, not login):**
- `POST /admin/2fa/enroll` — generates a secret, returns the `otpauth://` URI **once**. Guarded by `RequirePlatformRole` *without* the 2FA requirement (chicken-and-egg).
- `POST /admin/2fa/confirm` `{ code }` — sets `confirmed_at`, returns ten recovery codes **once**.
- `POST /admin/2fa/verify` `{ code }` — on success sets Redis `admin:2fa:<userId>` with a 12h TTL. Rate-limited: `admin:2fa:attempts:<userId>`, 5 per 15 min, reusing the login limiter. A recovery code is accepted here too and is deleted on use.
- `RequirePlatformRole` requires that Redis key when `ADMIN_REQUIRE_2FA=true` (default `true`; `false` for local dev), excluding the three routes above.

Step-up rather than folding TOTP into `POST /auth/login` because login is a contract route in `docs/02-api-contract.md` shared with every tenant user — changing its response shape for a handful of staff accounts would be a breaking change to a documented contract for no benefit.

**Deliberate limitation:** the 12h Redis key is not revoked when an admin is demoted. Demotion revokes sessions (§3.4), which kills the access token, so the stale 2FA key is unreachable without a fresh login — at which point the role check fails first. Confirm this holds with a test before accepting the shortcut.

### Task 6.4 — Phase 6 verification

Tests: allowlist rejects an off-CIDR IP with 404 before auth runs; empty allowlist disables the check; TOTP verify accepts a valid code, rejects a stale window, accepts a recovery code exactly once; `ADMIN_REQUIRE_2FA=false` bypasses cleanly.

> **GATE — stop and confirm before Phase 7.**

---

## Phase 7 — Documentation, contract, release

### Task 7.1 — `docs/02-api-contract.md`

Add an **Admin** section to the endpoint table covering every route above: method, path, guard, request, response, and every error code. CLAUDE.md is explicit that this file is the source of truth and that new domain routes belong in it.

Also document the deliberate `ACCOUNT_SUSPENDED` status split (401 from the guard, 403 from login) — see Task 1.8.

### Task 7.2 — `CLAUDE.md`

Add to the module bullets: **Admin**, one paragraph in the established voice, covering the DB-read role (and why it is not a claim), durable bans with the Redis-flush bound, password re-auth, read-only impersonation, and the "no decrypted config, no key hash" non-goal.

Add to the Redis key list: `banned:<userId>` (no TTL, DB-backed cache), `admin:count:<hash>` (30s), `admin:reauth:attempts:<userId>` (5/15min), `admin:2fa:<userId>` (12h), `admin:2fa:attempts:<userId>` (5/15min).

Add to the Environment section: `ADMIN_IP_ALLOWLIST` (optional, empty = disabled), `ADMIN_REQUIRE_2FA` (default true).

Add a ground rule: **admin responses never carry decrypted connector config, `password_hash`, or `key_hash`; admin DTO mapping is explicit field-by-field, never a struct embed of a sqlc row.**

### Task 7.3 — Swagger + env template

`make swagger` (annotate every admin handler as you write it, not in a catch-up pass). Update `.env.example` with the two new vars and their comments.

### Task 7.4 — Final verification

```
make lint && make test && make swagger
cd apps/frontend && pnpm lint && pnpm exec tsc --noEmit && pnpm test
docker compose up -d --build     # full stack still boots
```

Manual smoke, in order:
1. `make admin-grant EMAIL=... ROLE=superadmin` on an existing account; log in; `/admin` renders.
2. A tenant user gets 403 on every `/admin` route and no "Admin" nav entry.
3. Grant `support`: reads work, every mutation returns 403.
4. Ban a user → their login returns 403, an in-flight access token 401s within 15 min, **their MCP key stops working immediately**; unban restores all three.
5. Demote a superadmin holding a fresh access token → their next `/admin` call 403s immediately (no waiting for expiry — this is D1's whole point).
6. Delete an org with the wrong password → `REAUTH_FAILED`; six wrong attempts → `TOO_MANY_ATTEMPTS`; correct slug + password → deleted, audit entry present with actor and IP.
7. Impersonate a tenant user → their orgs and connectors are visible; any POST/PATCH/DELETE 403s; the banner counts down; Exit restores the admin session.
8. `/admin/audit-logs` filtered to `admin.*` shows every action from steps 4–7.

### Task 7.5 — Archive

Move this file to `.claude/plans/archives/` with a completion header (date, what shipped, what changed during execution, follow-ups), matching the existing archive convention. Update the memory notes.

---

## 8. Explicitly deferred

- **Hard user deletion + `deleted_users` archive.** Agritech built this in its own plan (`20260531-user-deletion-archive.md`), not the original superadmin plan, and for good reason: the sole-owner case (deleting the only owner of an org orphans that org, its connectors and its MCP keys) is a design question, not a task. Ban covers the operational need — a banned user cannot log in, cannot refresh, and cannot use their PATs — and is reversible. Deletion gets its own plan when a deletion request actually arrives.
- **Per-tenant usage metering / MCP call volume charts.** Needs a time-series store or rollup table; the audit log is not one. The stats page shows counts, not trends beyond 7d.
- **Admin-initiated password reset for a tenant user.** The self-service `POST /auth/forgot-password` flow already exists and is enumeration-safe; a staff-triggered variant is a second token-minting path to secure for marginal gain.
- **Admin-side connector config editing or credential rotation.** Would require decrypting config into an admin response, which §0.4 forbids outright.
- **Notifications / alerting on `failed` outbox rows or `mcp.ratelimit.hit` spikes.** The console surfaces both; paging on them is an observability plan, not an admin-panel one.
- **i18n for admin screens.** Staff-only UI stays English (agritech convention; the tenant app has no i18n either).

## 9. New environment variables

| Var | Default | Phase | Notes |
|---|---|---|---|
| `ADMIN_IP_ALLOWLIST` | *(empty — disabled)* | 6 | Comma-separated CIDRs. Parsed at boot; malformed entry fails startup. A wrong value locks staff out with no in-app recovery. |
| `ADMIN_REQUIRE_2FA` | `true` | 6 | Set `false` locally. Does not gate the enroll/confirm/verify routes. |

## 10. Files touched (summary)

**New backend:** `migrations/00011_platform_roles_bans.sql`, `migrations/00012_user_totp.sql`, `internal/middleware/platform.go`, `internal/module/admin/{handler,service,dto,countcache}.go`, `internal/infra/database/queries/admin.sql`, `cmd/grantadmin/main.go`, `internal/server/admin_integration_test.go`.

**Edited backend:** `internal/middleware/auth.go` (widened store, ban check, impersonation method guard), `internal/middleware/mcpkey.go` (banned owner), `internal/infra/redis/auth.go` (ban methods), `internal/module/auth/{token.go,service.go,handler.go}` (claims, login ban check, `/auth/me` platformRole), `internal/module/auditlog/service.go` (actions), `internal/shared/apperror/apperror.go` (codes), `internal/server/server.go` (wiring, IP extractor), `internal/infra/database/queries/{users,mcp_api_keys}.sql`, root `Makefile`.

**New frontend:** `app/(admin)/admin/**` (layout + 8 pages), admin API bindings in `lib/api/endpoints.ts`.

**Edited frontend:** `lib/auth/use-session.tsx`, `lib/api/client.ts` (impersonation 401 path), `app/(dashboard)/layout.tsx` (nav entry).

**Docs:** `docs/11-admin-panel.md` (new), `docs/02-api-contract.md`, `docs/09-railway-deploy.md`, `CLAUDE.md`, `.env.example`, `apps/backend/docs/` (regenerated swagger).
