# Kubernetes manifests

Deployment manifests for the junctera backend — the API, the background
worker, a one-off schema/seed Job, and their Postgres and Redis dependencies.
Plain YAML, no Helm or Kustomize.

These target a **local cluster** (minikube / kind / Docker Desktop) as written:
the api, worker, and migrate workloads all reference `junctera-api:latest` with
`imagePullPolicy: Never`, meaning the image must already exist in the cluster's
own image store. Load it before applying — e.g. `minikube image load
junctera-api:latest` or `kind load docker-image junctera-api:latest`.
For a real cluster, swap in a registry-qualified image reference and drop the
`Never` pull policy.

## Layout

```
k8s/
├── namespace.yaml            # junctera namespace — apply this first
├── configmap.yaml            # non-secret env shared by api, worker, migrate
├── secret.example.yaml       # template — copy to secret.yaml, fill in, never commit
├── api/
│   ├── deployment.yaml       # junctera-api, 2 replicas, /health probes on :3000
│   ├── service.yaml          # junctera-api-service :3000 (ClusterIP)
│   └── ingress.yaml          # nginx, host: junctera.local
├── worker/
│   └── deployment.yaml       # junctera-worker, 1 replica, /health probes on :3001
├── migrate/
│   └── job.yaml              # one-off: goose migrations, then plan seeding
├── postgres/
│   ├── statefulset.yaml      # postgres:16-alpine + 1Gi PVC
│   ├── service.yaml          # postgres-service :5432
│   └── secret.example.yaml   # template — copy to secret.yaml, fill in
└── redis/
    ├── deployment.yaml       # redis:7-alpine
    └── service.yaml          # redis-service :6379
```

## Configuration

Env reaches every workload through `envFrom`, combining two objects:

| Object | Kind | Holds |
| --- | --- | --- |
| `junctera-config` | ConfigMap | `APP_NAME`, `APP_ENV=production`, `LOG_LEVEL`, `PORT`, `JWT_ACCESS_EXPIRES_IN`, `JWT_REFRESH_EXPIRES_IN` |
| `junctera-secret` | Secret | `JWT_ACCESS_SECRET`, `JWT_REFRESH_SECRET` (min 32 chars each), `REDIS_URL` |
| `postgres-credentials` | Secret | `username`, `password`, `dbname` |

### Where DATABASE_URL comes from

It isn't stored anywhere. `postgres-credentials` is the single source of the
database credentials — it configures the Postgres StatefulSet, and each
workload assembles its own `DATABASE_URL` from those same three values using
Kubernetes' `$(VAR)` expansion:

```yaml
- name: DATABASE_URL
  value: postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres-service:5432/$(POSTGRES_DB)?sslmode=disable
```

This mirrors what `compose.yaml` does with `DATABASE_USER`/`PASSWORD`/`NAME`:
one place to change credentials, and no second copy that can silently drift out
of sync with the database that's actually running.

Two rules govern that expansion, and they're why each container repeats a
four-entry `env` block: `$(VAR)` only resolves variables declared in the *same*
container's `env` list — never ones arriving through `envFrom` — and only those
declared *before* the reference. So the three `secretKeyRef` lookups have to be
inline and ahead of `DATABASE_URL`. The block is duplicated across
`api/`, `worker/`, and both containers of `migrate/`; the credentials are not.

One constraint this introduces: the password is interpolated into a URL, so
avoid characters that would need percent-encoding there (`@ : / ? #`).

`REDIS_URL` stays a plain secret value, pointing at `redis-service:6379`.

Worker-tuning vars (`WORKER_JOB_TIMEOUT`, `SESSION_CLEANUP_*`) aren't set
anywhere here; the worker falls back to its built-in defaults. Add them to the
ConfigMap to override.

## Worker

`worker/deployment.yaml` runs the **same image** as `api/`, with `command`
overridden to `/app/worker`. It has no Service and no Ingress — nothing routes
to it. Its liveness/readiness probes hit `/health` on port 3001
(`WORKER_PORT`), which is the worker's internal ops endpoint reporting job
run/failure/skip stats; it is not part of the public API.

`replicas: 1` is the default. Scaling up is safe: job runs are coordinated by a
Redis lock (`worker:lock:<job>`), so each job still runs about once per interval
fleet-wide regardless of replica count.

## Apply

```bash
cp k8s/secret.example.yaml k8s/secret.yaml
cp k8s/postgres/secret.example.yaml k8s/postgres/secret.yaml
# edit both with real values (JWT secrets must be at least 32 characters)

kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/ -R
```

`k8s/secret.yaml` and `k8s/**/secret.yaml` are gitignored — never commit real
secrets.

Reaching the API through the Ingress needs `junctera.local` resolving to the
ingress controller; add it to your hosts file, or skip the Ingress entirely and
port-forward:

```bash
kubectl -n junctera port-forward svc/junctera-api-service 3000:3000
curl localhost:3000/health
```

## Migrations and seeding

`migrate/job.yaml` handles both, and `kubectl apply -f k8s/ -R` picks it up
along with everything else — no manual step on a first install. It runs the
same image as the Deployments with the entrypoint overridden: an init container
runs `/app/migrate up`, then the main container runs `/app/seed` to insert the
default plans. Migrations are embedded in the binary, so nothing is mounted.

Two behaviors worth knowing:

- **It tolerates racing Postgres.** The Job is applied at the same time as the
  StatefulSet, so the database usually isn't accepting connections yet. The
  first attempts fail, and `backoffLimit: 6` with `restartPolicy: OnFailure`
  retries until Postgres is up.
- **It deletes itself 5 minutes after succeeding** (`ttlSecondsAfterFinished`).
  That's what keeps `kubectl apply -f k8s/ -R` re-runnable — a completed Job's
  spec is immutable, so re-applying one that still exists is an error. Both
  steps are idempotent (goose skips applied migrations, the seeder upserts), so
  the re-run a later apply triggers is harmless.

Watch it or re-run it on demand:

```bash
kubectl -n junctera logs job/junctera-migrate --all-containers -f
kubectl -n junctera delete job junctera-migrate --ignore-not-found
kubectl apply -f k8s/migrate/job.yaml
```

Until it completes the API stays up but every request that touches the database
fails — the pods don't crash-loop, since `/health` doesn't query Postgres.

For migration commands the Job doesn't cover (`down`, `status`), port-forward
and use the Makefile targets against the cluster database:

```bash
kubectl -n junctera port-forward svc/postgres-service 5432:5432
# with DATABASE_URL pointed at localhost:5432:
make migrate-status
```

## Not here yet

- **Frontend.** There are no `web` Deployment/Service/Ingress manifests. The
  Next.js image and its compose service already exist (`apps/frontend/Dockerfile`,
  the `web` service in `compose.yaml`); porting them alongside `api/` is a
  follow-up. A `web` Deployment would need `BACKEND_URL` set to
  `http://junctera-api-service:3000`.
- **Postgres and Redis** are single-replica with no backup, no auth on Redis, and
  a 1Gi PVC — fine for a local cluster, not a production data tier. Use managed
  services or an operator for anything real.
