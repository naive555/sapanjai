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
	// max_tool_calls_per_month (step 5 of
	// .claude/plans/2026-09-13-billing-and-usage-metering.md) is enforced by
	// subscription.Service.EnforceLimit from the MCP gateway before a
	// tools/call dispatches. These three numbers are provisional pricing
	// input, not an engineering decision: per the plan's decision 1, there
	// are zero customers yet and therefore no observed usage distribution to
	// calibrate a cap against. NOTE: because UpsertPlan is
	// `ON CONFLICT (name) DO NOTHING` (queries/plans.sql), changing a value
	// here has no effect on a database where the plan row already exists --
	// see migration 00015, which is what applies this key (and any future
	// change to it) to already-seeded plans, and keep the two in sync.
	{"free", map[string]int{"max_members": 5, "max_roles": 3, "max_connectors": 2, "max_tool_calls_per_month": 1000}},
	{"pro", map[string]int{"max_members": 50, "max_roles": 20, "max_connectors": 10, "max_tool_calls_per_month": 20000}},
	{"enterprise", map[string]int{"max_members": -1, "max_roles": -1, "max_connectors": -1, "max_tool_calls_per_month": -1}}, // -1 = unlimited
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
