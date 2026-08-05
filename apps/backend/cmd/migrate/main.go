// Command migrate applies goose database migrations embedded in the
// migrations package. It reads DATABASE_URL via the shared config loader so
// validation matches the API server, and requires no external goose CLI.
//
// Usage: go run ./cmd/migrate [up|down|status|version|reset] [args...]
// Defaults to "up" when no command is given.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"

	"github.com/joho/godotenv"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/junctera/backend/internal/config"
	applogger "github.com/junctera/backend/internal/shared/logger"
	"github.com/junctera/backend/migrations"
)

func main() {
	loadDotEnv()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	log := applogger.New(cfg.AppEnv, cfg.LogLevel)

	command := "up"
	var args []string
	if len(os.Args) > 1 {
		command = os.Args[1]
		args = os.Args[2:]
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Error("failed to set goose dialect", "error", err)
		os.Exit(1)
	}

	if err := goose.RunContext(context.Background(), command, db, ".", args...); err != nil {
		log.Error("migration failed", "command", command, "error", err)
		os.Exit(1)
	}

	log.Info("migration command completed", "command", command)
}

// loadDotEnv loads environment variables from a .env file if one is found,
// mirroring cmd/api's lookup so `go run ./cmd/migrate` works the same way
// regardless of the working directory it's invoked from.
func loadDotEnv() {
	for _, path := range []string{"../../.env", "../.env", ".env"} {
		if err := godotenv.Load(path); err == nil {
			return
		}
	}
}
