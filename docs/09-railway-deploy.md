# 09 — Railway deployment

How this monorepo is deployed on Railway, and the two settings that are easy
to get wrong. Everything here is configured in the Railway dashboard: Railway's
Config as Code (`railway.toml` / `railway.json`) was deprecated on 2026-08-26,
services that never used it cannot opt in from 2026-08-28, and existing files
stop working 2026-12-01. Its replacement is Infrastructure as Code
(`.railway/railway.ts`, applied with `railway config plan` / `railway config
apply`) — worth revisiting once the deployment is settled, but note that the
documented `service()` schema has no equivalent for the pre-deploy command that
runs migrations below.

## Topology

Five Railway services in one project:

| Service    | Source                | Notes                                    |
| ---------- | --------------------- | ---------------------------------------- |
| `Postgres` | Railway plugin        | provides `DATABASE_URL`                  |
| `Redis`    | Railway plugin        | provides `REDIS_URL`                     |
| `api`      | repo, `apps/backend`  | public domain; runs migrations pre-deploy |
| `worker`   | repo, `apps/backend`  | same image, no domain                    |
| `web`      | repo, `apps/frontend` | public domain; the only thing users hit  |

`api` and `worker` build the *same image* from the same directory and differ
only in start command.

## The two settings that break the build if wrong

**1. Root Directory is what moves the Docker build context.** Both Dockerfiles
`COPY` manifests from the context root — `go.mod`/`go.sum` for the backend,
`package.json`/`pnpm-lock.yaml`/`pnpm-workspace.yaml` for the frontend — so a
repo-root context fails immediately:

```
$ docker build -f apps/backend/Dockerfile .     # repo-root context
ERROR: failed to compute cache key: "/go.sum": not found
$ docker build ./apps/backend                   # correct
builds fine
```

Setting the *Dockerfile Path* alone does **not** fix this; it points at the
Dockerfile without moving the context. Set Root Directory, and leave Dockerfile
Path empty so Railway auto-detects `./Dockerfile` inside it.

**2. The backend image ends in `CMD`, not `ENTRYPOINT`** (`apps/backend/Dockerfile`).
Railway's Custom Start Command replaces the image's CMD but *appends* to an
exec-form ENTRYPOINT. With an ENTRYPOINT, a start command of `/app/worker`
would run `/app/api /app/worker` — the API ignores unknown args, so the worker
service would silently boot a second API with no error anywhere. Don't
reintroduce an ENTRYPOINT.

## Per-service settings

### `api`

| Setting             | Value                                              |
| ------------------- | -------------------------------------------------- |
| Root Directory      | `apps/backend`                                      |
| Dockerfile Path     | *(empty — auto-detected)*                           |
| Custom Start Command| `/app/api`  *(or empty; it is the image CMD)*       |
| Pre-Deploy Command  | `/app/migrate up`                                   |
| Healthcheck Path    | `/health`                                           |
| Public Domain       | yes — MCP clients connect here directly             |

The pre-deploy command runs between build and deploy in a separate container
from the same image, and a failure blocks the deployment rather than shipping a
half-migrated app. `migrate` ships in the image with the goose migrations
embedded, so no SQL files or goose CLI are needed.

### `worker`

| Setting             | Value             |
| ------------------- | ----------------- |
| Root Directory      | `apps/backend`    |
| Custom Start Command| `/app/worker`     |
| Pre-Deploy Command  | *(none)*          |
| Healthcheck Path    | *(none)*          |
| Public Domain       | no                |

No pre-deploy: `api` owns migrations, and two services racing `migrate up`
buys nothing. No healthcheck: Railway probes the port it assigns via `PORT`,
but the worker's ops server listens on `WORKER_PORT` (default 3001) and the
service has no domain.

Scaling the worker (or the API, if the scheduler is ever folded into it) is
safe: `internal/worker` takes a Redis lock (`worker:lock:<job>`) held for
interval − 10% and deliberately not released on success, so N replicas still
produce about one run per interval rather than N.

