# Phase 1 — Data layer (execution plan for Sonnet)

> **ARCHIVED — completed 2026-07-19.** All 7 steps executed and verified against a live Postgres 16 + Redis 7 (docker compose):
> - Migrations ported to goose (`00001`–`00005`), DDL confirmed statement-identical to the source Drizzle SQL; full up/down/up round-trip verified live.
> - `cmd/migrate` (embedded FS, no external goose CLI needed) + `make migrate`/`migrate-down`/`migrate-status`.
> - sqlc wired up (`sqlc.yaml`, queries for `users`/`sessions`/`plans`, generated code committed). Note: the `timestamp` override required the `pg_catalog.timestamp` db_type key — a bare `"timestamp"` is silently ignored by sqlc's postgresql engine.
> - `database.Store` (pool + `*db.Queries` + `WithTx`) added; `cmd/api` boot flow deliberately left untouched per scope.
> - `cmd/seed` inserts free/pro/enterprise idempotently; verified live (second run is a no-op).
> - Integration test (`database_test.go`, gated on `DATABASE_URL`) covers the full user/session/plan round-trip; verified passing both with and without the env var set.
> - CI (`ci.yml`) updated with postgres/redis service containers + a migrate step; validated as well-formed YAML and the whole backend job simulated locally end-to-end.
> - `CLAUDE.md`/`README.md` status and command references updated.
> - Confirmed no regression: `/health` and the global 404 body are byte-identical to Phase 0.
>
> Current layout/commands are documented in the root `README.md` and `CLAUDE.md`, not this file. Kept here as a historical record only.

> **Read first:** `docs/01-source-analysis.md`, `docs/02-api-contract.md`, `docs/03-target-architecture.md`, `docs/04-migration-plan.md`, and `CLAUDE.md`. This plan implements **Phase 1 only** (the "Data layer" phase from `docs/04-migration-plan.md`). Phase 0 (scaffold) is complete and merged. Do NOT start auth handlers/services, JWT, bcrypt, Redis auth helpers, or any business logic — those are Phase 2+.
>
> **Goal of Phase 1:** turn the empty DB wiring from Phase 0 into a real, migrated, query-able data layer. After this phase, from a clean clone:
> `make up && make migrate && make seed` creates the full 10-table schema (byte-identical to the source) and inserts the 3 default plans, and the Go backend exposes type-safe **sqlc-generated queries for `users` and `sessions`** (plus a `plans` upsert used by seeding) ready for Phase 2 to consume. No HTTP behavior changes — `/health` is still the only route.

---

## Current state (verified — do not re-discover)

- Backend lives at `apps/backend/`, module path `github.com/controlplane/backend`, Go 1.26.2.
- `go.mod` already has: `echo/v4`, `pgx/v5`, `go-redis/v9`, `google/uuid`, `joho/godotenv`. `golang.org/x/crypto` is present (indirect).
- `apps/backend/internal/infra/database/database.go` — `New(ctx, url) (*pgxpool.Pool, error)` (pool + ping only).
- `apps/backend/internal/infra/redis/redis.go` — `New(ctx, url) (*redis.Client, error)` (client + ping only).
- `apps/backend/cmd/api/main.go` — boots config → redis → pool → `server.New(cfg, log, pool, rdb)` → graceful shutdown. **Leave the api boot flow unchanged in Phase 1** (it already opens the pool; Phase 2 will thread `Queries` through `server.New`).
- `apps/backend/migrations/` contains only `.gitkeep`.
- Root `Makefile` targets: `up down dev api web build test lint tidy fmt`. No `migrate`/`seed`/`sqlc` yet.
- `.env` / `.env.example` already define `DATABASE_URL` (`postgres://username:password@localhost:5432/controlplane?sslmode=disable`) and the JWT vars.

### Source of truth for the schema

The source Drizzle-generated SQL migrations at
`../controlplane-api/src/infrastructure/database/migrations/` (`0000`–`0004`) are the **byte-identical** DDL to reproduce. Their combined schema (10 tables) is described in `docs/01-source-analysis.md`. The seed data (3 plans) is `../controlplane-api/src/infrastructure/database/seed.ts`.

The auth service the queries must serve is `../controlplane-api/src/modules/auth/service.ts` — Phase 1 provides exactly the user/session operations it performs (find-user-by-email, create-user, find-session-by-refresh-token, revoke-by-id, revoke-by-family, revoke-all-active-for-user, create-session).

