// Command seed inserts the default subscription plans (free, pro,
// enterprise), mirroring src/infrastructure/database/seed.ts in the source
// app. It is idempotent: re-running is a no-op for plans that already exist.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/joho/godotenv"
	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	applogger "github.com/sapanjai/backend/internal/shared/logger"
)

var defaultPlans = []struct {
	name   string
	limits map[string]int
}{
	{"free", map[string]int{"max_members": 5, "max_roles": 3, "max_connectors": 2}},
	{"pro", map[string]int{"max_members": 50, "max_roles": 20, "max_connectors": 10}},
	{"enterprise", map[string]int{"max_members": -1, "max_roles": -1, "max_connectors": -1}}, // -1 = unlimited
}

func main() {
	loadDotEnv()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	log := applogger.New(cfg.AppEnv, cfg.LogLevel)

	ctx := context.Background()

	pool, err := database.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := database.NewStore(pool)

	log.Info("Seeding plans...")
	for _, p := range defaultPlans {
		limits, err := json.Marshal(p.limits)
		if err != nil {
			log.Error("failed to marshal plan limits", "plan", p.name, "error", err)
			os.Exit(1)
		}

		if err := store.UpsertPlan(ctx, db.UpsertPlanParams{
			Name:   p.name,
			Limits: limits,
		}); err != nil {
			log.Error("failed to seed plan", "plan", p.name, "error", err)
			os.Exit(1)
		}
	}
	log.Info("Done")
}

// loadDotEnv loads environment variables from a .env file if one is found,
// mirroring cmd/api's lookup so `go run ./cmd/seed` works the same way
// regardless of the working directory it's invoked from.
func loadDotEnv() {
	for _, path := range []string{"../../.env", "../.env", ".env"} {
		if err := godotenv.Load(path); err == nil {
			return
		}
	}
}
