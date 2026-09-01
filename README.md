# Sapanjai (HeartBridge)

**A managed [MCP](https://modelcontextprotocol.io) gateway for business data.** Sapanjai puts a permission boundary between an AI agent and a customer's systems: an organization mints a revocable key, points it at a connector, and the agent sees exactly the tools that organization's roles grant — over exactly the data the connector's allowlist admits, rate limited, and written to an audit log.

The problem it exists to solve: wiring Claude or Cursor into a company's spreadsheets today means handing an agent a long-lived OAuth credential. That credential is **all-or-nothing** (every file the account can reach), **unattributable** (the upstream audit trail says "the service account", not which person or which key), **hard to revoke** (rotating it cuts off everyone), and **unscoped** (nothing stops a "read last month's invoices" agent from listing the whole Drive). Sapanjai is the thing in between:

```
Claude · Cursor · Claude Desktop
        │  Authorization: Bearer sk_live_…
        ▼
POST /mcp/:connectorId          ← one Streamable HTTP MCP endpoint per connector
        │
        ├─ key           → the org it was minted in, and its creator's live RBAC grant ∩ the key's scopes
        ├─ tools/list    → only the tools that principal may call are registered at all
        ├─ tools/call    → re-checked, rate limited per connector, audit logged
        └─ adapter       → the upstream, restricted to the connector's allowlist
                           (today: Google Sheets + Drive, read only)
```

Everything a B2B product needs around that — accounts, organizations, roles, plans, audit logs, transactional email, a staff console — is here as well, because a gateway that cannot answer *"who did that, under which key, and can I switch it off"* is not a product. Those parts began as a reusable platform core and are still general enough to build another domain on.

The name: *sà-pan-jai* (สะพานใจ), a bridge of hearts — the gateway sits between two parties that do not otherwise trust each other with a credential.

## The three ideas the design turns on

Each was a finding from the spike in [`spikes/mcp-gateway/`](spikes/mcp-gateway/), and each is load-bearing. The reasoning is in [`docs/05-mcp-gateway.md`](docs/05-mcp-gateway.md).

**1. A tool is a route.** `sheets:read` gates `sheets_query_rows` exactly the way `RequirePermission("connector:read")` gates a REST route — the same action strings, the same `*` > `resource:verb` > `resource:*` matching, the same owner bypass. Adding MCP required no change to the RBAC engine, and an org's existing roles govern its agents for free.

**2. A denied tool is invisible, not merely uncallable.** It is never registered on the server built for that request, so it never appears in `tools/list` and the model does not know to attempt it. But the tool list is a **UX affordance, never the authorization boundary** — clients cache it — so every `tools/call` re-checks permission at call time. Revoking a role takes effect on the agent's *next call*, not its next reconnect.

**3. The credential carries the tenant.** MCP client headers are static configuration: the model cannot choose one per call and no client has a "switch organization" concept, so the `x-organization-id` header that scopes every REST route has no analogue here. The organization is bound to the key instead. A key's optional `scopes` can only **narrow** its creator's live grant, never widen it, and that grant is re-resolved on every request rather than frozen into the key.

## What is built, and what is not

| | Status |
| --- | --- |
| Gateway — `POST /mcp/:connectorId`, stateless Streamable HTTP, RBAC-filtered catalog, per-connector rate limit, audit trail | **shipped** |
| MCP keys — mint / list / revoke org-scoped PATs, raw token shown once | **shipped** |
| `google_sheets` connector — six read tools over allowlisted Sheets + Drive, OAuth refresh, health probe, signed short-lived file downloads | **shipped** |
| Platform core — auth, email verification, password reset, organizations, RBAC, audit logs, plans, background worker | **shipped** |
| Staff console (`/admin`) — cross-org reads, superadmin-only mutations, read-only impersonation, IP allowlist + TOTP step-up, dashboard UI | **shipped** ([`docs/11`](docs/11-admin-panel.md)) |
| Write tools (append a row, upload a file) | not built — [`docs/07`](docs/07-sheets-adapter-decisions.md) §3 |
| OAuth consent flow in the dashboard | not built — onboarding is a manual credential paste ([MCP client setup](#mcp-client-setup)) |
| OAuth 2.1 / dynamic client registration for Claude Desktop's connector picker | not built |
| A second adapter (FlowAccount, PEAK, LINE, …) | not built — the connector-agnostic boundary it would land on is drawn in [`docs/08`](docs/08-gateway-core.md) |

## Stack

| | |
| --- | --- |
| **Backend** | Go · [Echo](https://echo.labstack.com) · [sqlc](https://sqlc.dev) + [pgx/v5](https://github.com/jackc/pgx) · [goose](https://github.com/pressly/goose) migrations · go-redis v9 · golang-jwt/v5 · [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) · `log/slog` · [swaggo](https://github.com/swaggo/swag) |
| **Frontend** | Next.js (App Router) · TypeScript · Tailwind v4 · [shadcn/ui](https://ui.shadcn.com) · [TanStack Query](https://tanstack.com/query) |
| **Infra** | PostgreSQL 16 · Redis 7 · [Resend](https://resend.com) · Docker Compose · Kubernetes manifests · GitHub Actions |

## Prerequisites

- **Go 1.26+**
- **Node 24+** (Active LTS) with [Corepack](https://nodejs.org/api/corepack.html) enabled (`corepack enable`) — this repo uses **pnpm**, pinned by `apps/frontend/package.json`'s `packageManager` field. The exact major lives in `apps/frontend/.nvmrc`; run `nvm use` there if you juggle versions.
- **Docker** + Docker Compose v2

## Quickstart

```bash
cp .env.example .env

make up        # start Postgres + Redis (docker compose)
make migrate   # apply the database schema (goose)
make seed      # insert the default plans (free / pro / enterprise)

make api       # terminal 1 — Go API on :3000
make web       # terminal 2 — Next.js dev server on :4000
make worker    # terminal 3 (optional) — background jobs, health on :3001

curl localhost:3000/health
open http://localhost:4000   # dashboard — register a user to get started
```

That gets you the platform. To go from a fresh account to an AI agent actually reading a spreadsheet through the gateway, follow [MCP client setup](#mcp-client-setup) — it is the walkthrough this README is really about.

There is no process manager tying the three together — `make api`, `make web`, and `make worker` are separate terminals by design. To run everything in containers instead, see [Docker](#docker).

## Repository layout

```
apps/backend/
  cmd/
    api/           HTTP server entrypoint
    worker/        background job runner entrypoint
    migrate/       goose CLI wrapper (up / down / status)
    seed/          idempotent default-plan seeder
    grantadmin/    promote/demote an existing user's platform_role (make admin-grant)
    healthcheck/   tiny binary for the distroless image's HEALTHCHECK
  internal/
    config/        env parsing + validation (fails fast at boot)
    server/        Echo wiring: middleware stack, error handler, route mounting
    middleware/    RequireAuth, RequireOrg, RequirePermission, RequireMCPKey,
                   RequirePlatformRole, request ID
    module/        one package per domain: auth, organization, rbac, auditlog,
                   subscription, connector, health, mcpkey, mcp, admin
    adapter/       per-connector-type upstream integrations (googlesheets/ — the
                   first, Sheets + Drive; imports connector for the Checker
                   interface, not the reverse)
    worker/        the Job interface + interval scheduler with Redis locking
    job/           registered jobs: sessioncleanup, emaildispatch
    infra/         database (pgx pool + sqlc-generated queries), redis
    shared/        apperror, httpx, logger (incl. log redaction), email
                   (renderer + Resend/log senders), envelope (envelope
                   encryption for connector config)
  migrations/      goose SQL migrations, embedded into the binaries
  docs/            generated OpenAPI spec (committed)

apps/frontend/
  app/(auth)/      login, register, verify-email, forgot-password, reset-password
  app/(dashboard)/ overview, organizations, members, roles, activity (audit log),
                   subscription, mcp-keys, connectors (+ the google_sheets form)
  app/(admin)/     platform-staff console: overview, organizations, users, plans,
                   audit-logs, system — outside the tenant boundary
  app/api/[...path]/route.ts   runtime reverse proxy to the backend
  lib/api/         fetch client with single-flight 401 refresh
  lib/auth/        token store + session provider
  lib/org/         active-organization state
  components/      shadcn/ui primitives + app components

docs/              the design record — see the table at the end of this file
spikes/            throwaway feasibility spikes, each its own Go module and
                   deliberately outside the build (make/CI only cover apps/backend)
k8s/               Kubernetes manifests (api, worker, migrate Job, postgres, redis)
compose.yaml       full stack: db, redis, api, worker, web
```

## Architecture

### Two request flows

The dashboard's, which is an ordinary REST stack:

```
browser → /api/* (same-origin, Next.js Route Handler proxy)
        → Echo router
        → middleware:  Recover → RequestID → request logger
        → guard:       RequireAuth | RequireOrg | RequirePermission(action) | RequirePlatformRole
        → handler:     bind + validate DTO, no business logic
        → service:     business logic, returns apperror codes — no HTTP types
        → sqlc queries / Redis
```

And the agent's, which is the product:

```
MCP client → POST /mcp/:connectorId
        → RequireMCPKey:  hash the bearer token, look it up, resolve org + principal
        → resolveConnector: load the connector, confirm it belongs to that org
        → a fresh mcp.Server per request, registering only the permitted tools
        → tools/call:  re-check permission → Redis token bucket → adapter → upstream
        → audit log (best effort)
```

The second flow is stateless on purpose. Stateful Streamable HTTP pins a session to one server instance at `initialize`, which would mean sticky routing behind a load balancer and permission changes that only land on reconnect; stateless mode makes an MCP request behave exactly like an Echo route. The cost — no server→client sampling or elicitation, no resumable streams — buys horizontal scaling and immediate revocation. A connector that one day needs progress reporting should return a job id and poll with a second tool rather than reach for SSE.

A single Echo `HTTPErrorHandler` maps `apperror` codes to status codes and messages, so services never import `net/http`. The gateway needs a **second** mapping alongside it: a JSON-RPC error aborts the agent's turn, while `CallToolResult{IsError: true}` is text the model can read and adapt to, so permission denials and not-founds go back as `IsError` and the agent says "I don't have access to that" instead of crashing.

Multi-step writes (org create + owner membership, session rotation, register + queue its verification email) run inside a transaction. Each module follows the same shape: `handler.go` → `service.go` → `dto.go`, backed by sqlc-generated queries in `internal/infra/database` — `mcp` is the one exception, since its handler hands the JSON-RPC envelope to the MCP SDK's `StreamableHTTPHandler` rather than binding a DTO.

### Auth model

- **Access token** — HS256, claims `{ sub, email }`, 15 min by default (`JWT_ACCESS_EXPIRES_IN`). Checked against a Redis blacklist (`blacklist:<accessToken>`) on every guarded request.
- **Refresh token** — separate secret, claims `{ sub }`, backed by a `sessions` row whose `expires_at` is `now + JWT_REFRESH_EXPIRES_IN` seconds (default 7 days).
- **Rotation** — every `/auth/refresh` issues a new pair and revokes the old session. Replaying an already-rotated or revoked refresh token revokes the **entire token family**, so a stolen token is contained.
- **Rate limiting** — 5 failed logins per email per 15 minutes (`login:attempts:<email>`).
- **Logout** — blacklists the presented access token for 15 minutes and revokes all of the user's sessions.
- **Email verification / password reset** — both ride a Redis token + outbox pattern ([`docs/10`](docs/10-transactional-email.md)). `register` inserts the user row and its verification email into `email_outbox` in one transaction, so a user can never exist without its mail queued. Tokens are 32 CSPRNG bytes stored SHA-256-**hashed** in Redis and consumed with `GETDEL`, so redemption is single-use with no read-then-delete race and a Redis dump yields nothing redeemable. `POST /auth/verify-email` is deliberately a POST: the frontend page at `/verify-email?token=…` is the GET target and POSTs the token on, so a link scanner prefetching the page cannot burn it. `POST /auth/forgot-password` **always** returns `200 {success:true}` — unknown address, known address, and active cooldown are indistinguishable.
- **Verification is a banner, not a gate.** Nothing in the backend checks `is_verified`. `GET /auth/me` reads it fresh from the database rather than carrying it as a JWT claim, which would go stale for a full access-token lifetime after a user verifies.

### Secrets at rest

Connector `config` — OAuth client secrets, refresh tokens, database credentials — is sealed with envelope encryption (`internal/shared/envelope`): a fresh AES-256-GCM data key per row, wrapped by `CONNECTOR_MASTER_KEY`. No endpoint, response DTO, log line, or audit entry ever returns it; the dashboard's connector form is write-only by construction. Rotation is rotate-on-read rather than a batch job — every envelope carries the `kid` that wrapped it, and retired keys stay decrypt-only in `CONNECTOR_MASTER_KEY_PREVIOUS` while rows re-seal as they are read.

MCP keys are hashed with SHA-256, not bcrypt. A PAT is 256 bits of CSPRNG output, not a low-entropy password, so the slow-hash argument does not apply and the gateway would be paying bcrypt on every agent call ([`docs/07`](docs/07-sheets-adapter-decisions.md) §1).

### Guards

| Guard | Requires |
| --- | --- |
| `RequireAuth` | valid, non-blacklisted access token |
| `RequireOrg` | `RequireAuth` + `x-organization-id` header + caller is a member of that org |
| `RequirePermission(action)` | `RequireOrg` + the caller's roles grant `action` (owners bypass) |
| `RequireMCPKey` | a live, unrevoked MCP key (`Authorization: Bearer sk_live_…`) — no JWT, no org header; the key names the org |
| `RequirePlatformRole(…)` | `RequireAuth` + `users.platform_role` ∈ the listed roles, read **fresh from the database** on every request. Deliberately not a JWT claim: a claim would keep a demoted account privileged for a full access-token lifetime |

Permission matching: `*` grants everything; then an exact `resource:verb` match; then a `resource:*` wildcard on that resource.

### Logging

`log/slog` throughout, with redaction centralized in `internal/shared/logger/redact.go` and wired into every logger via `ReplaceAttr` — any attr key in the sensitive set (`authorization`, `password`, `token`, `access_token`, `refresh_token`, `cookie`, `secret`, `api_key`) logs as `[REDACTED]` regardless of call site. Because slog only exposes leaf keys, log individual fields rather than whole request structs. Request logging is limited to method, URI (sanitized), status, latency, and request id.

## API

[`docs/02-api-contract.md`](docs/02-api-contract.md) is the **source of truth** for routes, headers, status codes, and error messages. Summary:

| Method & path | Guard | Purpose |
| --- | --- | --- |
| `GET /health` | public | `{ status, uptime }` |
| `POST /auth/register` | public | Create a user, return an access/refresh pair |
| `POST /auth/login` | public | Return an access/refresh pair (rate limited) |
| `POST /auth/refresh` | public | Rotate the refresh token, return a new pair |
| `POST /auth/logout` | public¹ | Blacklist the access token, revoke all sessions |
| `GET /auth/me` | auth | The caller, including `isVerified` read fresh from the database |
| `POST /auth/verify-email` | public | Redeem a verification token (single-use) |
| `POST /auth/resend-verification` | auth | Re-queue the verification email (5 min cooldown) |
| `POST /auth/forgot-password` | public | Queue a reset email — **always** `200 {success:true}` |
| `POST /auth/reset-password` | public | Redeem a reset token; also verifies the user and revokes every session |
| `POST /organizations` | auth | Create an org; caller becomes its owner |
| `GET /organizations` | auth | Caller's memberships, org embedded |
| `GET /organizations/members` | org | Active org's member roster |
| `POST /organizations/invite` | org | Add an existing user (enforces `max_members`) |
| `DELETE /organizations/members/:userId` | org | Remove a member (never the owner) |
| `GET /rbac/roles` | org | List custom roles with their permissions |
| `POST /rbac/roles` | org | Create a role and set its permissions |
| `PUT /rbac/roles/:roleId/permissions` | org | Replace a role's permission set |
| `POST /rbac/assign` | org | Assign a role to a member |
| `GET /subscription` | org | Org's subscription with plan embedded (nullable) |
| `POST /subscription/assign` | org | Upsert the org's plan |
| `GET /plans` | auth | All plans (global, not org-scoped) |
| `GET /audit-logs` | org | Org's logs, newest first — `userId`, `action`, `limit` (1–100, default 50) |
| `POST /connectors` | perm:`connector:write` | Create a connector; `config` is sealed with envelope encryption |
| `GET /connectors` | perm:`connector:read` | Org's connectors, oldest first |
| `GET /connectors/:connectorId` | perm:`connector:read` | One connector (never includes `config`) |
| `PATCH /connectors/:connectorId` | perm:`connector:write` | Partial update; a supplied `config` is re-sealed |
| `DELETE /connectors/:connectorId` | perm:`connector:delete` | Remove a connector |
| `POST /connectors/:connectorId/health-check` | perm:`connector:write` | Probe the upstream — `501` for `type: "generic"`, a real probe for `type: "google_sheets"` |
| `POST /mcp-keys` | perm:`mcpkey:write` | Mint a Personal Access Token; the raw `apiKey` is returned once, here, and nowhere else |
| `GET /mcp-keys` | perm:`mcpkey:read` | Org's MCP keys (never the hash or raw token) |
| `DELETE /mcp-keys/:keyId` | perm:`mcpkey:delete` | Revoke a key (`revoked_at`) |
| `POST /mcp/:connectorId` | MCP key² | Not REST — one Streamable HTTP MCP JSON-RPC endpoint per connector. See [MCP client setup](#mcp-client-setup) below. |
| `GET /mcp/files/:connectorId/:fileId` | signed link³ | Downloads a Drive file handed out by `drive_get_file` |
| `GET /admin/{me,organizations,organizations/:orgId,users,users/:userId,connectors,mcp-keys,audit-logs,system/stats,plans}` | platform:`superadmin,support` | Cross-organization **read** views for platform staff — outside the tenant boundary, no `x-organization-id`. See [Staff console](#staff-console). |
| `POST /admin/organizations/:orgId/plan`, `PUT …/limits`, `DELETE /admin/organizations/:orgId`, `PATCH /admin/users/:userId/{platform-role,ban}`, `POST/PUT/DELETE /admin/plans[/:planId]` | platform:`superadmin` | Staff mutations. The three destructive ones (org delete, role grant, ban) re-verify the caller's own password |
| `POST /admin/users/:userId/impersonate` | platform:`superadmin,support` | Mints a 10-minute, non-refreshable, **read-only** token for a tenant user. Refuses any staff target |
| `POST /admin/2fa/{enroll,confirm,verify}` | platform:`superadmin,support` | TOTP step-up enrollment and verification — the only `/admin` routes exempt from the `ADMIN_REQUIRE_2FA` gate |

¹ reads `Authorization` if present, but does not require it.
² `Authorization: Bearer sk_live_...` — an MCP key from `POST /mcp-keys` above, not a JWT access token.
³ no header at all: the URL carries an HMAC signature and an expiry of at most 15 minutes. The signing key is derived from `CONNECTOR_MASTER_KEY` with HKDF-SHA256 rather than being a separate secret to own — a master-key rotation invalidating in-flight links is a non-event at that TTL.

Common error responses: `401 Unauthorized` / `Token revoked`, `400 Missing x-organization-id header`, `403 Not a member of this organization`, `403 Missing permission: <action>`, `422 Validation failed`, `404 Route not found`. Service-level codes (`EMAIL_TAKEN`, `REFRESH_TOKEN_REUSE`, `LIMIT_EXCEEDED`, …) and their exact messages are tabulated in `docs/02-api-contract.md`.

### Swagger

With `make api` running, [`localhost:3000/swagger`](http://localhost:3000/swagger) serves interactive docs for every route (schemas, status codes, `BearerAuth` scheme); the raw spec is at `/swagger/doc.json`.

The spec is generated from Go doc-comments and committed to `apps/backend/docs/`. After changing handler annotations:

```bash
go install github.com/swaggo/swag/cmd/swag@latest   # once
make swagger
```

### Try it with curl

```bash
# --- auth ---------------------------------------------------------------
# register → { accessToken, refreshToken }
curl -s localhost:3000/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"password123"}'

# login
curl -s localhost:3000/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"password123"}'

# refresh — rotates the pair; reusing the old refresh token afterwards 401s
curl -s localhost:3000/auth/refresh \
  -H 'Content-Type: application/json' \
  -d '{"refreshToken":"<refreshToken>"}'

# logout — blacklists the access token, revokes every session
curl -s localhost:3000/auth/logout \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <accessToken>' \
  -d '{"refreshToken":"<refreshToken>"}'
```

Everything below needs `Authorization: Bearer <accessToken>`, and — past org creation — an `x-organization-id` header naming an org the caller belongs to.

```bash
# --- organizations ------------------------------------------------------
curl -s localhost:3000/organizations \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <accessToken>' \
  -d '{"name":"Acme Corp","slug":"acme-corp"}'

curl -s localhost:3000/organizations \
  -H 'Authorization: Bearer <accessToken>'

curl -s localhost:3000/organizations/invite \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <accessToken>' \
  -H 'x-organization-id: <orgId>' \
  -d '{"email":"teammate@example.com","role":"member"}'

curl -s -X DELETE localhost:3000/organizations/members/<userId> \
  -H 'Authorization: Bearer <accessToken>' \
  -H 'x-organization-id: <orgId>'

# --- rbac ---------------------------------------------------------------
curl -s localhost:3000/rbac/roles \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <accessToken>' \
  -H 'x-organization-id: <orgId>' \
  -d '{"name":"editor","permissions":["project:create","project:*"]}'

curl -s -X PUT localhost:3000/rbac/roles/<roleId>/permissions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <accessToken>' \
  -H 'x-organization-id: <orgId>' \
  -d '{"permissions":["doc:read"]}'

curl -s localhost:3000/rbac/assign \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <accessToken>' \
  -H 'x-organization-id: <orgId>' \
  -d '{"userId":"<memberUserId>","roleId":"<roleId>"}'

# --- subscription & audit logs -----------------------------------------
curl -s localhost:3000/plans -H 'Authorization: Bearer <accessToken>'

curl -s localhost:3000/subscription/assign \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <accessToken>' \
  -H 'x-organization-id: <orgId>' \
  -d '{"planId":"<planId>"}'

curl -s 'localhost:3000/audit-logs?action=org.created&limit=10' \
  -H 'Authorization: Bearer <accessToken>' \
  -H 'x-organization-id: <orgId>'
```

## Frontend

With `make up` and `make api` running:

```bash
make web   # cd apps/frontend && pnpm dev — Next.js on :4000
```

`next dev` runs on **:4000**, not the framework default, because the Go API already owns :3000 and both run at once. Open [`localhost:4000`](http://localhost:4000) and register a user; `/` redirects to `/login` or `/organizations` depending on session state, and every page (Overview, Organizations, Members, Roles, Activity, Subscription, MCP keys, Connectors) talks to the live API. The unauthenticated pages cover the full email flow too — `/verify-email`, `/forgot-password`, `/reset-password`. An account holding a `platform_role` also sees an Admin nav entry into the staff console at `/admin` ([Staff console](#staff-console)). MCP keys (`/mcp-keys`) mints/revokes Personal Access Tokens for MCP clients; Connectors (`/connectors`) creates and manages upstream connections, including a `google_sheets`-specific form (`/connectors/:id/google-sheets`) for the OAuth paste-path credentials and the spreadsheet/Drive allowlist — see [MCP client setup](#mcp-client-setup) below for the full walkthrough.

**Same-origin only.** The browser never calls the Go API directly — it calls `/api/*` on the Next.js origin, and `app/api/[...path]/route.ts` proxies to `BACKEND_URL`. This is a Route Handler rather than a `next.config.ts` `rewrites()` entry on purpose: `next.config.ts` resolves once at build time, so a rewrite destination gets baked into the image, whereas the handler reads `process.env.BACKEND_URL` fresh on every request. The same production image therefore works in dev (`http://localhost:3000`) and in compose (`http://api:3000`) unchanged. A consequence worth knowing: **the backend has no CORS middleware and needs none.**

**Tokens.** The access token lives in memory only and is lost on a full page reload by design; the refresh token persists in `localStorage` and is used to silently re-authenticate on mount. The API client single-flights concurrent 401s through one `/auth/refresh` call, then retries the original request once.

```bash
cd apps/frontend
pnpm install
pnpm dev                 # :4000
pnpm build               # production build
pnpm exec tsc --noEmit   # typecheck
pnpm test                # vitest
pnpm lint                # eslint
```

These aren't wired into the root Makefile — run them from `apps/frontend/`. See [`apps/frontend/README.md`](apps/frontend/README.md) for the full breakdown.

## MCP client setup

The platform doubles as a **Managed MCP Gateway**: once a `google_sheets` connector is configured, an AI agent (Claude Code, Claude Desktop's HTTP path, Cursor, ...) can read a customer's spreadsheets and Drive files through the [Model Context Protocol](https://modelcontextprotocol.io), scoped by the same RBAC permissions as everything else here. Design notes: [`docs/05-mcp-gateway.md`](docs/05-mcp-gateway.md) (architecture) and [`docs/06-sheets-adapter.md`](docs/06-sheets-adapter.md) (the adapter spec). This walkthrough is everything needed to go from zero to a connected client using only this README.

**1. What you need from Google first** — there is no OAuth consent flow in the dashboard yet (onboarding is a manual credential paste for the MVP), so a customer supplies their own:

- A Google Cloud project with the **Sheets API** and **Drive API** enabled.
- An **OAuth 2.0 client ID** (Desktop app type is simplest) — gives you a `client_id` and `client_secret`.
- A **refresh token** for that client, scoped to `https://www.googleapis.com/auth/spreadsheets.readonly` and `https://www.googleapis.com/auth/drive.readonly` — obtained by running the OAuth consent screen once yourself (e.g. via [Google OAuth Playground](https://developers.google.com/oauthplayground) with your own client id/secret, or a short local script) and keeping the resulting `refresh_token`. A project left in **testing mode** (unverified, under 100 users) works fine for this — `spreadsheets.readonly`/`drive.readonly` are sensitive scopes and full verification is weeks of calendar time outside this project's control.
- The **spreadsheet and/or Drive folder ids** you want the connector to be able to read (from each item's URL). This allowlist is enforced on every call regardless of what else the OAuth token can reach.

**2. Create an org and mint an MCP key** — register/log in at [`localhost:4000`](http://localhost:4000), create an organization if you don't have one, then open **MCP keys** (`/mcp-keys`) → **Create key**. The raw key (`sk_live_...`) is shown exactly once — copy it now, it cannot be recovered later.

**3. Create the connector** — open **Connectors** (`/connectors`) → create one with type `google_sheets`, then fill in its dedicated form (`/connectors/:id/google-sheets`) with the `client_id`/`client_secret`/`refresh_token` from step 1 and the spreadsheet/folder ids to allowlist. The config is write-only: once saved, no endpoint or page ever shows it back.

**4. Run the health check** — from the connector's row, run **Health check**. It refreshes the OAuth token and reads metadata for the first allowlisted spreadsheet (or lists the first allowlisted folder if no spreadsheet is allowlisted); success flips the connector to `active`. A failure flips it to `error` without ever surfacing the underlying probe error (it may contain credential-shaped detail) — recheck the pasted credentials and the allowlist.

**5. Point an MCP client at it.** The endpoint is `POST /mcp/:connectorId`, Streamable HTTP, stateless JSON — `Content-Type: application/json` and `Accept: application/json, text/event-stream` are both required on every request (the SDK rejects a POST missing either), and the credential is the MCP key from step 2, **not** a JWT access token:

```bash
claude mcp add sapanjai --scope local --transport http \
  http://localhost:3000/mcp/<connectorId> \
  --header "Authorization: Bearer sk_live_..."
claude mcp list   # -> sapanjai: ... - ✔ Connected
```

(`--header` is variadic and swallows anything after it, so the URL must come *before* `--header` — a real gotcha hit during development.) `claude mcp list` should show the connection, and asking the agent to list its available tools should surface the six `google_sheets` tools — `sheets_list_spreadsheets`, `sheets_describe_spreadsheet`, `sheets_query_rows`, `sheets_read_range`, `drive_list_folder`, `drive_get_file` — plus two connector-type-agnostic ones, `sapanjai_describe_connector` and `sapanjai_whoami`, which let an agent orient itself and let you debug "why can't it see that tool" from the client side. All of it is filtered to whatever `sheets:read`/`drive:read` the key's creator actually holds, intersected with the key's own `scopes`. A tool call against a spreadsheet outside the connector's allowlist is rejected every time, even if the underlying Google account could otherwise reach it.

**What's not here yet:** write tools (append/update a sheet, upload to Drive), a LINE adapter, an OAuth consent flow in the dashboard (hence step 1's manual paste), and OAuth 2.1 / dynamic client registration for Claude Desktop's own connector-picker UI — see `docs/07-sheets-adapter-decisions.md` §3 "Out of scope" for the full list. This walkthrough has been verified against the code and its tests but **not** re-run end to end against a real Google account as part of this documentation pass — treat step 1 onward as the intended path, not a confirmed transcript.

## Staff console

`/admin` is the platform-staff surface, and the one place in the codebase that deliberately crosses the tenant boundary: every other route is scoped by `RequireOrg` / `RequirePermission`, while these read **across all organizations**. Staff typically hold no membership anywhere, so `x-organization-id` has no meaning here. Full design, threat model, and the decisions behind it: [`docs/11-admin-panel.md`](docs/11-admin-panel.md).

The product being supported is a gateway, so a support ticket is almost always *"my MCP client returns 401"* or *"the sheets connector says unhealthy"*. A console that could only see organizations and users would answer neither, which is why cross-org read views of connectors, MCP keys, and gateway audit traffic are in v1 rather than a later phase.

**Two roles.** `support` reads everything and mutates nothing; `superadmin` additionally holds every mutation. That split is enforced by two *separate* `RequirePlatformRole` guard instances on the route table — not a role branch inside a handler — so a compromised support account cannot destroy anything, and the routing itself is the source of truth for who may reach what. Starting an impersonation is on the read guard, because the token it mints is itself read-only.

**Shipped** — the ten read surfaces; eight superadmin mutations (assign plan, set custom limits, delete organization, grant/revoke `platform_role`, ban/unban, plan CRUD); read-only impersonation; the `ADMIN_IP_ALLOWLIST` network gate and the TOTP step-up; and the dashboard UI at `/admin` (overview, organizations, users, plans, audit logs, system stats).

Four decisions worth repeating here because they are easy to "optimize" back into a bug:

- **`platform_role` is never a JWT claim.** The guard reads `users.platform_role` fresh from the database on every admin request. A claim would keep a demoted account privileged for a full access-token lifetime, and the usual patch for that — a Redis override key — is a second source of truth. One indexed primary-key lookup buys instant revocation. The same call was already made for `is_verified`.
- **Bans are durable.** `users.banned_at` is the source of truth; `banned:<userId>` in Redis is a fast-path cache that can be flushed without unbanning anyone. Enforcement sits everywhere a credential is checked — `RequireAuth` (401 `Account suspended`), login (403), and the MCP key guard — so a banned owner's agent keys stop working even though a PAT has no expiry of its own.
- **The destructive three re-verify a password.** Deleting an organization, changing a platform role, and banning a user each take the caller's own password in the request body, rate-limited independently of login (`admin:reauth:attempts:<userId>`, 5 per 15 min). A session left open on an unlocked laptop is not by itself enough. Plan and limit changes deliberately do *not* prompt — making staff type a password for a reversible edit only trains them to type it reflexively on the routes that matter.
- **Impersonation is read-only and bounded.** The minted token lasts 10 minutes, cannot be refreshed, carries an `imp` claim that the auth guard turns into a 403 on any non-`GET`/`HEAD`/`OPTIONS` request, and can never reach `/admin` itself. Any platform-staff target is refused outright, closing off impersonation as a privilege-escalation ladder. Every start is audited with the operator's stated reason.

**Off-network callers get a 404, not a 403.** `ADMIN_IP_ALLOWLIST` gates the whole group *before* `RequireAuth` runs, so a scanner never learns there is anything here to be denied from. It is unset by default, which disables the check; a wrong value locks every staff account out with no in-app recovery path, so change it in staging first ([`docs/09`](docs/09-railway-deploy.md)).

There is no way to *create* an admin through the API. The **first** platform role has to come from the operator CLI — a chicken-and-egg `PATCH /admin/users/:userId/platform-role` cannot solve on its own, since calling it already requires being a superadmin:

```bash
make admin-grant EMAIL=you@example.com ROLE=superadmin   # or ROLE=support
make admin-grant EMAIL=you@example.com ROLE=none         # revoke
```

It never creates a user and never sets a password — the account must already exist and have registered normally. Once one superadmin exists, the console's own role-grant route handles every subsequent promotion or revocation; the CLI stays available for bootstrap and for cutting access without going through the console.

## Background worker

`cmd/worker` is a separate binary sharing the API's config, database pool, Redis client, and logger. It runs registered jobs on an interval, coordinated across replicas by a Redis lock (`worker:lock:<job>`, held for roughly one interval) so a job runs about once per interval **fleet-wide** rather than once per replica. A failed run releases the lock immediately so another replica can retry sooner. Each run gets a timeout and panic recovery — a broken job never takes the worker down.

Job stats (runs, failures, skips, last error, last duration) are exposed on the worker's own internal `GET /health` on `WORKER_PORT`. That endpoint is operational only and is not part of the public API contract.

```bash
make worker                  # cd apps/backend && go run ./cmd/worker
curl localhost:3001/health   # {status, uptime, jobs: [{name, runs, failures, skipped, ...}]}
```

Two jobs ship.

**Session cleanup** (`SESSION_CLEANUP_*`) batch-deletes expired sessions, plus revoked sessions older than a retention window. Revoked sessions are kept for a while on purpose — that's what lets refresh-token reuse detection recognize a replayed token as a family reuse instead of an unknown session.

**Email dispatch** (`EMAIL_*`) drains the `email_outbox` table: it claims a batch with `FOR UPDATE SKIP LOCKED`, sends via `internal/shared/email`, then marks each row `sent` (nulling the body in the same statement) or backs it off to `failed` after `EMAIL_MAX_ATTEMPTS`, and prunes old rows. Claiming is **lease-based rather than a `sending` status**: the claim pushes `next_attempt_at` forward by the job timeout plus one interval, so a run that dies mid-flight leaves its rows claimable again when the lease lapses. Nothing has to reap them, and there is no stuck state to own.

Two consequences worth knowing. `RESEND_API_KEY` is read **only** by `cmd/worker` — the API renders and enqueues and never talks to Resend, which keeps that secret off the internet-facing service. And rendered bodies contain live single-use tokens, so an `email.Message` is never logged and a body never appears in an error; the centralized log redaction cannot help there, because it matches attribute *keys* and the token sits inside a value. The one sanctioned exception is `email.LogSender`, which prints the whole body so a local developer can click the link without a Resend account — gated by an **allowlist** of local `APP_ENV` values, not by "anything that isn't production", so a staging deploy missing its API key fails loudly instead of quietly logging tokens.

**Adding a job.** Implement `worker.Job` — `Name() string`, `Interval() time.Duration`, `Run(ctx) (worker.Result, error)` — in a new package under `apps/backend/internal/job/<name>/`, then register it with one line in `cmd/worker/main.go`: `w.Register(yourjob.New(...))`. Locking, timeouts, panic recovery, and logging are handled for you.

## Configuration

Copy `.env.example` → `.env`. The API and worker read the same file.

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_NAME` | `sapanjai-api` | logger service name |
| `APP_ENV` | `development` | `development` enables pretty logging |
| `PORT` | `3000` | API port |
| `LOG_LEVEL` | `debug` | |
| `DATABASE_URL` | — | **required** |
| `DATABASE_USER` / `DATABASE_PASSWORD` / `DATABASE_NAME` | `username` / `password` / `sapanjai` | also configure the compose `db` container and the container-side `DATABASE_URL` |
| `REDIS_URL` | — | **required** |
| `REDIS_KEY_PREFIX` | `sapanjai:` | prepended to every Redis key, so an instance shared with another app cannot collide with ours. Must match on api and worker — they meet on the job locks and the token blacklist. Changing it orphans every live key (in-flight verification/reset links stop resolving); the orphans expire on their own TTLs. Set explicitly empty to opt out. |
| `JWT_ACCESS_SECRET` / `JWT_REFRESH_SECRET` | — | **required**, min 32 chars each |
| `CONNECTOR_MASTER_KEY` | — | **required**, base64 of exactly 32 bytes — generate with `openssl rand -base64 32`. Wraps every connector's envelope-encryption data key; the value in `.env.example` is a working dev key, not a placeholder to leave in place for anything real. |
| `CONNECTOR_MASTER_KEY_PREVIOUS` | — | optional, comma-separated base64 keys. Retired `CONNECTOR_MASTER_KEY` values kept decrypt-only so rows sealed under an old key still open; each read that lands on a retired key also re-seals under the current one (rotate-on-read). Drop an entry once every row has been read at least once since the rotation. |
| `JWT_ACCESS_EXPIRES_IN` | `15m` | Go duration string |
| `JWT_REFRESH_EXPIRES_IN` | `604800` | **seconds**, not a duration string |
| `APP_PUBLIC_URL` | `http://localhost:4000` | the browser-facing **frontend** origin that verification and reset links are built against — not the API. Trailing slash trimmed |
| `MCP_RATE_LIMIT_PER_MIN` | `60` | per-connector upstream-API token-bucket capacity, tokens/minute — see [`docs/07-sheets-adapter-decisions.md`](docs/07-sheets-adapter-decisions.md) step 4. Most `google_sheets` tools charge a floor of 1 unit per `tools/call`; `sheets_query_rows`' bounded scan charges 1 unit per page fetched from the upstream API instead, so a single call can cost more than 1 |
| `ADMIN_IP_ALLOWLIST` | — | optional, comma-separated CIDRs gating the whole `/admin` group before `RequireAuth` runs; unset/empty disables the check. Parsed once at boot — a malformed entry fails startup rather than the first request. A wrong value locks every platform staff account out of `/admin` with no in-app recovery path |
| `ADMIN_REQUIRE_2FA` | `true` | gates every `/admin` route except `POST /admin/2fa/{enroll,confirm,verify}` behind a completed TOTP step-up. Set `false` only for local development |
| `WORKER_PORT` | `3001` | worker's internal `/health` port |
| `WORKER_JOB_TIMEOUT` | `5m` | per-run timeout, any job |
| `SESSION_CLEANUP_INTERVAL` | `1h` | |
| `SESSION_CLEANUP_RETENTION` | `720h` | how long a revoked-but-unexpired session survives (30d) |
| `SESSION_CLEANUP_BATCH_SIZE` | `1000` | rows per `DELETE` statement |
| `RESEND_API_KEY` | — | **worker only.** Unset ⇒ `LogSender`, which prints the whole rendered email (including its live token) to the log — allowed only in a local `APP_ENV`, and a hard failure anywhere else |
| `EMAIL_FROM` | `Sapanjai <noreply@localhost>` | must be on a Resend-verified domain in production, or every send 403s |
| `EMAIL_DISPATCH_INTERVAL` | `15s` | |
| `EMAIL_DISPATCH_BATCH_SIZE` | `20` | rows claimed per run |
| `EMAIL_MAX_ATTEMPTS` | `5` | after this many failures a row is marked `failed` |
| `EMAIL_OUTBOX_RETENTION` | `168h` | how long `sent`/`failed` rows survive before the prune sweep (7d) |

A single `.env` serves both paths — `make api`/`make worker` load it via godotenv, and compose passes the same file to the containerized api/worker as `env_file`. Only the two host-specific URLs differ, and compose overrides them per service rather than in a second file: `DATABASE_URL` (the `db` hostname) and `REDIS_URL` (`redis://redis:6379`). That is safe in both directions — `godotenv.Load` never overrides a variable compose has already set, and `.env` is in `apps/backend/.dockerignore`, so nothing is baked into the image.

`DATABASE_USER`/`PASSWORD`/`NAME` are the single source of the credentials: compose feeds them to the Postgres container *and* builds the api/worker `DATABASE_URL` from them, so the two can't drift. The `DATABASE_URL` in `.env` is the host-side one (via the published port); containers get theirs from compose using the `db` hostname.

Redis keys used, all written with `REDIS_KEY_PREFIX` in front:

| Key | Purpose |
| --- | --- |
| `blacklist:<accessToken>` | logged-out access tokens, 15 min |
| `login:attempts:<email>` | 5 failures per 15 min |
| `banned:<userId>` | fast-path ban cache — `users.banned_at` is the source of truth |
| `verify:email:<sha256hex(token)>` | email-verification token → userId, 24h, `GETDEL` |
| `verify:resend:<userId>` | resend cooldown, `SET EX 300 NX` |
| `reset:password:<sha256hex(token)>` | password-reset token → userId, 1h, `GETDEL` |
| `reset:request:<email>` | reset cooldown, 15 min — keyed by **email, not userId**, because `forgot-password` runs before the address is known to belong to a user, and an id-keyed cooldown could not cover the unknown-address path identically |
| `worker:lock:<jobName>` | job lock, TTL ≈ one interval |
| `mcp:ratelimit:<connectorId>` | per-connector token bucket, idle TTL 2 min |
| `admin:count:<hash>` | short-TTL cache for admin list counts, 30s — keyed by the exact filter set, so paging one search doesn't re-`COUNT(*)` per page |
| `admin:reauth:attempts:<userId>` | password re-auth limiter on the three destructive admin mutations, 5 per 15 min — independent of `login:attempts:` so burning one budget doesn't hand an attacker a fresh one on the other |
| `admin:2fa:<userId>` | a completed TOTP step-up, 12h |
| `admin:2fa:attempts:<userId>` | brute-force limiter on the 6-digit step-up code, 5 per 15 min |

Both token namespaces store `sha256hex(32 random bytes)`, never the raw token, so a `KEYS` / `MONITOR` / RDB dump yields nothing redeemable. Key construction lives behind a small helper in each of `internal/infra/redis/{auth,email,ratelimit,admincount}.go` and `internal/worker/lock.go`, so the prefix cannot be applied on the write path and forgotten on the read path. It namespaces the *keyspace*, not the instance — `maxmemory` and eviction stay instance-wide, so a noisy co-tenant can still evict our locks and tokens.

## Docker

`apps/backend/Dockerfile` is a multi-stage build: a `golang:1.26-alpine` builder compiles every binary — `api`, `worker`, `migrate`, `seed`, `grantadmin`, `healthcheck` — into one image, and which one runs is chosen by overriding the container **command**. The runner is [`gcr.io/distroless/static-debian12:nonroot`](https://github.com/GoogleContainerTools/distroless); distroless has no shell, which is why `HEALTHCHECK` runs the dedicated `healthcheck` binary rather than `curl`. The `worker` service reuses that same image with `command: ["/app/worker"]` and `HEALTHCHECK_PORT=3001`, so the shared healthcheck probes the worker's port instead of the API's.

The image ends in `CMD ["/app/api"]` and deliberately sets **no `ENTRYPOINT`**. An exec-form entrypoint would make a command override *append* to `/app/api` rather than replace it, so the worker, migrate, and seed services would each silently start a second API — and hosts that expose only a "start command" (Railway, see [`docs/09`](docs/09-railway-deploy.md)) give you no way to reset the entrypoint.

`apps/frontend/Dockerfile` builds the Next.js standalone output on `node:${NODE_VERSION}-alpine`, where `NODE_VERSION` is a single build `ARG` shared by all three stages so the build and runtime majors can't diverge (override with `--build-arg NODE_VERSION=…`). The runner sets `HOSTNAME=0.0.0.0` explicitly — the standalone server otherwise binds to the container's assigned IP rather than all interfaces, and a loopback-based `HEALTHCHECK` would fail silently.

```bash
# individual images
docker build -t sapanjai-api:dev ./apps/backend
docker build -t sapanjai-web:dev ./apps/frontend

# full stack: db, redis, api, worker, web — web waits on api's HEALTHCHECK
docker compose up -d --build   # reads the same .env as the host-side workflow
open http://localhost:4000
```

## Kubernetes

Manifests live in [`k8s/`](k8s/) — see [`k8s/README.md`](k8s/README.md) for the layout and apply instructions.

```bash
cp k8s/secret.example.yaml k8s/secret.yaml
cp k8s/postgres/secret.example.yaml k8s/postgres/secret.yaml
# fill in real values in both, then:
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/ -R
```

`api`, `worker`, `postgres`, and `redis` are covered, plus a `migrate` Job that applies the schema and seeds the default plans on apply — no manual migration step. The frontend has no manifests yet; compose already runs the full stack (see [Docker](#docker)).

## Testing

```bash
make test    # go test ./...
make lint    # golangci-lint if installed, else go vet ./...
```

Two layers:

- **Unit tests** per service, using interface mocks for the database and Redis.
- **Integration tests** in `apps/backend/internal/server/`, run against real Postgres and Redis (service containers in CI). These encode `docs/02-api-contract.md` directly — every route, its happy path, and each of its error codes.

The cases that are non-negotiable, because they are the ones where a regression is a security incident rather than a bug:

*Auth* — refresh rotation; replaying a rotated token revoking the whole family; the rate limit tripping at 5 attempts; logout revoking every session and blacklisting the access token.

*Gateway* — a denied tool never appearing in `tools/list`; a permission or allowlist change taking effect on the very next call rather than the next reconnect; a revoked, expired, or unknown PAT rejected with reasons the caller cannot tell apart; one tenant's connector and spreadsheets unreachable from another org's key; a rate-limited or scan-budget-exhausted call ending cleanly as `IsError` / `scan_complete: false` rather than a panic or a raw Go error reaching the model.

CI (`.github/workflows/ci.yml`) runs lint, backend test, frontend (lint / typecheck / test / build), and a Docker build job. `release.yml` pushes images to ghcr on a green CI run against `main`.

## Commands

```
make up              # start db + redis
make down            # stop all compose services

make migrate         # apply all pending migrations (goose up)
make migrate-down    # roll back the most recent migration
make migrate-status  # show migration status
make seed            # seed default plans (free/pro/enterprise) — idempotent

make admin-grant EMAIL=you@example.com ROLE=superadmin   # or ROLE=support / ROLE=none

make api             # run the Go API
make web             # run the Next.js dev server
make worker          # run the background job runner

make build           # backend binaries + frontend production build
make test            # go test ./...
make lint            # golangci-lint if installed, else go vet ./...
make fmt             # go fmt ./...
make tidy            # go mod tidy

make sqlc            # regenerate sqlc query code (requires sqlc)
make swagger         # regenerate the OpenAPI spec (requires swag)
```

`make sqlc` is only needed after editing `apps/backend/internal/infra/database/queries/*.sql`, and requires the CLI: `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`. Building or running the API does not.

## Documentation

| File | Contents |
| --- | --- |
| [`CLAUDE.md`](CLAUDE.md) | Ground rules and conventions — read before changing anything |
| [`docs/01-source-analysis.md`](docs/01-source-analysis.md) | Domain model, behaviors, and known quirks |
| [`docs/02-api-contract.md`](docs/02-api-contract.md) | **Source of truth** for routes, headers, status codes, error messages |
| [`docs/03-target-architecture.md`](docs/03-target-architecture.md) | Package layout, design decisions, resolved deviations |
| [`docs/04-migration-plan.md`](docs/04-migration-plan.md) | Phased delivery plan |
| [`docs/05-mcp-gateway.md`](docs/05-mcp-gateway.md) | Managed MCP Gateway architecture, phases, and shipped-vs-not status |
| [`docs/06-sheets-adapter.md`](docs/06-sheets-adapter.md) | `google_sheets` connector spec: tool catalog, RBAC mapping, guardrails |
| [`docs/07-sheets-adapter-decisions.md`](docs/07-sheets-adapter-decisions.md) | Why the adapter is shaped the way it is — the four decisions, what was left unbuilt and its trigger, and the risks still open. The 12 build steps are archived in `.claude/plans/archives/2026-08-18-sheets-adapter.md` |
| [`docs/08-gateway-core.md`](docs/08-gateway-core.md) | The connector-agnostic boundary: which parts of the gateway belong to every connector and which belong to the one that happened to be first. Read before adding a second adapter |
| [`docs/09-railway-deploy.md`](docs/09-railway-deploy.md) | How this monorepo is deployed on Railway, and the two settings that are easy to get wrong |
| [`docs/10-transactional-email.md`](docs/10-transactional-email.md) | The outbox + Redis-token design behind verification and password reset, and the token-logging rule |
| [`docs/11-admin-panel.md`](docs/11-admin-panel.md) | Staff console design: the authorization boundary, the impersonation threat model, and what was deliberately left out |
| [`apps/frontend/README.md`](apps/frontend/README.md) | Frontend proxy, token model, page map |
| [`k8s/README.md`](k8s/README.md) | Manifest layout and apply instructions |
