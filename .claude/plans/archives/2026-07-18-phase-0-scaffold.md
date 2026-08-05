# Phase 0 — Scaffold (execution plan for Sonnet)

> **ARCHIVED — completed 2026-07-18.** All 5 steps executed and verified (build/vet/test green, live health-check + graceful-shutdown checks, Docker images built and run). Two follow-up changes landed *after* this plan and are not reflected in the text below:
> - `backend/`/`frontend/` were moved to `apps/backend/`/`apps/frontend/` (monorepo nesting).
> - The frontend package manager was switched from npm to pnpm.
>
> Current layout/commands are documented in the root `README.md` and `CLAUDE.md`, not this file. Kept here as a historical record only.

> **Read first:** `docs/01-source-analysis.md`, `docs/02-api-contract.md`, `docs/03-target-architecture.md`, `docs/04-migration-plan.md`, and `CLAUDE.md`. This plan implements **Phase 0 only** (the "Scaffold" phase from `docs/04-migration-plan.md`). Do NOT start auth, DB queries, or business logic — those are Phases 1+.
>
> **Goal of Phase 0:** a runnable skeleton. From a clean clone, `make up && make dev` starts Postgres + Redis + a Go API that serves `GET /health` returning `{"status":"ok","uptime":<seconds>}`, plus an empty Next.js app. No auth, no migrations content, no real DB queries yet — just the wiring that everything else hangs off.
>
> **Environment facts (already verified):** Go 1.26.2 is installed. `backend/` and `frontend/` are empty. Root has `CLAUDE.md`, `docs/`, `.claude/`, `.git`. Source project is at `../controlplane-api`. Platform is Windows; the repo uses a Bash-capable shell. Module path for Go: use `github.com/controlplane/backend` (placeholder — fine for a local monorepo).

---

## Scope boundary (what Phase 0 does and does NOT include)

**IN scope:**
- Monorepo glue: root `Makefile`, `compose.yaml`, `.env.example`, `.gitignore`, `.dockerignore`.
- Backend Go module: config loader, slog logger, Echo server with middleware wiring, `apperror` package + custom `HTTPErrorHandler` (skeleton), request-id middleware, `/health` endpoint, graceful shutdown, `cmd/api/main.go`, backend `Dockerfile`, `.air.toml`.
- Empty-but-present directory structure for future modules (with `.gitkeep`).
- Frontend: Next.js app (App Router, TS, Tailwind), a placeholder landing page, frontend `Dockerfile`.
- CI skeleton: `.github/workflows/ci.yml` with a backend job (build + vet + test) and a frontend job (lint + build).

**OUT of scope (later phases — do not implement):**
- Any migration SQL content, goose setup, sqlc, pgx pool queries (Phase 1).
- Auth, JWT, bcrypt, Redis auth helpers, any module besides health (Phases 2–4).
- shadcn/ui components, API client, dashboard pages (Phase 6).
- k8s manifest porting (Phase 5).

> Note: the API container **does** depend on db+redis in compose so the topology is real, and `main.go` **does** open a pgx pool and a redis client and ping them at boot (so Phase 1/2 only add queries, not wiring). But no schema/queries exist yet — pinging a running empty Postgres/Redis succeeds, which is all Phase 0 needs.

---

## Step-by-step

### Step 1 — Root monorepo glue

Create these at repo root (`controlplane/`):

**`.env.example`** (mirror source values from `../controlplane-api/.env.example`, renaming `NODE_ENV`→`APP_ENV`):
```
APP_NAME=controlplane-api
APP_ENV=development
PORT=3000
LOG_LEVEL=debug

DATABASE_USER=username
DATABASE_PASSWORD=password
DATABASE_NAME=controlplane
DATABASE_URL=postgres://username:password@localhost:5432/controlplane?sslmode=disable

REDIS_URL=redis://localhost:6379

JWT_ACCESS_SECRET=your-access-secret-min-32-chars-long
JWT_REFRESH_SECRET=your-refresh-secret-min-32-chars-long
JWT_ACCESS_EXPIRES_IN=15m
JWT_REFRESH_EXPIRES_IN=604800
```
Also create `.env` as a copy (gitignored) so `make dev` works immediately. Add a `.env.docker` variant where host `localhost` becomes service names `db`/`redis` (used by the api container).

**`.gitignore`** — include: `.env`, `.env.docker` if you don't want it committed (commit it; it has no real secrets — decide and note it), `node_modules/`, `frontend/.next/`, `frontend/out/`, `backend/tmp/` (air), `backend/bin/`, `*.log`, Go build cache is global so no need.

