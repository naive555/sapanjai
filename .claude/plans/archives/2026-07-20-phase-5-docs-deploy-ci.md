# Phase 5 — Docs, Deploy, CI Parity — Implementation Plan

> **Status: ✅ All 7 steps complete (2026-07-21).** Two Definition-of-done
> items are 🟡 rather than ✅ — full `kubectl --dry-run=client` schema
> validation (no cluster configured in this environment) and an actual
> GitHub Actions run (needs a push/PR) — both are external-environment
> gaps, not code gaps; see Steps 4 and 5's "Verification" notes for what
> was checked instead and the exact follow-up command to run when available.
>
> Target executor: **Sonnet**. This plan is prescriptive: file paths, exact
> commands, and copy-paste-ready snippets. Read `docs/04-migration-plan.md`
> §"Phase 5" and `docs/03-target-architecture.md` lines 17, 53, 62–63 first.
> No business-logic changes in this phase — routes and behavior are frozen by
> Phase 4. This phase only adds API docs, a production image, k8s manifests,
> and CI/release parity.

## Scope

Phase 5 = **Docs, deploy, CI parity** (migration-plan §Phase 5):

1. **Swagger** at `/swagger` via **swaggo** (the decided stack — `CLAUDE.md`).
2. **Production Dockerfile**: distroless runner + working `HEALTHCHECK`.
3. **k8s manifests** ported from `../controlplane-api/k8s/` (api image → Go, env parity).
4. **GitHub Actions**: add `lint` (golangci-lint) + `docker build`, mirror source `release.yml` (ghcr push).
5. **Docs**: README + `docs/03` open-question / status updates; record deviations.

### Non-goals (defer)

- Frontend k8s `web` Deployment and frontend Docker/CI depth → **Phase 6**
  (frontend is only scaffolded; do not wire it into k8s here). Leave the
  commented `web:` block in `compose.yaml` as-is.
- Any change to handlers/services/DTO behavior. Swagger annotations are
  comments only — they must not alter runtime behavior.

## Ground truth captured from the codebase

- Go 1.26.2, Echo v4, module `github.com/controlplane/backend`.
- Route inventory (16 routes), from `internal/module/*/handler.go`:

  | Method | Path | Guard | Handler |
  | --- | --- | --- | --- |
  | GET | `/health` | public | `health.check` |
  | POST | `/auth/register` | public | `auth.register` |
  | POST | `/auth/login` | public | `auth.login` |
  | POST | `/auth/refresh` | public | `auth.refresh` |
  | POST | `/auth/logout` | public (reads bearer) | `auth.logout` |
  | POST | `/organizations` | RequireAuth | `organization.create` |
  | GET | `/organizations` | RequireAuth | `organization.list` |
  | POST | `/organizations/invite` | RequireOrg | `organization.invite` |
  | DELETE | `/organizations/members/:userId` | RequireOrg | `organization.removeMember` |
  | GET | `/rbac/roles` | RequireOrg | `rbac.listRoles` |
  | POST | `/rbac/roles` | RequireOrg | `rbac.createRole` |
  | PUT | `/rbac/roles/:roleId/permissions` | RequireOrg | `rbac.updatePermissions` |
  | POST | `/rbac/assign` | RequireOrg | `rbac.assignRole` |
  | GET | `/subscription` | RequireOrg | `subscription.get` |
  | POST | `/subscription/assign` | RequireOrg | `subscription.assign` |
  | GET | `/audit-logs` | RequireOrg | `auditlog.query` |

- Error body is `{ "message": string }`. Header `x-organization-id` gates
  `RequireOrg` routes; `Authorization: Bearer` gates auth'd routes.
- Existing Dockerfile: `apps/backend/Dockerfile` (alpine + curl HEALTHCHECK).
- Existing CI: `.github/workflows/ci.yml` (backend: vet/build/migrate/test;
  frontend: lint/build). No lint job, no docker build, no release workflow.
