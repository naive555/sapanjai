# Sapanjai (HeartBridge)

A multi-tenant B2B SaaS platform template: **Go backend + Next.js dashboard** in one monorepo.

It gives you the parts every B2B product needs before it can have any features of its own:

- **Auth** — bcrypt password hashing, HS256 JWT access/refresh pairs, session rotation with token-family reuse detection, a Redis access-token blacklist, and login rate limiting.
- **Organizations** — orgs, memberships, invite/remove, and an active-org header (`x-organization-id`) that scopes every other route.
- **RBAC** — org-scoped custom roles with wildcard permissions (`*` > `resource:verb` > `resource:*`).
- **Audit logs** — immutable, org-scoped, queryable, written best-effort so they can never fail a request.
- **Subscriptions** — plans with limits (e.g. `max_members`) enforced at the point of use.
- **Background jobs** — an interval scheduler with Redis-based cross-replica locking.

Add your business domain on top as new modules.

| | |
| --- | --- |
| **Backend** | Go · [Echo](https://echo.labstack.com) · [sqlc](https://sqlc.dev) + [pgx/v5](https://github.com/jackc/pgx) · [goose](https://github.com/pressly/goose) migrations · go-redis v9 · golang-jwt/v5 · `log/slog` · [swaggo](https://github.com/swaggo/swag) |
| **Frontend** | Next.js (App Router) · TypeScript · Tailwind v4 · [shadcn/ui](https://ui.shadcn.com) · [TanStack Query](https://tanstack.com/query) |
| **Infra** | PostgreSQL 16 · Redis 7 · Docker Compose · Kubernetes manifests · GitHub Actions |

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

There is no process manager tying the three together — `make api`, `make web`, and `make worker` are separate terminals by design. To run everything in containers instead, see [Docker](#docker).

## Repository layout

```
apps/backend/
  cmd/
    api/           HTTP server entrypoint
    worker/        background job runner entrypoint
    migrate/       goose CLI wrapper (up / down / status)
    seed/          idempotent default-plan seeder
    healthcheck/   tiny binary for the distroless image's HEALTHCHECK
  internal/
    config/        env parsing + validation (fails fast at boot)
    server/        Echo wiring: middleware stack, error handler, route mounting
    middleware/    RequireAuth, RequireOrg, RequirePermission, request ID
    module/        one package per domain: auth, organization, rbac,
                   auditlog, subscription, connector, health
    worker/        the Job interface + interval scheduler with Redis locking
    job/           registered jobs (sessioncleanup)
    infra/         database (pgx pool + sqlc-generated queries), redis
    shared/        apperror, httpx, logger (incl. log redaction), envelope
                   (envelope encryption for connector config)
  migrations/      goose SQL migrations, embedded into the binaries
  docs/            generated OpenAPI spec (committed)

apps/frontend/
  app/(auth)/      login, register
  app/(dashboard)/ organizations, members, roles, audit, subscription
  app/api/[...path]/route.ts   runtime reverse proxy to the backend
  lib/api/         fetch client with single-flight 401 refresh
  lib/auth/        token store + session provider
  lib/org/         active-organization state
  components/      shadcn/ui primitives + app components

docs/              source analysis, API contract, architecture, migration plan
k8s/               Kubernetes manifests (api, worker, migrate Job, postgres, redis)
compose.yaml       full stack: db, redis, api, worker, web
```

## Architecture

### Request flow

```
browser → /api/* (same-origin, Next.js Route Handler proxy)
        → Echo router
        → middleware:  Recover → RequestID → request logger
        → guard:       RequireAuth | RequireOrg | RequirePermission(action)
        → handler:     bind + validate DTO, no business logic
        → service:     business logic, returns apperror codes — no HTTP types
        → sqlc queries / Redis
```

A single Echo `HTTPErrorHandler` maps `apperror` codes to status codes and messages, so services never import `net/http`. Multi-step writes (org create + owner membership, session rotation) run inside a transaction.

Each module follows the same shape: `handler.go` → `service.go` → `dto.go`, backed by sqlc-generated queries in `internal/infra/database`.

### Auth model

- **Access token** — HS256, claims `{ sub, email }`, 15 min by default (`JWT_ACCESS_EXPIRES_IN`). Checked against a Redis blacklist (`blacklist:<accessToken>`) on every guarded request.
- **Refresh token** — separate secret, claims `{ sub }`, backed by a `sessions` row whose `expires_at` is `now + JWT_REFRESH_EXPIRES_IN` seconds (default 7 days).
- **Rotation** — every `/auth/refresh` issues a new pair and revokes the old session. Replaying an already-rotated or revoked refresh token revokes the **entire token family**, so a stolen token is contained.
- **Rate limiting** — 5 failed logins per email per 15 minutes (`login:attempts:<email>`).
- **Logout** — blacklists the presented access token for 15 minutes and revokes all of the user's sessions.

### Guards

| Guard | Requires |
| --- | --- |
| `RequireAuth` | valid, non-blacklisted access token |
| `RequireOrg` | `RequireAuth` + `x-organization-id` header + caller is a member of that org |
| `RequirePermission(action)` | `RequireOrg` + the caller's roles grant `action` (owners bypass) |

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
| `POST /connectors/:connectorId/health-check` | perm:`connector:write` | Probe the upstream — `501` until a real adapter is registered |

¹ reads `Authorization` if present, but does not require it.

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

`next dev` runs on **:4000**, not the framework default, because the Go API already owns :3000 and both run at once. Open [`localhost:4000`](http://localhost:4000) and register a user; `/` redirects to `/login` or `/organizations` depending on session state, and every page (Organizations, Members, Roles, Audit Logs, Subscription) talks to the live API.

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

## Background worker

`cmd/worker` is a separate binary sharing the API's config, database pool, Redis client, and logger. It runs registered jobs on an interval, coordinated across replicas by a Redis lock (`worker:lock:<job>`, held for roughly one interval) so a job runs about once per interval **fleet-wide** rather than once per replica. A failed run releases the lock immediately so another replica can retry sooner. Each run gets a timeout and panic recovery — a broken job never takes the worker down.

Job stats (runs, failures, skips, last error, last duration) are exposed on the worker's own internal `GET /health` on `WORKER_PORT`. That endpoint is operational only and is not part of the public API contract.

```bash
make worker                  # cd apps/backend && go run ./cmd/worker
curl localhost:3001/health   # {status, uptime, jobs: [{name, runs, failures, skipped, ...}]}
```

The one job today is **session cleanup**: batched deletes of expired sessions, plus revoked sessions older than a retention window. Revoked sessions are kept for a while on purpose — that's what lets refresh-token reuse detection recognize a replayed token as a family reuse instead of an unknown session.

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
| `JWT_ACCESS_SECRET` / `JWT_REFRESH_SECRET` | — | **required**, min 32 chars each |
| `CONNECTOR_MASTER_KEY` | — | **required**, base64 of exactly 32 bytes — generate with `openssl rand -base64 32`. Wraps every connector's envelope-encryption data key; the value in `.env.example` is a working dev key, not a placeholder to leave in place for anything real. |
| `JWT_ACCESS_EXPIRES_IN` | `15m` | Go duration string |
| `JWT_REFRESH_EXPIRES_IN` | `604800` | **seconds**, not a duration string |
| `WORKER_PORT` | `3001` | worker's internal `/health` port |
| `WORKER_JOB_TIMEOUT` | `5m` | per-run timeout, any job |
| `SESSION_CLEANUP_INTERVAL` | `1h` | |
| `SESSION_CLEANUP_RETENTION` | `720h` | how long a revoked-but-unexpired session survives (30d) |
| `SESSION_CLEANUP_BATCH_SIZE` | `1000` | rows per `DELETE` statement |

`DATABASE_USER`/`PASSWORD`/`NAME` are the single source of the credentials: compose feeds them to the Postgres container *and* builds the api/worker `DATABASE_URL` from them, so the two can't drift. The `DATABASE_URL` in `.env` is the host-side one (via the published port); containers get theirs from compose using the `db` hostname. `.env.docker` supplies the rest of the env for the containerized api/worker — throwaway dev placeholders, and it deliberately omits `DATABASE_URL` for the same reason.

Redis keys used: `blacklist:<accessToken>` (15 min), `login:attempts:<email>` (max 5 / 15 min), `worker:lock:<jobName>` (TTL ≈ job interval).

## Docker

`apps/backend/Dockerfile` is a multi-stage build: a `golang:1.26-alpine` builder compiles all five binaries — `api`, `worker`, `migrate`, `seed`, and `healthcheck` — into one image, so any of them can be run by overriding the entrypoint. The runner is [`gcr.io/distroless/static-debian12:nonroot`](https://github.com/GoogleContainerTools/distroless). Distroless has no shell, which is why `HEALTHCHECK` runs the dedicated `healthcheck` binary rather than `curl`. The `worker` service reuses that same image with its entrypoint overridden to `/app/worker` and `HEALTHCHECK_PORT=3001`, so the shared healthcheck probes the worker's port instead of the API's.

`apps/frontend/Dockerfile` builds the Next.js standalone output on `node:${NODE_VERSION}-alpine`, where `NODE_VERSION` is a single build `ARG` shared by all three stages so the build and runtime majors can't diverge (override with `--build-arg NODE_VERSION=…`). The runner sets `HOSTNAME=0.0.0.0` explicitly — the standalone server otherwise binds to the container's assigned IP rather than all interfaces, and a loopback-based `HEALTHCHECK` would fail silently.

```bash
# individual images
docker build -t sapanjai-api:dev ./apps/backend
docker build -t sapanjai-web:dev ./apps/frontend

# full stack: db, redis, api, worker, web — web waits on api's HEALTHCHECK
docker compose up -d --build
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
- **Integration tests** in `apps/backend/internal/server/`, run against real Postgres and Redis (service containers in CI). These encode `docs/02-api-contract.md` directly — every route, its happy path, and each of its error codes. Auth edge cases specifically covered: refresh rotation, token-family reuse revoking the family, the rate limit tripping at 5 attempts, and logout revoking all sessions plus blacklisting the access token.

CI (`.github/workflows/ci.yml`) runs lint, backend test, frontend (lint / typecheck / test / build), and a Docker build job. `release.yml` pushes images to ghcr on a green CI run against `main`.

## Commands

```
make up              # start db + redis
make down            # stop all compose services

make migrate         # apply all pending migrations (goose up)
make migrate-down    # roll back the most recent migration
make migrate-status  # show migration status
make seed            # seed default plans (free/pro/enterprise) — idempotent

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
| [`apps/frontend/README.md`](apps/frontend/README.md) | Frontend proxy, token model, page map |
| [`k8s/README.md`](k8s/README.md) | Manifest layout and apply instructions |