**`.dockerignore`** at root and in each of `backend/`, `frontend/`: exclude `.git`, `node_modules`, `.next`, `tmp`, `.env`.

**`compose.yaml`** — 4 services, model on `../controlplane-api/compose.yaml`:
- `db`: `postgres:16-alpine`, env from `DATABASE_USER/PASSWORD/NAME`, port `5432:5432`, named volume `pgdata`, healthcheck `pg_isready -U ${DATABASE_USER}`, `restart: unless-stopped`.
- `redis`: `redis:7-alpine`, port `6379:6379`, healthcheck `redis-cli ping`, `restart: unless-stopped`.
- `api`: `build: ./backend`, port `3000:3000`, `env_file: .env.docker`, `depends_on` db+redis `condition: service_healthy`, `restart: unless-stopped`.
- `web`: `build: ./frontend`, port `4000:3000` (Next default port 3000 inside container; expose on host 4000 to avoid clashing with api), `depends_on: [api]`. Keep it minimal; it can be commented-in once the frontend builds.
- `volumes: { pgdata: {} }`.

**`Makefile`** — language-neutral targets. Use `.PHONY`. On Windows the repo's shell is Bash-capable; keep recipes POSIX. Targets:
```
up:        docker compose up -d db redis
down:      docker compose down
dev:       run backend (air) and frontend (next dev) concurrently — see note
api:       cd backend && go run ./cmd/api
web:       cd frontend && npm run dev
build:     cd backend && go build -o bin/api ./cmd/api ; cd frontend && npm run build
test:      cd backend && go test ./...
lint:      cd backend && go vet ./...   (golangci-lint added in Phase 5)
tidy:      cd backend && go mod tidy
fmt:       cd backend && go fmt ./...
```
For `dev` concurrency without extra tooling, document that the developer runs `make api` and `make web` in two terminals, OR provide a `dev` recipe that backgrounds one process — keep it simple: two-terminal instructions in a comment, and make `dev` an alias that prints the instruction. (Do not add a Node process-manager dependency in Phase 0.)

### Step 2 — Backend Go module skeleton

From `backend/`:
```
go mod init github.com/controlplane/backend
```
Set `go 1.26` in `go.mod`. Add dependencies (let `go get` pick current versions):
- `github.com/labstack/echo/v4` (framework + its `middleware` subpackage)
- `github.com/jackc/pgx/v5` and `github.com/jackc/pgx/v5/pgxpool`
- `github.com/redis/go-redis/v9`
- `github.com/caarlos0/env/v11` **or** hand-roll env parsing with `os.Getenv` (prefer no dependency for config in Phase 0 — use stdlib `os` + a small validator). Choose stdlib to keep deps minimal.

Create this exact tree (empty dirs get a `.gitkeep`):
```
backend/
├── go.mod / go.sum
├── .air.toml
├── Dockerfile
├── .dockerignore
├── cmd/api/main.go
├── internal/
│   ├── config/config.go
│   ├── server/server.go
│   ├── middleware/requestid.go
│   ├── module/
│   │   ├── health/handler.go
│   │   ├── auth/.gitkeep
│   │   ├── organization/.gitkeep
│   │   ├── rbac/.gitkeep
│   │   ├── auditlog/.gitkeep
│   │   └── subscription/.gitkeep
│   ├── domain/.gitkeep
│   ├── infra/
│   │   ├── database/database.go        (pgx pool constructor + Ping only)
│   │   └── redis/redis.go              (client constructor + Ping only)
│   └── shared/
│       ├── apperror/apperror.go
│       └── logger/logger.go
└── migrations/.gitkeep                 (goose SQL added in Phase 1)
```

**`internal/config/config.go`** — a `Config` struct with fields: `AppName, AppEnv, Port, LogLevel, DatabaseURL, RedisURL, JWTAccessSecret, JWTRefreshSecret, JWTAccessExpiresIn (time.Duration), JWTRefreshExpiresIn (time.Duration from seconds)`. A `Load() (*Config, error)` that reads env, applies defaults (Port=3000, AppEnv=development, LogLevel=info, JWTAccessExpiresIn=15m, JWTRefreshExpiresIn=604800s), and **validates**: DATABASE_URL and REDIS_URL required; both JWT secrets required and ≥32 chars. Return a clear error listing all missing/invalid vars (fail fast — improvement over source which only checked REDIS_URL). Parse `JWT_ACCESS_EXPIRES_IN` as a Go duration string (`15m`), and `JWT_REFRESH_EXPIRES_IN` as integer seconds → `time.Duration`.