### `web`

| Setting             | Value                                        |
| ------------------- | -------------------------------------------- |
| Root Directory      | `apps/frontend`                               |
| Custom Start Command| *(empty — the image CMD `node server.js`)*    |
| Healthcheck Path    | `/`                                           |
| Public Domain       | yes                                           |

`/` returns 200 (the root page redirects client-side after resolving session
status), so it is a valid probe.

## Environment variables

Pin `PORT=3000` on `api` so the internal address the frontend dials stays
stable — otherwise Railway's assigned port moves and `BACKEND_URL` goes stale.

**`api` and `worker` (identical — `config.Load` validates the full set in both
processes, not just the API):**

| Variable               | Value                              |
| ---------------------- | ---------------------------------- |
| `DATABASE_URL`         | `${{Postgres.DATABASE_URL}}`       |
| `REDIS_URL`            | `${{Redis.REDIS_URL}}`             |
| `JWT_ACCESS_SECRET`    | ≥ 32 chars                         |
| `JWT_REFRESH_SECRET`   | ≥ 32 chars                         |
| `CONNECTOR_MASTER_KEY` | `openssl rand -base64 32`          |
| `PORT`                 | `3000` (api only)                  |

Optional, with defaults: `APP_ENV`, `LOG_LEVEL`, `JWT_ACCESS_EXPIRES_IN`,
`JWT_REFRESH_EXPIRES_IN`, `MCP_RATE_LIMIT_PER_MIN`,
`CONNECTOR_MASTER_KEY_PREVIOUS`, and the worker's `WORKER_JOB_TIMEOUT` /
`SESSION_CLEANUP_*`. See CLAUDE.md § Environment.

Rotating `CONNECTOR_MASTER_KEY` without moving the old value into
`CONNECTOR_MASTER_KEY_PREVIOUS` makes every stored connector config
unreadable — see the envelope-encryption ground rule in CLAUDE.md.

**`web`:**

| Variable      | Value                                  |
| ------------- | -------------------------------------- |
| `BACKEND_URL` | `http://<api>.railway.internal:3000`    |
| `GATEWAY_URL` | the `api` service's public HTTPS URL    |

These are different on purpose. `BACKEND_URL` is *dialled* server-side by the
proxy at `app/api/[...path]/route.ts`, so it uses private networking — Go binds
`:PORT` dual-stack, which Railway's IPv6-only private network needs.
`GATEWAY_URL` is *handed out*: it goes into the connector page's MCP wiring
snippet, so it must be a public address an external MCP client can reach.

Both are read at request time, so no build-time variables are needed and one
image works in any environment.

## First deploy

1. Add the Postgres and Redis plugins.
2. Create `api`, `worker`, `web` with the settings above.
3. Deploy `api` — the pre-deploy command applies migrations.
4. Seed the default plans once: `railway run --service api /app/seed`. Without
   this, `GET /plans` returns an empty list and the plan picker is blank.
5. Deploy `worker` and `web`.

## Verifying

```
curl https://<api>/health          # {"status":"ok","uptime":...}
curl -o /dev/null -w '%{http_code}' https://<web>/          # 200
curl -o /dev/null -w '%{http_code}' https://<web>/api/health # 200, proxy works
curl -o /dev/null -w '%{http_code}' https://<web>/api/plans  # 401, guard is live
```

Worker logs should show `worker started jobs=1` followed by
`job completed job=session-cleanup`. If they instead show
`sapanjai-api listening`, the start command is not taking effect — see gotcha 2.

## Not on Railway

`compose.yaml` (local), `k8s/` (manifests), and `.github/workflows/release.yml`
(pushes the backend image to ghcr on CI success against `main`) are unchanged
by any of this. Note `release.yml` publishes only the **backend** image — there
is no frontend image in ghcr, so `web` must build from source on Railway.
