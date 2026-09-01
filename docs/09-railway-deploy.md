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

| Serverless          | on, pre-launch (see below)                    |

`/` returns 200 (the root page redirects client-side after resolving session
status), so it is a valid probe.

**Serverless (app sleeping).** Settings → Deploy → Serverless → *Enable
Serverless*, per service. Railway stops the service after ~10 minutes with no
outbound traffic and wakes it on inbound traffic — from the internet or from
another service over the private network. For a dashboard with no users yet
this is most of the frontend's cost, so leave it on until launch. Two caveats:
the first request after a sleep "may return a 502 Bad Gateway", and Next.js
telemetry makes outbound calls that can reset the idle timer — which is why
`NEXT_TELEMETRY_DISABLED=1` is baked into `apps/frontend/Dockerfile` rather
than left to a service variable.

The deploy healthcheck does *not* keep it awake — Railway "does not monitor the
healthcheck endpoint after the deployment has gone live"; it only gates the
deploy.

**Never enable Serverless on `worker`.** It wakes on *inbound* traffic and a
job scheduler receives none, so it would sleep between ticks and silently stop
running jobs. Be wary on `api` too once MCP clients connect: a 502 on a cold
tool call is a bad failure mode.

## Environment variables

Pin `PORT=3000` on `api` so the internal address the frontend dials stays
stable — otherwise Railway's assigned port moves and `BACKEND_URL` goes stale.

**`api` and `worker` (identical — `config.Load` validates the full set in both
processes, not just the API):**

| Variable               | Value                              |
| ---------------------- | ---------------------------------- |
| `DATABASE_URL`         | `${{Postgres.DATABASE_URL}}`       |
| `REDIS_URL`            | `${{Redis.REDIS_URL}}`             |
| `REDIS_KEY_PREFIX`     | `sapanjai:` (must match on both)   |
| `JWT_ACCESS_SECRET`    | ≥ 32 chars                         |
| `JWT_REFRESH_SECRET`   | ≥ 32 chars                         |
| `CONNECTOR_MASTER_KEY` | `openssl rand -base64 32`          |
| `PORT`                 | `3000` (api only)                  |
| `APP_PUBLIC_URL`       | `https://${{sapanjai-web.RAILWAY_PUBLIC_DOMAIN}}` |

`APP_PUBLIC_URL` is the origin verification and password-reset links are built
against, so it must point at **`web`**, not `api` — a link pointing at the API
lands on a route that does not exist. Left at its `http://localhost:4000`
default, every email sent from a deployment ships a dead link.

`REDIS_KEY_PREFIX` matters here because `${{Redis.REDIS_URL}}` resolves to a
service **inside this Railway project** — if another application's services
live in the same project and reference the same `Redis` plugin, both apps share
one instance and one keyspace (DB 0, no prefix). Several of our key names come
from the platform-core template that sibling projects also derive from, so
`login:attempts:<email>` in particular becomes one shared counter: a user with
the same address in both apps can be locked out of ours by failed logins
against theirs, and a success on either side clears the other's count. The
prefix makes the keyspaces disjoint.

It does **not** make the *instances* disjoint. `maxmemory` and the eviction
policy are instance-wide, and the evictor does not care which prefix or logical
DB a key lives under — a noisy neighbour can still evict our `worker:lock:` and
`verify:email:` keys, both of which fail silently when they vanish (a dropped
lock lets two replicas run one job; a dropped token kills a user's verification
link). Check with:

```
redis-cli -u "$REDIS_URL" CONFIG GET maxmemory-policy maxmemory
redis-cli -u "$REDIS_URL" INFO stats | grep evicted_keys
```

A climbing `evicted_keys`, or an `allkeys-*` policy, means the prefix is not
enough and the app wants its own Redis service.

Set `REDIS_KEY_PREFIX` to the same value on `api` and `worker` — they meet on
`<prefix>worker:lock:<job>` and `<prefix>blacklist:<token>`, so a mismatch
gives you two processes that cannot see each other's locks or logouts.

**`worker` only** — the transactional-mail sender. Deliberately *not* set on
`api`: the API renders and enqueues and never talks to Resend, so the key has no
reason to sit on the internet-facing service (see
[`10-transactional-email.md`](10-transactional-email.md)).

| Variable         | Value                                        |
| ---------------- | -------------------------------------------- |
| `RESEND_API_KEY` | from the Resend dashboard                    |
| `EMAIL_FROM`     | `Name <addr@your-verified-domain>`           |