**`internal/shared/logger/logger.go`** — build a `*slog.Logger`. If `AppEnv == "development"` use a text handler (human-readable); else JSON handler. Level from `LogLevel` (map fatal/error/warn/info/debug/trace → slog levels; slog has no trace/fatal, map trace→debug, fatal→error). Return the logger; do NOT set redaction yet (redaction lands with request-logging middleware in Phase 2) but leave a `// TODO(phase2): redact authorization/password` note.

**`internal/shared/apperror/apperror.go`** — define:
```go
type Error struct { Code string }
func (e *Error) Error() string { return e.Code }
func New(code string) *Error { return &Error{Code: code} }
```
Plus a map `codeToHTTP map[string][2]...` is awkward; instead export `var Map = map[string]struct{ Status int; Message string }{ ... }` populated with the FULL table from `docs/02-api-contract.md` (EMAIL_TAKEN…NOT_FOUND). Provide `func Resolve(code string) (int, string)` returning the mapping or `(500, "Internal server error")` for unknown. This is used by the server's error handler. (No service throws these yet in Phase 0 — the map just needs to exist and be correct.)

**`internal/middleware/requestid.go`** — Echo middleware: read `X-Request-Id` header; if empty, generate a UUID (`github.com/google/uuid` or `crypto/rand`-based — prefer `github.com/google/uuid` for clarity, add the dep). Store on context and set the response header `X-Request-Id` to the same value (echo it back — matches source behavior).

**`internal/infra/database/database.go`** — `func New(ctx, url) (*pgxpool.Pool, error)` that creates a pool and `Ping`s it. Nothing else.

**`internal/infra/redis/redis.go`** — `func New(ctx, url) (*redis.Client, error)` parsing `redis.ParseURL`, then `Ping`. Nothing else.

**`internal/module/health/handler.go`** — capture a package-level or injected `startTime := time.Now()` at server construction. Handler returns JSON `{"status":"ok","uptime":<float seconds since start>}` (source uses `process.uptime()` = seconds as float). Register on `GET /health`. Mark route as public (no auth — there is no auth yet anyway).

**`internal/server/server.go`** — `func New(cfg *Config, log *slog.Logger, pool *pgxpool.Pool, rdb *redis.Client) *echo.Echo`:
- Create `echo.New()`, set `e.HideBanner = true`.
- Set `e.HTTPErrorHandler` to a custom handler: if the error is `*echo.HTTPError`, respond with its code/message; if it's `*apperror.Error`, use `apperror.Resolve`; else log at error level and respond 500 `Internal server error`. Also handle Echo's default 404 → `{"message":"Route not found"}` with 404, and bind/validation errors → 422 `Validation failed` (full wiring lands in Phase 2; a correct default is enough now). Keep JSON error body shape consistent (`{"message": "..."}`).
- Middleware order: `middleware.Recover()`, request-id (custom), request logger (use `echo/middleware.RequestLoggerWithConfig` feeding slog — basic version; redaction in Phase 2).
- Mount health handler.
- Return `e`.

**`cmd/api/main.go`** — bootstrap mirroring source `src/index.ts`:
1. `config.Load()` → fatal on error.
2. Build logger.
3. `ctx` with `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`.
4. Open redis (Ping) → log "Redis connected"; open pgx pool (Ping) → log "Database connected". Fatal on failure.
5. Build server via `server.New(...)`.
6. Start `e.Start(":"+port)` in a goroutine; log the listen address.
7. Block on `<-ctx.Done()`; then graceful shutdown: `e.Shutdown(shutdownCtx)` (5s timeout), close pool, close redis. Log "Shutdown complete".

**`.air.toml`** — standard air config building `./cmd/api`, output to `tmp/`, watch `.go` files. (Air is a dev-only tool; document `go install github.com/air-verse/air@latest` in README/CLAUDE if not present. `make api` uses plain `go run` and does NOT require air, so air is optional.)

**`backend/Dockerfile`** — multi-stage:
- Stage `builder`: `golang:1.26-alpine`, copy `go.mod/go.sum`, `go mod download`, copy source, `CGO_ENABLED=0 go build -o /out/api ./cmd/api`.
- Stage runner: `gcr.io/distroless/static-debian12` (or `alpine:3.20` if distroless is inconvenient), copy binary, `EXPOSE 3000`, healthcheck hitting `/health` (distroless has no shell — use an `alpine` runner if you want a shell-based HEALTHCHECK; otherwise omit HEALTHCHECK in the image and rely on compose/k8s probes). `ENTRYPOINT ["/api"]`.