- Source k8s tree to port: `../controlplane-api/k8s/`.
- Env parity gotcha: source configmap uses `NODE_ENV`; the Go app uses
  **`APP_ENV`** (+ `APP_NAME`, `LOG_LEVEL`, `PORT`, `JWT_*`).

## Decisions locked for this phase

1. **Swagger tool = swaggo** (`swaggo/swag` + `swaggo/echo-swagger`). Generated
   `docs/` package is **committed** so CI needs no `swag` binary.
2. **Runner image = distroless** (`gcr.io/distroless/static-debian12:nonroot`)
   with a tiny **`cmd/healthcheck`** Go binary for `HEALTHCHECK`.
3. **Release workflow** ports source `release.yml` (ghcr push on CI success).
4. UI at `/swagger` (redirect) + `/swagger/*` (echo-swagger handler).

---

## Step 1 — Swagger annotations (swaggo) — ✅ DONE (2026-07-21)

### What shipped

- Added deps: `github.com/swaggo/swag`, `github.com/swaggo/echo-swagger`.
  Installed the `swag` CLI via `go install github.com/swaggo/swag/cmd/swag@latest`.
- `cmd/api/main.go`: added the general-info doc-comment block (`@title`,
  `@version`, `@BasePath`, `@securityDefinitions.apikey BearerAuth`, etc.)
  above `func main()`.
- Annotated all 16 handler methods across `auth`, `organization`, `rbac`,
  `subscription`, `auditlog`, `health` with `@Summary`/`@Tags`/`@Param`/
  `@Success`/`@Failure`/`@Router`, referencing the existing DTO structs by
  name and the contract's exact status codes/messages from `docs/02`.
- `internal/server/server.go`: mounted `GET /swagger` (301 redirect to
  `/swagger/index.html`) and `GET /swagger/*` (`echoSwagger.WrapHandler`),
  importing the generated `docs` package with `_`.
- Makefile: added `make swagger` target running `swag init`.

### Deviation from the original draft — discovered during implementation

The original draft planned an exported `server.ErrorResponse` (alias or
rename of the existing unexported `errorBody`) referenced from every
handler's `@Failure` annotations as `server.ErrorResponse`. **This does not
work with swaggo.** Two problems surfaced running `swag init
--parseDependency --parseInternal`:

1. With `--parseDependency`, swag namespaces cross-package type refs by
   their *import path* when resolving `{object}` annotations, so a bare
   `server.ErrorResponse` reference from a file in another package silently
   fails with `cannot find type definition: server.ErrorResponse` — even
   though the type exists and is exported. Adding `--useStructName` changes
   how swag *emits* names but does not fix resolution.
2. The real constraint: swag only resolves a `pkgname.Type` comment
   reference in a given file if that package is one **the file already
   imports** as `pkgname`. `internal/server` is never imported by any
   handler file (only by `cmd/api/main.go`), so no handler file could ever
   reference `server.ErrorResponse` regardless of flags.

**Fix applied**: moved the shared error-response type out of `internal/server`
into `internal/shared/httpx` (new file `httpx/response.go`), which every
handler file already imports for `httpx.BindAndValidate`. `server.go` now
uses `httpx.ErrorResponse` instead of a locally-defined `errorBody`/
`ErrorResponse` type (behaviorally identical — same `{"message": string}`
JSON shape). All `@Failure` annotations reference `httpx.ErrorResponse`.
`swag init` runs with `--parseDependency --parseInternal --useStructName`
(the last flag keeps generated definition names short, e.g. `ErrorResponse`
instead of a mangled full-import-path name).

**Takeaway for later steps/phases**: any future DTO shared across module
packages for Swagger purposes must live in a package that's *already
imported* by every referencing file (e.g. `httpx`, `apperror`), not in
`internal/server`.

### Verified

- `go build ./...`, `go vet ./...`, `go test ./...` — all clean, no behavior
  changes (confirmed via `git diff --stat`: only new annotations/comments,
  the `httpx.ErrorResponse` move, and the swagger mount in `server.go`).