---

## Scope boundary

**IN scope:**
- Port all 5 source migration files to **goose** SQL migrations under `apps/backend/migrations/` (up **and** down), producing the identical 10-table schema.
- A `cmd/migrate` runner (goose as a **library** with embedded SQL — no goose CLI install required) + `make migrate` / `make migrate-down` targets.
- **sqlc** setup: `sqlc.yaml`, query files for `users`, `sessions`, and `plans`, committed generated code, `make sqlc` target.
- A thin `Store` in `internal/infra/database` that wraps the pool, exposes the generated `*Queries`, and provides a `WithTx` transaction helper (Phase 2 session rotation / Phase 3 org-create need it).
- A `cmd/seed` command that inserts the 3 default plans idempotently (`ON CONFLICT (name) DO NOTHING`) + `make seed` target.
- One integration test (gated on `DATABASE_URL`) proving migrate → seed → a user/session round-trip works.
- Docs touch-up (CLAUDE status line, README quickstart, `.env.example` note if needed).

**OUT of scope (later phases — do not implement):**
- Any auth/JWT/bcrypt logic, Redis blacklist / login-attempt helpers (Phase 2).
- Queries for organizations, memberships, roles, permissions, member_roles, audit_logs, org_subscriptions **beyond the `plans` upsert for seeding** (added in their owning phases 3–4).
- Wiring `Queries` into `server.New` / handlers (Phase 2).
- Threading a real query into `/health` — health stays as-is.

> If you find yourself writing an auth service, a handler, or a query for a table other than users/sessions/plans, STOP — it belongs to a later phase.

---

## Step-by-step

### Step 1 — Port migrations to goose

Add the goose library dependency:
```
cd apps/backend && go get github.com/pressly/goose/v3
```

Create 5 files in `apps/backend/migrations/`, one per source file, **preserving the exact DDL** (columns, types, `DEFAULT`s, `NOT NULL`, `UNIQUE`/FK constraint names, `ON DELETE cascade`). Transform each source file by:
1. Wrapping the statements between `-- +goose Up` and `-- +goose Down`.
2. **Removing the Drizzle `--> statement-breakpoint` comment lines** (goose splits statements on `;`; these markers are not needed and `-->` is not a goose annotation).
3. Writing a `-- +goose Down` section that `DROP TABLE ... CASCADE` in reverse dependency order.