After writing code: `cd backend && go mod tidy && go vet ./... && go build ./...` must succeed.

### Step 3 — Frontend Next.js skeleton

From `frontend/`, scaffold Next.js (App Router, TypeScript, Tailwind, ESLint, `src/` dir optional — follow `docs/03` layout using `app/`):
```
npx create-next-app@latest . --ts --tailwind --eslint --app --no-src-dir --use-npm --import-alias "@/*"
```
(Run non-interactively; if the CLI prompts, pass flags to avoid prompts. If `create-next-app` refuses because the dir has a `.gitkeep`, remove it first.)

Then:
- Replace `app/page.tsx` with a minimal placeholder: an `<h1>controlplane</h1>` and a line noting it's the dashboard shell (Phase 6 builds real pages). Keep the default Tailwind setup.
- Add an `.env.local.example` with `NEXT_PUBLIC_API_URL=http://localhost:3000`.
- Add `frontend/Dockerfile` (multi-stage: `node:22-alpine` deps → build → runner running `next start`). Keep it but it's optional to wire into compose in Phase 0 (can be left commented in `compose.yaml`).
- Ensure `npm run build` and `npm run lint` succeed.

Do NOT add shadcn/ui, TanStack Query, or an API client yet — that's Phase 6.

### Step 4 — CI skeleton

Create `.github/workflows/ci.yml` with two jobs (model env on `../controlplane-api/.github/workflows/ci.yml` but for Go/Node):
- `backend`: `runs-on: ubuntu-latest`, `actions/setup-go@v5` with `go-version: '1.26'`, working-directory `backend`, steps: `go mod download`, `go vet ./...`, `go build ./...`, `go test ./...`. (No postgres/redis services needed in Phase 0 since there are no integration tests yet; add service containers in Phase 2/5.)
- `frontend`: `actions/setup-node@v4` node 22, working-directory `frontend`, `npm ci`, `npm run lint`, `npm run build`.

### Step 5 — Docs touch-up

- Update root `CLAUDE.md` "Status" line to note Phase 0 scaffold is complete and how to run it.
- Add a short `README.md` at repo root: what the monorepo is (one line, point to `docs/`), prerequisites (Go 1.26+, Node 22+, Docker), and the quickstart:
  ```
  cp .env.example .env
  make up
  make api      # terminal 1 — Go API on :3000
  make web      # terminal 2 — Next.js on :3000 (dev) / mapped to :4000 in docker
  curl localhost:3000/health
  ```

---

## Acceptance criteria (verify before declaring Phase 0 done)

1. `cd backend && go build ./... && go vet ./... && go test ./...` all pass (tests may be a single trivial health-handler test — add one: assert `/health` returns 200 and `status":"ok"`).
2. `make up` starts db + redis; both report healthy (`docker compose ps`).
3. With `.env` present and db/redis up, `make api` boots, logs "Redis connected" and "Database connected", and `curl -s localhost:3000/health` returns `{"status":"ok","uptime":<number>}`.
4. `curl -i localhost:3000/nonexistent` returns 404 with body message `Route not found`.
5. Response to `/health` includes an `X-Request-Id` header; sending a request with `X-Request-Id: abc` echoes `abc` back.
6. Ctrl-C on the api triggers graceful shutdown logs (no panic).
7. `cd frontend && npm run build` succeeds; `npm run dev` serves the placeholder page.
8. `docker compose build api` succeeds (image builds).
9. CI file is valid YAML and both jobs are defined.

## Guardrails / notes for the executor

- **Match `docs/02-api-contract.md` exactly** for the two behaviors Phase 0 touches: `/health` body shape and the 404 `Route not found` / 422 `Validation failed` / 400 `Invalid request body` global messages. Everything else is stubs.
- Keep services free of HTTP types; the health handler is the only handler and it may be simple.
- Do not invent business logic, DB tables, or auth. If something seems to need a table/query, it belongs to a later phase — leave a `// TODO(phaseN)` and stop.
- Minimize dependencies: stdlib config, `echo`, `pgx`, `go-redis`, `google/uuid`. Nothing else in Phase 0.
- Prefer small, compilable commits per step; run `go build ./...` after each Go file group.
- If any tool is missing on the machine (air), make it optional — core flow must work with `go run` and `npm` only.
- When Phase 0 is complete and all acceptance criteria pass, STOP and report what was created + how to run it. Do not begin Phase 1.
```