- Generated `apps/backend/docs/{docs.go,swagger.json,swagger.yaml}` —
  inspected `swagger.json`: all 16 operations present across 14 paths (two
  paths carry two methods each: `/organizations` GET+POST, `/subscription`
  GET+POST), `securityDefinitions.BearerAuth` present with correct
  description.
- Did **not** get a live `/swagger` UI smoke test — Docker Desktop wasn't
  running locally (`docker compose up -d db redis` failed: "cannot connect
  to the Docker daemon"). Static validation of the generated spec was used
  instead. **Follow-up**: once infra is available, run `make up && make
  migrate && make seed && (cd apps/backend && go run ./cmd/api)` and hit
  `GET /swagger` (expect 301 → `/swagger/index.html`) and
  `GET /swagger/doc.json`.

### Files touched

- `apps/backend/cmd/api/main.go` (general API info)
- `apps/backend/internal/server/server.go` (swagger mount, `httpx.ErrorResponse` swap)
- `apps/backend/internal/shared/httpx/response.go` (new — `ErrorResponse` type)
- `apps/backend/internal/module/{auth,organization,rbac,subscription,auditlog,health}/handler.go` (annotations)
- `apps/backend/docs/{docs.go,swagger.json,swagger.yaml}` (generated, committed)
- `apps/backend/go.mod`, `go.sum` (swaggo deps)
- `Makefile` (`swagger` target)

---

## Step 2 — Production Dockerfile (distroless + healthcheck binary) — ✅ DONE (2026-07-21)

Shipped exactly as drafted below, no deviations.

### 2a. New `apps/backend/cmd/healthcheck/main.go`

Tiny binary; exits 0 if `GET http://127.0.0.1:$PORT/health` is 2xx, else 1.

```go
// Command healthcheck is a zero-dependency liveness probe for the distroless
// runtime image, where no shell or curl is available for Docker HEALTHCHECK.
package main

import (
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil || resp.StatusCode >= 300 {
		os.Exit(1)
	}
	os.Exit(0)
}
```

### 2b. Rewrite `apps/backend/Dockerfile`

```dockerfile
# syntax=docker/dockerfile:1

########################
# Builder
########################
FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

########################
# Runner (distroless, nonroot)
########################
FROM gcr.io/distroless/static-debian12:nonroot AS runner
WORKDIR /app

COPY --from=builder /out/api ./api
COPY --from=builder /out/healthcheck ./healthcheck

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/healthcheck"]

ENTRYPOINT ["/app/api"]
```

Notes: distroless `:nonroot` runs as uid 65532 by default (no `USER`/`adduser`
needed). `CGO_ENABLED=0` static build is required for `static-debian12`.
`migrations/` are embedded via `embed.go`, so no need to copy SQL files.

### 2c. Verify build

```bash
docker build -t controlplane-api:dev ./apps/backend
```

**✅ Verified live (2026-07-21), once Docker Desktop was started:**

- `docker build -t controlplane-api:dev ./apps/backend` — succeeds; final
  image `57.2MB`, both `api` and `healthcheck` binaries built in the same
  builder stage.
- `docker compose up -d --build api` (against real `db`+`redis` compose
  services, migrations already applied) — container reaches
  `Up ... (healthy)` within the 10s `start-period`.
- `docker inspect controlplane-api --format='{{json .State.Health}}'` shows
  `"Status": "healthy"`, two consecutive health-check log entries with
  `"ExitCode": 0`.
- Container logs confirm the healthcheck binary's requests: `GET /health
  status=200`.
- `docker inspect --format='User: {{.Config.User}}'` → `65532` (distroless
  `nonroot`, confirming no root escalation).
- Live endpoint checks against the running container:
  - `GET /health` → `{"status":"ok","uptime":...}`
  - `GET /swagger` → `301 Moved Permanently` → `Location: /swagger/index.html`
  - `GET /swagger/index.html` → `200`
  - `GET /swagger/doc.json` → valid OpenAPI 2.0 doc, `info.title: "Controlplane API"`,
    matches the statically-inspected spec from Step 1.
- `docker exec controlplane-api /app/healthcheck` → exit `0` (confirms the
  binary is independently runnable, not just wired into `HEALTHCHECK`).
- Cleaned up with `docker compose down`; no leftover containers.

This also serves as the live `/swagger` UI smoke test that Step 1 had
deferred (Docker wasn't available at the time).

---

## Step 3 — golangci-lint — ✅ DONE (2026-07-21, not yet committed)

### Deviation from the original draft — golangci-lint v2 config format

`go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`
resolves to **v2.12.2** now (the tool moved to a `/v2` module path and a
new config schema — `version: "2"` at the top, `gofmt`/`goimports` live
under a separate `formatters:` block instead of `linters.enable`, and
`issues.exclude-rules` moved to `linters.exclusions.rules`). The v1-style
config in the original draft doesn't validate under v2. Actual config
shipped:

```yaml
version: "2"

run:
  timeout: 5m

linters:
  enable:
    - errcheck
    - govet
    - staticcheck
    - ineffassign
    - unused
    - misspell
  exclusions:
    rules:
      - path: _test\.go
        linters:
          - errcheck

formatters:
  enable:
    - gofmt
    - goimports
```

Validated with `golangci-lint config verify` (no output = valid).

### 3b. Update Makefile `lint` — done, unchanged from draft

```make
## Lint the backend (golangci-lint if installed, else go vet)
lint:
	cd apps/backend && (command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || go vet ./...)
```

### 3c. Run once locally and fix findings — done, one real fix applied

`golangci-lint run` surfaced 4 issues on the first pass:

1. **`errcheck` in `cmd/migrate/main.go:48`** — `defer db.Close()` return
   value unchecked. **Fixed**: `defer func() { _ = db.Close() }()`,
   matching the existing house style for intentionally-ignored `Close()`
   errors in short-lived/cleanup paths (`internal/infra/redis/redis.go:25`
   uses the same `_ = client.Close()` pattern).
2. **3× `gofmt` findings, different files each run** — e.g. run 1 flagged
   `database.go`/`auth.go`(redis)/`bind.go`; run 2 flagged
   `database.go`/`organization/dto.go`/`organization/service.go`; run 3
   flagged `rbac/dto.go`/`rbac/service.go`/`rbac/service_test.go`.
   **Root cause, confirmed and not a real formatting problem**: this repo
   checkout has `core.autocrlf=true` (Windows), so most untouched `.go`
   files have CRLF line endings on disk while git stores LF. Verified by
   normalizing each flagged file's line endings (`sed 's/\r$//'`) and
   re-running `gofmt -d` — **zero diff** every time, meaning the actual
   (committed) content is already gofmt-clean; only the CRLF artifact
   differs. The specific 3-file subset golangci-lint's `gofmt` formatter
   picks changes nondeterministically run-to-run (raw `gofmt -l .` flags
   ~34 CRLF files consistently; golangci-lint's own check appears to sample/
   cap differently — not fully understood, but irrelevant given the root
   cause). **Not fixed** — touching line endings on ~34 unrelated files is
   out of scope for this step and would produce a huge unrelated diff.
   **Why this is safe to leave**: `.github/workflows/ci.yml`'s backend job
   runs on `ubuntu-latest`, which checks out LF-native (no `autocrlf`
   rewriting), so this flakiness is Windows-local-only and won't reproduce
   in CI.

Re-ran after the `errcheck` fix: `go build ./...`, `go vet ./...`,
`go test ./...` all clean — no behavior change.

**Not committed yet** — per instruction, this step's changes
(`.golangci.yml`, `cmd/migrate/main.go`, `Makefile`) are sitting in the
working tree.

---

## Step 4 — k8s manifests — ✅ DONE (2026-07-21, not yet committed)

Ported `../controlplane-api/k8s/` to `./k8s/` at repo root, verbatim except
where noted. Final layout matches the draft exactly:

```
k8s/
├── README.md                  # new — apply instructions + Phase 6 note
├── namespace.yaml              # unchanged
├── configmap.yaml              # env parity (see 4a)
├── secret.example.yaml         # unchanged content, fixed a source typo (see 4b)
├── api/
│   ├── deployment.yaml         # unchanged
│   ├── service.yaml            # unchanged
│   └── ingress.yaml            # unchanged
├── postgres/
│   ├── statefulset.yaml        # unchanged
│   ├── service.yaml            # unchanged
│   └── secret.example.yaml     # unchanged
└── redis/
    ├── deployment.yaml         # unchanged
    └── service.yaml            # unchanged
```

### 4a. `k8s/configmap.yaml` — env parity, done as drafted

`NODE_ENV: production` → `APP_ENV: production` + added `APP_NAME:
controlplane-api`. Verified all 6 keys against `internal/config/config.go`'s
actual `getEnv`/`os.Getenv` calls (`APP_NAME`, `APP_ENV`, `PORT`,
`LOG_LEVEL`, `JWT_ACCESS_EXPIRES_IN`, `JWT_REFRESH_EXPIRES_IN`) — exact match.

### 4b. `k8s/secret.example.yaml` — one incidental fix

Kept `JWT_ACCESS_SECRET`/`JWT_REFRESH_SECRET`/`DATABASE_URL`/`REDIS_URL`.
Fixed a source typo while porting: the source's example used
`DATABASE_URL: database://user:password@...` (invalid scheme, presumably a
copy-paste slip) — changed to `postgres://...` to match this project's
actual connection string format. Confirmed the source's real (gitignored,
untracked) `k8s/secret.yaml` was **not** copied over — only the committed
`.example.yaml` templates were ported, consistent with `k8s/secret.yaml`
and `k8s/**/secret.yaml` being gitignored in the source. Added the same two
gitignore lines to this repo's root `.gitignore`.

