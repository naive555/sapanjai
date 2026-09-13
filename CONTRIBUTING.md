# Contributing to Sapanjai

## Before your first pull request: the CLA

Sapanjai is dual licensed — AGPL-3.0 to everyone, plus a commercial license for
organizations that cannot accept the AGPL ([`LICENSING.md`](LICENSING.md)). The second one
is only offerable if the project may license the whole codebase that way, so contributions
are accepted under a Contributor License Agreement: [`CLA.md`](CLA.md).

You keep your copyright. Signing is one comment on your first pull request, and a bot will
prompt you. It covers everything you contribute afterwards.

## Getting it running

```
cp .env.example .env    # then fill in the required values listed there
make up                 # postgres + redis
make migrate            # schema
make seed               # default plans
make api                # terminal 1
make web                # terminal 2 — dev server on :4000
make worker             # terminal 3, optional — background jobs
```

There is no process manager tying these together; they are separate terminals by design.
`docker compose up -d --build` runs the whole stack in containers instead.

## Before you open the pull request

```
make test                      # backend
make lint                      # backend

cd apps/frontend
pnpm lint
pnpm exec tsc --noEmit
pnpm test
```

CI runs the same four jobs — lint, backend, frontend, docker build — so a green local run
is a good predictor.

## Things that will come up in review

- **[`docs/02-api-contract.md`](docs/02-api-contract.md) is the source of truth** for
  routes, headers, status codes, and error messages. If you change or add a route, the
  contract changes in the same pull request.
- **Module shape is handler → service → sqlc queries**, one per domain. Services return
  `apperror` codes and know nothing about HTTP; a single Echo error handler maps codes to
  responses.
- **Migrations are additive-forward only.** Never edit an applied migration in
  `apps/backend/migrations/` — add a new goose migration and run `make sqlc`.
- **Secrets never travel.** Decrypted connector config must not reach a response DTO, a log
  line, or audit metadata. New sensitive log keys go in
  `internal/shared/logger/redact.go`, not at the call site — and log individual fields
  rather than whole request structs, since redaction matches leaf keys only.
- **Tests are expected**, not optional: unit tests per service with mocked infra, and
  integration tests against real postgres and redis for anything touching a route.
- `docs/` holds the design reasoning behind the core. Read the relevant one before changing
  something it covers — several decisions that look arbitrary are load-bearing and
  documented there.
- `spikes/*` are throwaway feasibility modules, deliberately outside the build. Don't wire
  them into the main pipeline.

## Reporting a security issue

Please don't open a public issue for a vulnerability. [`SECURITY.md`](SECURITY.md) has the
private reporting routes, what's in and out of scope, and what to expect back.