Renumber with zero-padded, goose-friendly versions starting at **1** (goose treats version 0 as the baseline, so do not keep the source's `0000`). Suggested names (each maps 1:1 to a source file — keep statement order within each file identical):

| New goose file | Source file | Tables (Up) |
| --- | --- | --- |
| `00001_users_sessions.sql` | `0000_lyrical_shriek.sql` | `users`, `sessions` (+ sessions→users FK) |
| `00002_organizations_memberships.sql` | `0001_smart_baron_zemo.sql` | `organizations`, `memberships` (+ 2 FKs) |
| `00003_roles_permissions_member_roles.sql` | `0002_ambiguous_hercules.sql` | `roles`, `permissions`, `member_roles` (+ 4 FKs) |
| `00004_audit_logs.sql` | `0003_mighty_dark_beast.sql` | `audit_logs` |
| `00005_plans_org_subscriptions.sql` | `0004_optimal_lester.sql` | `plans`, `org_subscriptions` (+ 2 FKs) |

Example of the transform for the first file (Up body is the verbatim source DDL minus the breakpoint markers; Down drops in reverse order):
```sql
-- +goose Up
CREATE TABLE "users" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"email" text NOT NULL,
	"password_hash" text NOT NULL,
	"display_name" text,
	"is_verified" boolean DEFAULT false NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "users_email_unique" UNIQUE("email")
);
CREATE TABLE "sessions" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"user_id" uuid NOT NULL,
	"refresh_token" text NOT NULL,
	"family" uuid NOT NULL,
	"is_revoked" boolean DEFAULT false NOT NULL,
	"expires_at" timestamp NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "sessions_refresh_token_unique" UNIQUE("refresh_token")
);
ALTER TABLE "sessions" ADD CONSTRAINT "sessions_user_id_users_id_fk" FOREIGN KEY ("user_id") REFERENCES "public"."users"("id") ON DELETE cascade ON UPDATE no action;

-- +goose Down
DROP TABLE "sessions";
DROP TABLE "users";
```
Apply the same pattern to the other four (reverse-order drops: e.g. file 3 drops `member_roles`, `permissions`, `roles`; file 5 drops `org_subscriptions`, `plans`).

> **Fidelity check:** after transforming, diff each Up body against the corresponding source `.sql` (ignoring the added goose headers and removed `--> statement-breakpoint` lines) — the CREATE/ALTER statements must be character-identical, including the quoted constraint names. The schema is the contract.

### Step 2 — Migration runner (`cmd/migrate`) using embedded SQL

Create `apps/backend/migrations/embed.go`:
```go
// Package migrations embeds the goose SQL files so the migrate command and
// tests can run migrations without the goose CLI or filesystem access.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
```

Create `apps/backend/cmd/migrate/main.go` — a small CLI that runs goose against `DATABASE_URL` using the embedded FS and the `pgx` stdlib driver:
- Load `.env` the same way `cmd/api` does (reuse the `godotenv` fallback list `../../.env`, `../.env`, `.env`), or call `config.Load()` and use `cfg.DatabaseURL` (preferred — reuses validation).
- Open a `*sql.DB` via `database/sql` with the pgx stdlib driver: `import _ "github.com/jackc/pgx/v5/stdlib"` and `sql.Open("pgx", cfg.DatabaseURL)`. (goose needs a `*sql.DB`, not a pgxpool.)
- `goose.SetBaseFS(migrations.FS)`, `goose.SetDialect("postgres")`.
- Parse the first CLI arg as the goose command (`up`, `down`, `status`, `version`, `reset`); default to `up`. Pass remaining args through. Run `goose.RunContext(ctx, command, db, ".", args...)`.
- Log applied migrations via the app logger or goose's default; exit non-zero on error.

Add `github.com/jackc/pgx/v5/stdlib` (already available via the pgx module — just the import; run `go mod tidy`).

Add Makefile targets:
```makefile
## Apply all pending database migrations
migrate:
	cd apps/backend && go run ./cmd/migrate up

## Roll back the most recent migration
migrate-down:
	cd apps/backend && go run ./cmd/migrate down

## Show migration status
migrate-status:
	cd apps/backend && go run ./cmd/migrate status
```
Add `migrate migrate-down migrate-status` to the `.PHONY` line.

### Step 3 — sqlc setup + query files

Create `apps/backend/sqlc.yaml` (v2, pgx/v5 engine, schema read from the goose migrations, generated code into `internal/infra/database/db`):
```yaml
version: "2"
sql:
  - engine: "postgresql"
    schema: "migrations"
    queries: "internal/infra/database/queries"
    gen:
      go:
        package: "db"
        out: "internal/infra/database/db"
        sql_package: "pgx/v5"
        emit_interface: true          # Querier interface — Phase 2 unit tests mock it
        emit_json_tags: true
        emit_pointers_for_null_types: true
        overrides:
          - db_type: "uuid"
            go_type: "github.com/google/uuid.UUID"
          - db_type: "pg_catalog.timestamp"
            go_type: "time.Time"
          - db_type: "jsonb"
            go_type: "encoding/json.RawMessage"
```
> Note: the `pg_catalog.` prefix is required for the `timestamp` override to take effect — a bare `db_type: "timestamp"` is silently ignored by sqlc's postgresql engine (verified empirically). `uuid` and `jsonb` do not need the prefix.
Notes:
- `schema: "migrations"` — sqlc understands the `-- +goose Up`/`Down` annotations and applies only the Up DDL. Do not maintain a second schema file.
- `emit_interface: true` is required so Phase 2 can mock `db.Querier` for service unit tests (per CLAUDE testing expectations).
- Nullable `uuid`/`timestamp` columns will emit pointers because of `emit_pointers_for_null_types`; that's fine for users/sessions (only `display_name` is nullable among them).

Create `apps/backend/internal/infra/database/queries/users.sql`:
```sql
-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name)
VALUES ($1, $2, $3)
RETURNING *;
```

Create `apps/backend/internal/infra/database/queries/sessions.sql` (mirrors `auth/service.ts` exactly):
```sql
-- name: CreateSession :one
INSERT INTO sessions (user_id, refresh_token, family, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetSessionByRefreshToken :one
SELECT * FROM sessions WHERE refresh_token = $1;

-- name: RevokeSessionByID :exec
UPDATE sessions SET is_revoked = true WHERE id = $1;

-- name: RevokeSessionFamily :exec
UPDATE sessions SET is_revoked = true WHERE family = $1;

-- name: RevokeAllUserSessions :exec
UPDATE sessions SET is_revoked = true
WHERE user_id = $1 AND is_revoked = false;
```

Create `apps/backend/internal/infra/database/queries/plans.sql` (only what seeding needs now):
```sql
-- name: UpsertPlan :exec
INSERT INTO plans (name, limits)
VALUES ($1, $2)
ON CONFLICT (name) DO NOTHING;

-- name: GetPlanByName :one
SELECT * FROM plans WHERE name = $1;
```

Add the `make sqlc` target (codegen is a dev tool; the generated code is committed, so builds don't need sqlc installed):
```makefile
## Regenerate sqlc query code (requires sqlc installed)
sqlc:
	cd apps/backend && sqlc generate
```
Add `sqlc` to `.PHONY`. Document in README/CLAUDE that sqlc is installed with `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest` (or run via the `sqlc/sqlc` Docker image).

Generate the code:
```
cd apps/backend && sqlc generate
```
This produces `internal/infra/database/db/{db.go,models.go,querier.go,users.sql.go,sessions.sql.go,plans.sql.go}`. **Commit the generated files.** Run `go mod tidy` (sqlc pgx output imports `pgx/v5`, `google/uuid`, already present).

### Step 4 — `Store` wrapper (pool + Queries + tx helper)

Update `apps/backend/internal/infra/database/database.go` — keep `New` returning the pool, and add a `Store`:
```go
// Store bundles the pgx pool with the sqlc-generated Queries and provides a
// transaction helper. Handlers/services in later phases depend on *Store (or
// the db.Querier interface) rather than the raw pool.
type Store struct {
	Pool    *pgxpool.Pool
	*db.Queries
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool, Queries: db.New(pool)}
}

// WithTx runs fn inside a transaction, passing a *db.Queries bound to the tx.
// Commits on nil error, otherwise rolls back. Used by Phase 2 session rotation
// and Phase 3 org-create + owner-membership writes.
func (s *Store) WithTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op
	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
```
Import the generated package `github.com/controlplane/backend/internal/infra/database/db`. `db.New` accepts a `DBTX`, which `*pgxpool.Pool` satisfies; `db.Queries.WithTx(tx)` accepts a `pgx.Tx`.

> Do **not** yet change `cmd/api/main.go` or `server.New` to build/pass the `Store`. Phase 2 does that wiring when a handler first needs it. Phase 1 only needs `NewStore` to exist and compile (the seed command and the integration test exercise it).

### Step 5 — Seed command (`cmd/seed`)

Create `apps/backend/cmd/seed/main.go` mirroring `seed.ts`:
- Load config (`config.Load()`), open a pool via `database.New(ctx, cfg.DatabaseURL)`, build `store := database.NewStore(pool)`.
- Define the 3 plans and call `store.UpsertPlan` for each (idempotent via `ON CONFLICT DO NOTHING`):
  ```go
  plans := []struct {
      name   string
      limits map[string]int
  }{
      {"free", map[string]int{"max_members": 5, "max_roles": 3}},
      {"pro", map[string]int{"max_members": 50, "max_roles": 20}},
      {"enterprise", map[string]int{"max_members": -1, "max_roles": -1}},
  }
  ```
  Marshal `limits` to JSON (`encoding/json`) → pass as `json.RawMessage` to match the `jsonb` override. Log `"Seeding plans..."` / `"Done"` like the source.
- Close the pool; exit 0 on success, non-zero on error.

Add the Makefile target:
```makefile
## Seed default plans (free / pro / enterprise) — idempotent
seed:
	cd apps/backend && go run ./cmd/seed
```
Add `seed` to `.PHONY`.

### Step 6 — Integration test (gated on DATABASE_URL)

Add `apps/backend/internal/infra/database/database_test.go` — a single integration test that runs only when `DATABASE_URL` is set (skip otherwise so unit `go test ./...` stays green without a DB):
- `if os.Getenv("DATABASE_URL") == "" { t.Skip("DATABASE_URL not set; skipping integration test") }`.
- Run migrations up against the DB using `migrations.FS` + goose (same as `cmd/migrate`), so the test is self-contained.
- Build a `Store`, then exercise the round-trip:
  1. `CreateUser` with a unique email → assert returned row has the email + non-nil `id`.
  2. `GetUserByEmail` → same id.
  3. `CreateSession` with a random `family` UUID and a future `expires_at` → `GetSessionByRefreshToken` → assert `is_revoked == false`.
  4. `RevokeSessionFamily` → re-fetch → `is_revoked == true`.
  5. `UpsertPlan("free", ...)` twice → `GetPlanByName("free")` succeeds (idempotency: no error on the second insert).
- Use a unique email per run (e.g. suffix with `uuid.NewString()`) so reruns against a persistent dev DB don't collide, or wrap in a tx / clean up.

This encodes the exact query surface Phase 2's auth service will call.

### Step 7 — Docs & CI touch-up

- **`CLAUDE.md`**: update the "Status" line to note Phase 1 (data layer) is complete — schema migrated via goose, seed available, sqlc queries for users/sessions in place. Add the new `make migrate` / `make seed` / `make sqlc` lines to the Commands block if not already accurate (they are listed there as future commands — confirm they now work).
- **`README.md`**: extend the quickstart to:
  ```
  cp .env.example .env
  make up
  make migrate
  make seed
  make api
  ```
  Add a one-line note that sqlc (`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`) is only needed to regenerate queries, not to build.
- **`.github/workflows/ci.yml`** (optional but recommended): add `postgres:16` + `redis:7` **service containers** to the backend job and set `DATABASE_URL`/`REDIS_URL` env so the gated integration test runs in CI (mirror the source `ci.yml` service setup). If you add this, run `go run ./cmd/migrate up` before `go test`. If CI wiring proves fiddly, leave a `// TODO(phase5)` and keep the unit run green — full CI parity is a Phase 5 deliverable.

---

## Acceptance criteria (verify before declaring Phase 1 done)

1. `cd apps/backend && go build ./... && go vet ./...` pass (including `cmd/migrate`, `cmd/seed`, generated `db` package, and the `Store`).
2. `go test ./...` passes with **no** `DATABASE_URL` set (integration test skips; nothing else regresses).
3. With `make up` running db+redis and `.env` present:
   - `make migrate` applies all 5 migrations; `make migrate-status` shows 5 applied and 0 pending.
   - Connecting to the DB shows the **10 tables** (`users, sessions, organizations, memberships, roles, permissions, member_roles, audit_logs, plans, org_subscriptions`) plus goose's `goose_db_version` bookkeeping table, with the correct unique/FK constraints (spot-check `users_email_unique`, `sessions_refresh_token_unique`, `sessions_user_id_users_id_fk ON DELETE cascade`).
   - `make seed` inserts free/pro/enterprise; running it a **second time** succeeds with no error and no duplicates (`SELECT count(*) FROM plans` == 3).
   - `make migrate-down` rolls back the last migration cleanly (drops `org_subscriptions`, `plans`); re-running `make migrate` restores them.
4. With `DATABASE_URL` set, `go test ./internal/infra/database/...` runs the integration test and passes (user + session + plan round-trip).
5. The ported migration Up bodies are DDL-identical to the source `0000`–`0004` files (constraint names and `ON DELETE cascade` preserved).
6. `sqlc generate` runs clean (no errors/warnings) and the committed generated code matches what it produces (no drift).
7. `make api` still boots and `curl -s localhost:3000/health` still returns `{"status":"ok","uptime":<number>}` — Phase 1 changed no HTTP behavior.

## Guardrails / notes for the executor

- **Schema fidelity is the contract.** Do not "improve" column types, add indexes, or rename constraints. Byte-identical DDL per `CLAUDE.md`.
- **Stay in the users/sessions/plans lane.** No queries for other tables, no auth/service/handler code, no Redis helpers. If a step seems to need them, it's a later phase — stop and note it.
- Prefer the goose **library + embedded FS** approach so no external CLI is required to migrate; the only optional external tool is `sqlc` (codegen only — generated code is committed).
- Keep `cmd/api` boot flow untouched; `NewStore` must merely exist and compile. Phase 2 wires it into the server.
- Commit sqlc-generated code. Run `go mod tidy` after adding goose / pgx stdlib and after codegen.
- Build after each step group (`go build ./...`); keep commits small and compilable.
- When all acceptance criteria pass, STOP and report what was created + how to run migrate/seed. Do not begin Phase 2.
