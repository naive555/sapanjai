// Command api boots the sapanjai backend: load config, connect to
// Redis and Postgres, start the HTTP server, and shut down gracefully on
// SIGINT/SIGTERM.
//
// @title                      Sapanjai API
// @version                    0.1.0
// @description                Multi-tenant B2B SaaS platform API.
// @BasePath                   /
// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                Type "Bearer {token}" — the JWT access token.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/redis"
	"github.com/sapanjai/backend/internal/server"
	applogger "github.com/sapanjai/backend/internal/shared/logger"
)

func main() {
	loadDotEnv()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	log := applogger.New(cfg.AppEnv, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rdb, err := redis.New(ctx, cfg.RedisURL)
	if err != nil {
		log.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	log.Info("Redis connected")

	pool, err := database.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	log.Info("Database connected")

	e, err := server.New(cfg, log, pool, rdb)
	if err != nil {
		log.Error("failed to build server", "error", err)
		os.Exit(1)
	}

	go func() {
		addr := ":" + cfg.Port
		log.Info("sapanjai-api listening", "addr", addr)
		if err := e.Start(addr); err != nil && err.Error() != "http: Server closed" {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received, shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx, e, pool, rdb); err != nil {
		log.Error("error during shutdown", "error", err)
		os.Exit(1)
	}

	log.Info("shutdown complete")
}

// loadDotEnv loads environment variables from a .env file if one is found.
// It checks the repo root first (the common case when running via
// `make api`, which cds into apps/backend/ before `go run`), then a couple
// of other plausible working directories. It is a no-op — not fatal — if
// none exist, since in Docker/production env vars are injected directly
// (mirrors Bun's optional .env loading in the source app).
func loadDotEnv() {
	for _, path := range []string{"../../.env", "../.env", ".env"} {
		if err := godotenv.Load(path); err == nil {
			return
		}
	}
}
