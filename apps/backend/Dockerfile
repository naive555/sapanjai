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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/worker ./cmd/worker
# migrate and seed ship in the same image so a deployment can run the schema
# and plan seeding as a one-off (k8s/migrate/job.yaml) without a second build.
# Migrations are embedded in the binary, so no SQL files need copying.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/migrate ./cmd/migrate
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/seed ./cmd/seed

########################
# Runner (distroless, nonroot)
########################
FROM gcr.io/distroless/static-debian12:nonroot AS runner
WORKDIR /app

COPY --from=builder /out/api ./api
COPY --from=builder /out/healthcheck ./healthcheck
COPY --from=builder /out/worker ./worker
COPY --from=builder /out/migrate ./migrate
COPY --from=builder /out/seed ./seed

EXPOSE 3000

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/app/healthcheck"]

# CMD, not ENTRYPOINT: the worker, migrate and seed binaries share this image
# and are selected by overriding the container command. An exec-form ENTRYPOINT
# would make those overrides *append* as arguments to /app/api instead of
# replacing it — the API would silently start in the worker's place — and
# platforms that expose only a "start command" (Railway, Fly, Cloud Run) offer
# no way to reset the entrypoint. compose.yaml and k8s/ override this the same
# way.
CMD ["/app/api"]