`EMAIL_FROM` must be on a domain verified in Resend or every send 403s, burns
all `EMAIL_MAX_ATTEMPTS`, and lands as `status='failed'`. With `RESEND_API_KEY`
unset, a deployed worker does not silently succeed — it logs recipient+subject
only and records `no RESEND_API_KEY configured` in `email_outbox.last_error`.

Optional, with defaults: `APP_ENV`, `LOG_LEVEL`, `JWT_ACCESS_EXPIRES_IN`,
`JWT_REFRESH_EXPIRES_IN`, `MCP_RATE_LIMIT_PER_MIN`,
`CONNECTOR_MASTER_KEY_PREVIOUS`, `ADMIN_IP_ALLOWLIST`, `ADMIN_REQUIRE_2FA`
(`api` only — the worker never serves `/admin`, so neither var does anything
there), and the worker's `WORKER_JOB_TIMEOUT` / `SESSION_CLEANUP_*` /
`EMAIL_DISPATCH_INTERVAL` / `EMAIL_DISPATCH_BATCH_SIZE` /
`EMAIL_MAX_ATTEMPTS` / `EMAIL_OUTBOX_RETENTION`. See CLAUDE.md § Environment.

### Admin console: `ADMIN_IP_ALLOWLIST` has no in-app recovery path

`ADMIN_IP_ALLOWLIST` (comma-separated CIDRs, parsed once at `api` boot — a
malformed entry fails the deploy rather than silently letting every request
through) restricts `/admin` before `RequireAuth` even runs. **A wrong value
locks every platform staff account out of the console, and there is no
break-glass account, no bypass header, and no way to fix it from inside the
console it just locked you out of.** Recovery is: fix the variable on the
`api` service, redeploy. Test any change against a non-empty value in a
non-production environment first. Left unset (the default), the check is
disabled — required for local dev and for any deployment that hasn't
explicitly opted in.

