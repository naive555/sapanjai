// Command grantadmin promotes (or demotes) an existing user to a platform
// role. It is deliberately promote-only: it looks up a user by email and
// updates users.platform_role, and never creates a user or sets a
// password.
//
// Why not seed a superadmin account instead (see docs/11-admin-panel.md §3):
// a seed that mints a privileged account with a known credential is a
// production incident waiting for someone to run `make seed` against the
// wrong DATABASE_URL. Promoting an account that already proved mailbox
// control (it registered and logged in normally) is the safe shape.
//
// Usage:
//
//	go run ./cmd/grantadmin -email user@example.com -role superadmin
//	go run ./cmd/grantadmin -email user@example.com -role support
//	go run ./cmd/grantadmin -email user@example.com -role none   # revoke
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	applogger "github.com/sapanjai/backend/internal/shared/logger"
)

// validRoles mirrors the users_platform_role_check CHECK constraint from
// migration 00011 (docs/11-admin-panel.md D2: two platform roles today, a
// third is one CHECK change away).
var validRoles = map[string]bool{
	"superadmin": true,
	"support":    true,
}

// errUnknownEmail is grantPlatformRole's sentinel for "no user row matches
// this email" — kept distinct from any other store error so main can print
// the promote-only guidance instead of a bare "no such user" for what might
// otherwise be a transient DB failure.
var errUnknownEmail = errors.New("no such user")

// grantStore is the subset of *database.Store grantPlatformRole depends on,
// narrowed so a unit test can hand-mock it without a real database.
type grantStore interface {
	GetUserByEmail(ctx context.Context, email string) (db.User, error)
	SetUserPlatformRole(ctx context.Context, arg db.SetUserPlatformRoleParams) error
}

// grantPlatformRole looks up email and sets its platform_role to role
// ("none" clears it to NULL). It never creates a user. Returns
// errUnknownEmail (wrapped) when no user matches, so callers can
// distinguish "this account doesn't exist yet" from any other store
// failure.
func grantPlatformRole(ctx context.Context, store grantStore, email, role string) error {
	user, err := store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %q", errUnknownEmail, email)
		}
		return fmt.Errorf("look up user: %w", err)
	}

	var platformRole *string
	if role != "none" {
		platformRole = &role
	}

	if err := store.SetUserPlatformRole(ctx, db.SetUserPlatformRoleParams{
		ID:           user.ID,
		PlatformRole: platformRole,
	}); err != nil {
		return fmt.Errorf("set platform role: %w", err)
	}

	return nil
}

func main() {
	loadDotEnv()

	email := flag.String("email", "", "email of an existing user to grant/revoke a platform role for")
	role := flag.String("role", "", `platform role to grant: "superadmin", "support", or "none" to revoke`)
	flag.Parse()

	if *email == "" || *role == "" {
		fmt.Fprintln(os.Stderr, "usage: grantadmin -email user@example.com -role superadmin|support|none")
		os.Exit(1)
	}
	if *role != "none" && !validRoles[*role] {
		fmt.Fprintf(os.Stderr, "invalid -role %q: must be \"superadmin\", \"support\", or \"none\"\n", *role)
		os.Exit(1)
	}

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

	if err := grantPlatformRole(ctx, store, *email, *role); err != nil {
		if errors.Is(err, errUnknownEmail) {
			fmt.Fprintf(os.Stderr, "no such user %q — register normally first, then grant\n", *email)
			os.Exit(1)
		}
		log.Error("grantadmin failed", "error", err)
		os.Exit(1)
	}

	if *role == "none" {
		log.Info("revoked platform role", "email", *email)
	} else {
		log.Info("granted platform role", "email", *email, "role", *role)
	}
}

// loadDotEnv loads environment variables from a .env file if one is found,
// mirroring cmd/seed's lookup so `go run ./cmd/grantadmin` works the same
// way regardless of the working directory it's invoked from.
func loadDotEnv() {
	for _, path := range []string{"../../.env", "../.env", ".env"} {
		if err := godotenv.Load(path); err == nil {
			return
		}
	}
}
