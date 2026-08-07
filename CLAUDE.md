# CLAUDE.md

## What this repo is

**Sapanjai (HeartBridge)** — a multi-tenant B2B SaaS platform, **Go (backend) + Next.js (frontend)** in one monorepo. It starts from a platform-core template and business domains get added on top as new modules.

**What the core already provides** (all live end-to-end):

- **Auth** — `/auth/{register,login,refresh,logout}`: bcrypt (cost 12) password hashing, HS256 JWT access/refresh pair issuance, session create + rotation with token-family reuse detection (a reused/revoked refresh token revokes its whole family), Redis-backed access-token blacklist and login rate limiting (5 attempts / 15 min), best-effort audit logging of `user.register`/`user.login`.
- **Organizations** — `POST /organizations` (create + owner membership in a tx), `GET /organizations` (caller's memberships with embedded org), `GET /organizations/members` (active org's member roster), `POST /organizations/invite` (role check + `max_members` plan-limit enforcement), `DELETE /organizations/members/:userId` (role check + cannot-remove-owner), with best-effort `org.created`/`org.member.invited` audit logging. All `RequireAuth`/`RequireOrg`-guarded.
- **RBAC** — `/rbac/{roles,assign}`: create/list/update-permissions role CRUD-lite + role assignment. `RequirePermission(action)` implements `*`/exact/`resource:*` semantics with owner bypass; its first production caller is the Connectors module below.
- **Subscription** — `/subscription` (get with embedded plan) and `/subscription/assign` (upsert), plus `GET /plans` (`RequireAuth`-only, not org-scoped) so the frontend can populate a plan picker — plan ids are server-generated UUIDs with no other way to discover them.
- **Audit logs** — `GET /audit-logs`, filterable by `userId`/`action` with a capped `limit`, `RequireOrg`-guarded.
- **Connectors** — `/connectors` CRUD + `/connectors/:id/health-check`: org-scoped upstream connections (DB creds, API keys, ...) for the Managed MCP Gateway pivot (`docs/05-mcp-gateway.md`), gated by `RequirePermission("connector:{read,write,delete}")`. `config` is sealed at rest with envelope encryption (`internal/shared/envelope`: a fresh AES-256-GCM data key per row, wrapped by `CONNECTOR_MASTER_KEY`) and never returned by any endpoint or written to a log. This is the **generic skeleton only** — `type` is currently just `"generic"`, and the health-check `Checker` registry is empty (every check returns 501 `HEALTH_CHECK_UNSUPPORTED`) until a real adapter (FlowAccount, PEAK, ...) is implemented.
- **Background worker** — `cmd/worker`, a `worker.Job`-based scheduler (`internal/worker/`) running registered jobs on an interval behind a Redis lock (`worker:lock:<job>`, held for ~one interval so a multi-replica fleet still runs each job about once per interval — a failed run releases the lock immediately for an earlier retry), with per-run timeouts and panic recovery. The one shipped job, session cleanup (`internal/job/sessioncleanup/`), batch-deletes expired sessions and revoked sessions past a retention window (`SESSION_CLEANUP_*` env vars), backed by indexes on `sessions` from migration `00006`. The worker exposes its own internal `GET /health` on `WORKER_PORT` (job run/failure/skip stats) — not part of the public API contract — and ships in the same Docker image as the API with its entrypoint overridden (`compose.yaml`'s `worker` service, `k8s/worker/deployment.yaml`).
- **Frontend** — Next.js dashboard (`apps/frontend/`, App Router + shadcn/ui + TanStack Query) with pages for every module (auth, organizations + switcher, members, RBAC roles, audit logs, subscription), a client-token/single-flight-refresh auth model, and a runtime reverse proxy at `app/api/[...path]/route.ts`.
- **Ops** — every route documented at `/swagger` (swaggo-generated, spec committed to `apps/backend/docs/`); a distroless production image (`gcr.io/distroless/static-debian12:nonroot`) with a dedicated `cmd/healthcheck` binary since distroless has no shell for `HEALTHCHECK`; `golangci-lint` (v2 config schema) in `make lint` and CI; k8s manifests in `k8s/`; CI jobs for lint, backend, frontend, and a docker build, with `release.yml` pushing to ghcr on CI success against `main`.

See [`README.md`](README.md) for the quickstart, [`apps/frontend/README.md`](apps/frontend/README.md) for frontend-specific details. `docs/` holds the design documents behind the core — read them before implementing anything that touches it.

## Decided stack (do not re-litigate without the owner)

- **Backend**: Go, **Echo** framework, **sqlc + pgx/v5**, **goose** migrations, go-redis v9, golang-jwt/v5, bcrypt (cost 12), slog logging, swaggo (`/swagger`)
- **Frontend**: **Next.js (App Router) + TypeScript + Tailwind + shadcn/ui**, TanStack Query
- **Infra**: PostgreSQL 16, Redis 7, root **Makefile** + **docker-compose**, k8s manifests, GitHub Actions

## Layout

```
apps/backend/    Go API — cmd/{api,migrate,seed,worker}, internal/{config,server,middleware,module,worker,job,infra,shared}, migrations/
apps/frontend/   Next.js dashboard — app/{(auth),(dashboard)}/, lib/{api,auth,org}/, components/
docs/            01-source-analysis · 02-api-contract · 03-target-architecture · 04-migration-plan · 05-mcp-gateway
spikes/          throwaway feasibility spikes, each its own Go module — mcp-gateway/
```

`spikes/*` are **separate Go modules and deliberately outside the build**: `make test`/`make lint` and CI are scoped to `apps/backend`, and Go skips nested modules in `./...`. Nothing in `spikes/` is production code or a dependency of it — treat it as evidence attached to a design doc, and don't let it drift into the main pipeline.

## Ground rules

- **`docs/02-api-contract.md` is the source of truth** for the core routes, headers, status codes, and error messages. The Go backend must match it exactly for those routes; new business-domain routes belong in the contract too — add them there when you add them.
- Module convention: handler → service → sqlc queries per module (`auth`, `organization`, `rbac`, `auditlog`, `subscription`, `connector`, `health`). Services return `apperror` codes; a single Echo `HTTPErrorHandler` maps codes to HTTP responses. No HTTP concerns inside services. New domains follow the same shape.
- Auth guards are middleware: `RequireAuth`, `RequireOrg` (needs `x-organization-id` header + membership), `RequirePermission(action)`. Permission semantics: `*` > exact `resource:verb` > `resource:*` wildcard.
- Secrets stored at rest (currently: connector `config`) use envelope encryption via `internal/shared/envelope`: a per-row data key sealed by a `KeyProvider` holding the master key. The env-var-backed `EnvKeyProvider` is the only implementation today; a KMS/Vault provider is a one-line swap in `server.New`, not a caller-side change. Decrypted config must never leave the owning service — no response DTO field, no log line, no audit metadata.
- Master-key rotation is rotate-on-read, not a batch job: every sealed envelope carries the `kid` of the key that wrapped it, and `EnvKeyProvider` can hold retired keys (`CONNECTOR_MASTER_KEY_PREVIOUS`) for decrypt-only use. `Encryptor.OpenAndRotate` opens under whichever key the envelope names and, if that key is no longer primary, also returns a re-sealed replacement for the caller to persist (best-effort — a failed write just means the row offers the same rotation again next read). To rotate: generate a new key, move the current `CONNECTOR_MASTER_KEY` into `CONNECTOR_MASTER_KEY_PREVIOUS`, set `CONNECTOR_MASTER_KEY` to the new value, deploy — then drop the retired key once every row has been read at least once. As of this writing, `connector.Service` still calls plain `Open`; wiring it to `OpenAndRotate` and persisting the rotated blob is a follow-up (needs a narrow sqlc query that only touches `encrypted_config`, not `UpdateConnector`'s full row rewrite).
- Schema changes are additive-forward only: never edit an applied migration in `apps/backend/migrations/`, add a new goose migration and regenerate sqlc code (`make sqlc`).
- Audit-log writes are best-effort: log failures, never fail the request.
- Log redaction is centralized in `internal/shared/logger/redact.go`, wired into every logger via `slog.HandlerOptions.ReplaceAttr` — any attr key in the sensitive set (`authorization`, `password`, `token`/`access_token`/`refresh_token`, `cookie`, `secret`, `api_key`; matched case-insensitively, `-`/`_` stripped) logs as `[REDACTED]` no matter the call site. Add new sensitive keys there, not at call sites. Caveat: slog only exposes leaf keys, so **log individual fields, never a whole request/body struct** (`slog.Any("body", req)` would serialize `req.Password`). Request logging stays limited to method/uri/status/latency/request_id — no headers, no bodies — and the URI goes through `logger.SanitizeURI`.
- Redis keys: `blacklist:<accessToken>` (15 min), `login:attempts:<email>` (max 5 per 15 min), `worker:lock:<jobName>` (TTL ≈ job interval).
- Multi-step writes (org create + owner membership; session rotation) run in transactions.
- Background jobs implement `worker.Job` and are registered in `cmd/worker/main.go`. Job runs are guarded by a Redis lock (`worker:lock:<job>`, TTL ≈ one interval) so multiple replicas run a job about once per interval; a failed run releases the lock for an immediate retry. Job failures never crash the worker (panics are recovered).
- Frontend never calls the backend cross-origin: `app/api/[...path]/route.ts` is a runtime proxy reading `BACKEND_URL` per-request, deliberately **not** a `next.config.ts` `rewrites()` entry (those resolve at build time, not container-runtime). **No CORS middleware exists on the backend and none should be added** — the browser only ever calls same-origin `/api/*`.
- Frontend tokens: access token in memory only (never persisted), refresh token in `localStorage`, single-flight refresh on 401 (`apps/frontend/lib/api/client.ts`).

## Commands

```
make up        # start db + redis (docker compose)
make api       # run the Go API (terminal 1)
make web       # run the Next.js dev server on :4000 (terminal 2)
make worker    # run the background job runner (terminal 3, optional)
make migrate   # goose up          make seed   # default plans
make sqlc      # regen query code  make test   # go test ./... (backend only)
make swagger   # regen OpenAPI docs (swaggo)
make lint      # golangci-lint if installed, else go vet (backend only —
               # frontend lint/typecheck/test run from apps/frontend/: pnpm lint / pnpm exec tsc --noEmit / pnpm test)
```

No process manager runs backend + frontend (+ worker) together — `make api`, `make web`, `make worker` are separate terminals. Regenerating sqlc code requires the `sqlc` CLI (`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`); building/running the API does not. Full containerized stack: `docker compose up -d --build` (db, redis, api, worker, web).

## Environment

Copy `.env.example` → `.env` (and `.env.docker.example` → `.env.docker` for the containerized stack — both targets are git-ignored, only the templates are tracked). Required: `DATABASE_URL`, `REDIS_URL`, `JWT_ACCESS_SECRET`/`JWT_REFRESH_SECRET` (min 32 chars), `CONNECTOR_MASTER_KEY` (base64, exactly 32 bytes — `openssl rand -base64 32`), `JWT_ACCESS_EXPIRES_IN` (duration, default 15m), `JWT_REFRESH_EXPIRES_IN` (**seconds**, default 604800), `PORT` (3000), `LOG_LEVEL`, `APP_ENV`. Optional: `CONNECTOR_MASTER_KEY_PREVIOUS` (comma-separated base64 keys, retired `CONNECTOR_MASTER_KEY` values kept decrypt-only during a rotation — see the envelope-encryption ground rule above). Worker-only, all optional with defaults: `WORKER_PORT` (3001), `WORKER_JOB_TIMEOUT` (5m), `SESSION_CLEANUP_INTERVAL` (1h), `SESSION_CLEANUP_RETENTION` (720h/30d), `SESSION_CLEANUP_BATCH_SIZE` (1000).

## Testing expectations

- Unit tests per service with interface mocks for infra.
- Integration tests against real postgres+redis (CI service containers) encoding the contract in `docs/02` — every route × happy path × every error code.
- Auth edge cases that must be covered: refresh rotation, token-family reuse → revoke family, rate limit at 5 attempts, logout revokes all sessions + blacklists access token.