### 4c. `k8s/api/deployment.yaml` — unchanged from source

Liveness/readiness probes against `/health` kept as-is;
`image: controlplane-api:latest` / `imagePullPolicy: Never` kept for local
kind/minikube use (swap for a ghcr tag + `IfNotPresent` when deploying to a
real cluster off Step 5's release workflow — not done here, out of scope).

### 4d. Copied unchanged

`namespace.yaml`, `api/service.yaml`, `api/ingress.yaml`,
`postgres/{statefulset,service,secret.example}.yaml`,
`redis/{deployment,service}.yaml` — byte-identical to source (image tags
`postgres:16-alpine`/`redis:7-alpine` already match this project's
`compose.yaml`).

### 4e. `web` Deployment — deferred to Phase 6, done as drafted

Added `k8s/README.md` with the manifest layout, apply instructions, and an
explicit "Not here yet" section noting the frontend Deployment/Service land
in Phase 6.

### Verification — deviation from the plan's `kubectl --dry-run=client` step

No Kubernetes cluster is configured in this environment (Docker Desktop's
Kubernetes is not enabled — `kubectl config get-contexts` returns empty),
and `kubectl apply --dry-run=client` (even with `--validate=false`) still
calls the API server for resource-type discovery, so it fails with a
connection error regardless of `--dry-run`/`--validate` flags — there is no
way to do a pure offline `kubectl apply` dry-run without a live (or
kind/minikube) cluster. **Fallback used**: parsed all 11 YAML files with
`yaml.safe_load_all` (Python) — every file parses cleanly and each
document's `kind` matches what's expected (`Namespace`, `ConfigMap`,
`Secret`×3, `Deployment`×2, `Service`×3, `StatefulSet`, `Ingress`). Given
these are near-verbatim ports of the source's already-deployed manifests
(only `configmap.yaml`'s data keys and the one `secret.example.yaml` typo
changed), schema risk is low. **Follow-up**: if/when a local cluster (kind,
minikube, or Docker Desktop Kubernetes) is available, run `kubectl apply
--dry-run=client -f k8s/ -R` for full schema validation before any real
deploy.

**Not committed yet** — working tree only, per instruction.

---

## Step 5 — CI + release parity — ✅ DONE (2026-07-21, not yet committed)

### 5a. `.github/workflows/ci.yml`

Added a `lint` job and a `docker` job. Deviation from the draft: looked up
current action versions rather than guessing — `golangci-lint-action` is
now on **v9** (checked via WebFetch against the action's GitHub page,
2026-07-21), and **v7+ is required for golangci-lint v2's config schema**
(v7.0.0 dropped v1 support). Pinned `version: v2.12.2` in the action to
match the exact version installed and verified locally in Step 3, avoiding
any local/CI drift.

```yaml
lint:
  name: Lint
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.26"
    - uses: golangci/golangci-lint-action@v9
      with:
        version: v2.12.2
        working-directory: apps/backend
```

`docker` job added after `backend`, depending on both:

```yaml
docker:
  name: Docker build
  runs-on: ubuntu-latest
  needs: [lint, backend]
  steps:
    - uses: actions/checkout@v4
    - name: Build image
      run: docker build -t controlplane-api:ci ./apps/backend
```

Final job graph: `lint`, `backend`, `docker` (needs `[lint, backend]`),
`frontend` (unchanged, independent).

### 5b. `.github/workflows/release.yml` — new

Ported source's ghcr push-on-CI-success workflow with
`context: ./apps/backend` and `permissions.packages: write` as drafted.
**Deviation**: also bumped the two Docker actions past what the source
pins — `docker/login-action@v3` → **`@v4`** and
`docker/build-push-action@v5` → **`@v7`** (checked current versions via
WebFetch; source's pins are stale relative to today). `docker/login-action`
and `docker/build-push-action` versions confirmed as of 2026-07-21.

### Verification

- `.github/workflows/ci.yml` and `release.yml` both parse as valid YAML
  (`yaml.safe_load_all`); `ci.yml`'s job graph confirmed
  (`docker.needs == [lint, backend]`).
- Ran the equivalent of every new CI step locally: `go vet ./...` (clean),
  `go build ./...` (clean), `go test ./...` (clean), `golangci-lint run`
  (same 3-CRLF-file gofmt flakiness noted in Step 3 — not a real issue, CI
  is LF-native), `docker build -t controlplane-api:ci ./apps/backend`
  (succeeds, cached from Step 2's identical Dockerfile).
- **Not run**: the actual GitHub Actions workflows themselves (`act` isn't
  installed in this environment, and running real CI requires pushing/
  opening a PR). **Follow-up**: once pushed, watch the first CI run —
  particularly the `lint` job (first time `golangci-lint-action@v9` +
  `v2.12.2` runs in this repo's CI) and the `release.yml` trigger (fires on
  `workflow_run` after CI succeeds on `main` — will only be exercised once
  this branch merges to `main`).

**Not committed yet** — working tree only, per prior instruction (no new
instruction to commit was given for this step).

---

## Step 6 — Docs updates — ✅ DONE (2026-07-21, not yet committed)

- **README.md**: status line updated to "Phase 5 ... complete", with
  `/swagger` linked inline. Added three new sections: "Swagger / API docs"
  (UI URL, raw spec URL, `make swagger` regen instructions), "Docker"
  (distroless build, `docker compose up -d --build api`), "Kubernetes"
  (links to `k8s/README.md`, apply instructions). Updated the stale
  `Layout` k8s/ line ("ported in a later phase" → "api/postgres/redis; web/
  lands in Phase 6") and the `Common commands` table (`make lint`
  description was still "go vet ./..." from before Step 3; added the
  missing `make swagger` row).
- **CLAUDE.md**: status paragraph updated to "Phase 5 ... complete" with a
  new sentence covering everything Phase 5 added (swagger, distroless
  image + healthcheck binary, golangci-lint v2, k8s manifests with env
  parity, CI `lint`/`docker` jobs, `release.yml`). Also fixed the adjacent
  `Commands` block's `make lint` line (was claiming "golangci-lint +
  eslint" — frontend eslint isn't wired into the root Makefile) and added
  the missing `make swagger` entry, since it was directly adjacent to the
  status edit and factually wrong.
- **Decided not to duplicate** the Step-1 `httpx.ErrorResponse` swaggo
  rationale into `docs/03-target-architecture.md`'s "Deviations resolved"
  section: that section is specifically about API-contract behavioral
  deviations from `../controlplane-api` (Phase 4's entries are all
  response-shape/status-code changes), not internal Go package layout
  choices. The swaggo/httpx move has no API-contract impact, so it stays
  documented once, here in Step 1.

---

## Step 7 — Verify (run in order) — ✅ DONE (2026-07-21)

Ran every command in the draft sequence, in order, against the final state
of all 6 prior steps:

```bash
cd apps/backend
go mod tidy                            # no changes — deps already tidy
swag init -g cmd/api/main.go -o docs --parseDependency --parseInternal --useStructName
git diff --exit-code docs/             # exit 0 — regeneration is byte-identical to committed docs
go vet ./...                           # clean
golangci-lint run                      # 3 gofmt findings, same CRLF flakiness as Steps 3/5 (confirmed
                                        # non-issue there; CI is LF-native) — no real findings
go build ./...                         # clean
go test ./...                          # all packages ok
cd ../.. && docker build -t controlplane-api:dev ./apps/backend   # succeeds (cached from Step 2)
```

Manual smoke — ran the full stack for real, end-to-end, with the API
process running directly via `go run` (not containerized, complementing
Step 2c's containerized smoke test):

```bash
docker compose up -d db redis                             # → both healthy
DATABASE_URL=... go run ./cmd/migrate up                  # → current version: 5, no-op
cd apps/backend && go run ./cmd/api &                      # → "controlplane-api listening" on :3000
curl -s localhost:3000/health                               # → {"status":"ok","uptime":15.1...}
curl -s -o /dev/null -w "%{http_code}" -X GET localhost:3000/swagger        # → 301
curl -s -o /dev/null -w "%{http_code}" localhost:3000/swagger/index.html    # → 200
curl -s localhost:3000/auth/register -d '{"email":"smoke-...@example.com","password":"password123"}'
  # → real {"accessToken": "<JWT>", "refreshToken": "<JWT>"} pair, full auth flow working
```

Torn down cleanly afterward (`kill` on the API process, `docker compose
down`); confirmed no leftover containers or processes.

## Definition of done

1. ✅ `/swagger` serves Swagger UI with all 16 routes, correct tags, request/
   response schemas, and BearerAuth security; `swagger.json` matches
   `docs/02-api-contract.md` (statuses + messages). Live-verified against a
   running container (see Step 2c).
2. ✅ Generated `apps/backend/docs/` committed-ready; regeneration is
   idempotent via `make swagger`.
3. ✅ `docker build ./apps/backend` produces a distroless image (57.2MB)
   whose `HEALTHCHECK` binary passes against a running container —
   verified live via `docker compose up -d --build api` + `docker inspect`
   (see Step 2c).
4. 🟡 `k8s/` env vars match Go config (`APP_ENV`, not `NODE_ENV`) — confirmed
   against `internal/config/config.go`. All 11 manifests YAML-valid with
   correct `kind`s; full `kubectl --dry-run=client` schema validation still
   outstanding (no cluster configured here — see Step 4 follow-up).
5. 🟡 CI: `lint` + backend + docker build jobs added, `release.yml` present,
   both YAML-valid and every step's command verified locally — actual
   GitHub Actions execution still outstanding (needs a push/PR; see Step 5
   follow-up).
6. ✅ `go test ./...` unchanged — no behavior regressions.
7. ✅ README + CLAUDE.md status updated.

## Suggested commit sequence

1. `feat(docs): swagger annotations + /swagger endpoint (swaggo)` ← Step 1, ready to commit
2. `feat(deploy): distroless image + healthcheck binary`
3. `chore(lint): add golangci-lint config + Makefile/CI lint`
4. `feat(deploy): port k8s manifests (env parity, api image)`
5. `ci: add lint job, docker build, and release workflow`
6. `docs: update README/CLAUDE for Phase 5`
