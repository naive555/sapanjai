# Migration Plan — Node/Elysia → Go/Echo

> Working outline for the planning model (Opus) to expand into a detailed implementation plan. Reads 01–03 first. The goal is behavioral parity per `02-api-contract.md`, then the frontend.

## Concept mapping (Node → Go)

| Source concept | Source implementation | Go replacement |
| -------------- | --------------------- | -------------- |
| HTTP framework | Elysia app + plugins with prefixes | Echo, `e.Group("/auth")` etc. |
| Request validation | TypeBox schemas in `model.ts` | DTO structs + `validator` tags; custom binder returning 422 `Validation failed` |
| Auth macros (`requireAuth`/`requireOrg`/`requirePermission`) | Elysia `.macro({ resolve })` injecting `user`/`organizationId`/`membership` | Echo middleware chain setting typed values in context; per-route wrapping |
| Global error handler | `onError` + ERROR_MAP of `Error('CODE')` strings | `apperror.Error{Code string}` type + custom `echo.HTTPErrorHandler` |
| Drizzle ORM + relational queries (`db.query.x.findFirst({with})`) | drizzle-orm/pg | sqlc queries with explicit JOINs; relations flattened into purpose-built queries |
| drizzle-kit migrations | 5 SQL files + meta journal | goose; port SQL files as `0001_*.sql` … with `-- +goose Up` headers (schema is identical) |
| ioredis + RedisAuth | blacklist + login-attempt counters | go-redis v9, same key names/TTLs |
| @elysiajs/jwt | HS256 sign/verify | golang-jwt/v5, same secrets/claims |
| bcryptjs cost 12 | password hashing | golang.org/x/crypto/bcrypt cost 12 (compatible hashes — existing users keep working) |
| pino + redaction | structured JSON logs | slog JSON handler; redact authorization/password fields in request logging middleware |
| request-id hook | read/generate UUID, echo header | echo middleware.RequestID with generator, or custom (preserve `x-request-id` name) |
| Swagger plugin | `/swagger` UI | swaggo annotations + echo-swagger at `/swagger` |
| Bun test + mock.module | unit tests with mocked db/redis | interfaces + hand mocks (or moq); integration tests against CI postgres/redis like source CI |
| Graceful shutdown | SIGTERM/SIGINT handlers | signal.NotifyContext + server.Shutdown |
| Seed script | insert 3 default plans, onConflictDoNothing | `cmd/seed` or Makefile target using sqlc, `ON CONFLICT DO NOTHING` |

## Suggested phases

### Phase 0 — Scaffold (small)
Monorepo root: Makefile, compose.yaml (db/redis from source values), .env.example. `backend/`: go.mod, Echo hello-world with `/health` (status + uptime), config loader, slog, Dockerfile. CI skeleton.

### Phase 1 — Data layer
Port migrations to goose (byte-identical schema). sqlc setup + queries for users/sessions. pgx pool + redis client with boot-time pings. Seed command (3 plans).

### Phase 2 — Auth (largest, most subtle)
Register/login/refresh/logout handlers + service; bcrypt; JWT pair issuance; session create/rotate with family reuse detection; Redis blacklist + login rate limiting; apperror + HTTPErrorHandler with the full ERROR_MAP; request-id middleware. **Parity tests**: token rotation, reuse → family revocation, rate limit at 5 attempts/15 min, logout revokes all sessions.

### Phase 3 — Org + guards
RequireAuth/RequireOrg middleware. Organizations CRUD-lite (create w/ owner membership in a tx, list-by-user, invite w/ role check + `max_members` enforcement, remove-member rules). Audit-log recording (best-effort, never fails the request).

### Phase 4 — RBAC + subscription + audit query
Roles/permissions/member_roles queries; `hasPermission` with `*` / exact / `resource:*` semantics; RequirePermission middleware. Subscription get/assign + limit resolution (plan merged with custom_limits). Audit-log query endpoint with filters.

### Phase 5 — Docs, deploy, CI parity
Swagger at `/swagger`. Dockerfile (distroless, healthcheck against /health). Port k8s manifests (api image swap; add web later). GitHub Actions: go vet/lint/test with postgres+redis services (mirror source ci.yml env).

### Phase 6 — Frontend
Next.js scaffold + shadcn. API client (refresh single-flight, org header). Pages in the order they unlock: auth → org switcher → members → roles → audit → subscription. Frontend Dockerfile + compose service + CI job.

### Phase 7 — Background worker
Fifth binary `cmd/worker` in the Go module: `worker.Job` interface + interval scheduler + Redis job lock (`worker:lock:<job>`), internal `/health` on `WORKER_PORT`. First job: session cleanup (batched delete of expired sessions and revoked ones past a 30d retention window) with supporting indexes (migration `00006`). Compose `worker` service and `k8s/worker/` Deployment. Not in the source app — new capability, not a port.

## Parity verification strategy

- A black-box HTTP test suite (Go httptest against the real stack, or a `hurl`/REST file collection) encoding `02-api-contract.md`: every route × happy path × each error code. Ideally runnable against **both** the Bun original and the Go port to prove parity before cutover.
- Same DB schema means a snapshot of the Bun app's database must work under the Go app unchanged (bcrypt hashes compatible; JWT secrets shared → live-migration/blue-green is possible with sessions intact, since tokens are HS256 with the same claims).

## Deviations to decide up front (see open questions in 03)

Subscription-assign guard · members list endpoint · refresh-token hashing · blacklist TTL from config. Default recommendation: fix all four, record each in docs as an intentional deviation with a CHANGELOG note.

## Definition of done (migration)

1. All endpoints pass the parity suite (status codes + messages + bodies).
2. `make up && make migrate && make seed && make dev` works from a clean clone.
3. CI green: lint + unit + integration (service containers).
4. Swagger UI serves the full API at `/swagger`.
5. Frontend exercises every module against the Go backend.
