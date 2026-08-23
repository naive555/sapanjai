.PHONY: up down dev api web worker build test lint tidy fmt migrate migrate-down migrate-status sqlc seed swagger

## Start Postgres + Redis in the background
up:
	docker compose up -d db redis

## Stop and remove all compose services
down:
	docker compose down

## Run backend and frontend together.
## No process manager is used in Phase 0 — open separate terminals instead:
##   make api
##   make web
##   make worker
dev:
	@echo "Run these in separate terminals:"
	@echo "  make api    # Go API on :3000"
	@echo "  make web    # Next.js dev server"
	@echo "  make worker # background jobs"

## Run the Go API (requires `make up` first and a .env file)
api:
	cd apps/backend && go run ./cmd/api

## Run the background worker (requires `make up` first and a .env file)
worker:
	cd apps/backend && go run ./cmd/worker

## Apply all pending database migrations
migrate:
	cd apps/backend && go run ./cmd/migrate up

## Roll back the most recent migration
migrate-down:
	cd apps/backend && go run ./cmd/migrate down

## Show migration status
migrate-status:
	cd apps/backend && go run ./cmd/migrate status

## Seed default plans (free / pro / enterprise) — idempotent
seed:
	cd apps/backend && go run ./cmd/seed

## Run the Next.js dev server
web:
	cd apps/frontend && pnpm dev

## Build backend binaries and frontend production build
build:
	cd apps/backend && go build -o bin/api ./cmd/api && go build -o bin/worker ./cmd/worker
	cd apps/frontend && pnpm build

## Run backend tests
test:
	cd apps/backend && go test ./...

## Lint the backend (golangci-lint if installed, else go vet)
lint:
	cd apps/backend && (command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || go vet ./...)

## Tidy go.mod/go.sum
tidy:
	cd apps/backend && go mod tidy

## Format Go source
fmt:
	cd apps/backend && go fmt ./...

## Regenerate sqlc query code (requires sqlc installed: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest)
sqlc:
	cd apps/backend && sqlc generate

## Regenerate Swagger/OpenAPI docs (requires swag installed: go install github.com/swaggo/swag/cmd/swag@latest)
## Deliberately WITHOUT --useStructName: that flag names each definition by its
## bare Go struct name, so every module's CreateRequest/SuccessResponse collapsed
## into one definition and whichever package swag parsed last silently won —
## /swagger showed organization.CreateRequest as the body of POST /mcp-keys and
## POST /connectors. Package-qualified names are uglier but cannot collide as
## modules are added. Do not re-add the flag.
swagger:
	cd apps/backend && swag init -g cmd/api/main.go -o docs --parseDependency --parseInternal