What it actually filters depends on how the request reached `api`, which is
why this lives in this document rather than only in `.env.example`. Every
`/admin` request from the console's own UI arrives through `web`'s runtime
proxy (`app/api/[...path]/route.ts`) over Railway's **private** network — the
same path `BACKEND_URL` uses for everything else on this page. `api`'s
`e.IPExtractor` (`internal/server/server.go`) is configured to trust that hop
(`TrustPrivateNet`) and read the real caller's address from
`X-Forwarded-For` beyond it — but `route.ts` unconditionally **strips** any
inbound `X-Forwarded-For`/`X-Real-IP` before it forwards a request (see that
file's own comment), rather than trying to tell a genuine upstream entry
apart from one the browser itself set. The consequence: for console traffic
proxied through `web`, `api` sees `web`'s own private-network address, not
the staff member's — there is no trustworthy signal of "which office/VPN did
this admin call from" once headers from the browser are refused on
principle. `ADMIN_IP_ALLOWLIST` is therefore a real defense against
internet-wide scanning of `api`'s public `/admin/*` routes directly (an
off-network request gets a 404 before authenticating at all, which is the
control's actual job), but it is **not** a substitute for restricting who can
reach the console's UI in the first place — that has to happen in front of
`web` (Railway access control on the `web` service, a WAF rule, or a VPN
requirement) if per-office/VPN granularity is required. See
`internal/server/server.go`'s `e.IPExtractor` comment for the full trust
chain this depends on.

`ADMIN_REQUIRE_2FA` (default `true`) gates every `/admin` route except
`POST /admin/2fa/{enroll,confirm,verify}` behind a confirmed TOTP step-up,
cached 12h per admin in Redis. It has no equivalent lockout risk — enroll and
confirm are always reachable to any platform-role account regardless of this
setting — but note it does nothing on the worker either.

Rotating `CONNECTOR_MASTER_KEY` without moving the old value into
`CONNECTOR_MASTER_KEY_PREVIOUS` makes every stored connector config
unreadable — see the envelope-encryption ground rule in CLAUDE.md.

### Bootstrapping the first platform admin

`grantadmin` ships in the same image as `api`/`worker`/`migrate`/`seed` and is
the only way to grant the first `superadmin` — `PATCH
/admin/users/:userId/platform-role` already requires being a superadmin, so
there is no API path to create the first one. It never creates a user or sets
a password (see the doc comment atop `apps/backend/cmd/grantadmin/main.go`):
the target account must already exist, having registered and logged in
through the app normally.

It ships in the image so that any host which can override a container command
can run it (`/app/grantadmin -email … -role …` — that is how `k8s/migrate/job.yaml`
runs `migrate` and `seed`). **Railway is not such a host for a one-off:** it
isn't a per-deploy step, so it doesn't belong on Pre-Deploy Command the way
`migrate up` does, and the distroless runner has no console to exec into (see
"Running migrate and seed by hand" below). On Railway, run it the same way as
the `seed` one-off — from your machine, against the database's public URL,
since `grantadmin` reads `DATABASE_URL` through the same `config.Load` as the
API:

```
cd apps/backend
DATABASE_URL='<DATABASE_PUBLIC_URL>' go run ./cmd/grantadmin -email you@example.com -role superadmin
```

**`web`:**

| Variable                  | Value                                                    |
| ------------------------- | -------------------------------------------------------- |
| `BACKEND_URL`             | `http://${{sapanjai-api.RAILWAY_PRIVATE_DOMAIN}}:3000`    |
| `GATEWAY_URL`             | `https://${{sapanjai-api.RAILWAY_PUBLIC_DOMAIN}}`         |

Use Railway's cross-service references (`${{ServiceName.VAR}}`) rather than
pasting literal hostnames, so neither breaks if a domain changes. The `:3000`
is why `PORT` is pinned on `api` — without it Railway assigns a port that moves
between deploys.

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
4. Seed the default plans once — see "Running migrate and seed by hand" below.
   Without this, `GET /plans` returns an empty list and the plan picker is blank.
5. Deploy `worker` and `web`.

## Running migrate and seed by hand

The runtime image is `gcr.io/distroless/static-debian12:nonroot`, which has no
shell. Railway's **Console tab cannot attach to it** ("this container doesn't
include a shell"), and it never will — that is the same property that forces
the dedicated `cmd/healthcheck` binary instead of a `curl` HEALTHCHECK. Don't
try to get a shell in there; use one of these instead.

**Migrations: the Pre-Deploy Command.** `/app/migrate up` needs no shell —
Railway execs commands directly in exec form rather than wrapping them in
`sh -c`, so a bare binary path is exactly right. This runs on every deploy and
is the only migration path you should need.

**One-offs (seeding, a manual `migrate` run, or `grantadmin`): from your
machine, against the database's public URL.** Copy `DATABASE_PUBLIC_URL` from
the Postgres service's Variables tab — the internal `*.railway.internal`
address is not reachable from outside Railway — then:

```
cd apps/backend
DATABASE_URL='<DATABASE_PUBLIC_URL>' go run ./cmd/seed
DATABASE_URL='<DATABASE_PUBLIC_URL>' go run ./cmd/migrate up    # if ever needed
DATABASE_URL='<DATABASE_PUBLIC_URL>' go run ./cmd/grantadmin -email you@example.com -role superadmin
```

(See "Bootstrapping the first platform admin" above for what `grantadmin`
does and doesn't do.)

All three commands call the same `config.Load` as the API, so they validate the
*whole* env contract, not just `DATABASE_URL`. That is fine: `loadDotEnv` uses
`godotenv.Load`, which does **not** overwrite variables already set, so the
explicit `DATABASE_URL` above wins while `JWT_*_SECRET` and
`CONNECTOR_MASTER_KEY` come from your local `.env`.

`railway run` does *not* help here — it injects Railway's variables into a
command running on **your machine**, so `railway run /app/seed` would look for
`/app/seed` locally and fail.

**If you genuinely need a console**, swap the runner base to
`gcr.io/distroless/static-debian12:debug-nonroot`, which bundles a busybox
shell at `/busybox/sh`. That puts a shell back into the production image, so
treat it as a temporary debugging build rather than the default.

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

## Troubleshooting

### `fetch failed` / `ECONNREFUSED` in the web logs

```
⨯ [TypeError: fetch failed] {
  [cause]: AggregateError:
    code: 'ECONNREFUSED',
    [errors]: [ [Error], [Error] ]
  }
}
```

`BACKEND_URL` is unset on the web service, and the proxy fell back to its
hardcoded `http://localhost:3000` (see `backendUrl()` in
`app/api/[...path]/route.ts`). Railway assigns `PORT=8080` by default, so the
app listens on 8080 and nothing answers on 3000 inside the container.

The **two** entries in `[errors]` are the giveaway: `localhost` resolves to both
`::1` and `127.0.0.1` and both refuse. A wrong `*.railway.internal` host fails
with a single error or a DNS failure instead — so two errors means localhost,
i.e. a missing variable rather than a wrong one.

Note this passes the deploy healthcheck: `/` returns 200 whether or not the
backend is reachable, so the deployment goes green and the fault only appears
once a page actually calls the API.

Locally the same misconfiguration returns **404**, not 500 — with `PORT` at its
default 3000 the fallback loops back into the Next.js server itself, which has
no `/health` route. Reproduce the Railway behaviour with
`docker run -e PORT=8080 ...`.
