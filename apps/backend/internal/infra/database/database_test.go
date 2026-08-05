package database_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/junctera/backend/internal/infra/database"
	"github.com/junctera/backend/internal/infra/database/db"
	"github.com/junctera/backend/migrations"
)

// TestUserSessionPlanRoundTrip exercises exactly the query surface the
// Phase 2 auth service will call: create/find a user, create a session,
// detect-and-revoke a token family, and idempotently upsert a plan. It runs
// migrations against the real DATABASE_URL first, so it doubles as a
// migration smoke test.
func TestUserSessionPlanRoundTrip(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open database/sql: %v", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	pool, err := database.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer pool.Close()

	store := database.NewStore(pool)

	email := "integration-" + uuid.NewString() + "@example.com"

	// 1. CreateUser
	user, err := store.CreateUser(ctx, db.CreateUserParams{
		Email:        email,
		PasswordHash: "bcrypt-hash-placeholder",
		DisplayName:  nil,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Email != email {
		t.Fatalf("CreateUser: got email %q, want %q", user.Email, email)
	}
	if user.ID == uuid.Nil {
		t.Fatal("CreateUser: got nil id")
	}

	// 2. GetUserByEmail
	fetched, err := store.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if fetched.ID != user.ID {
		t.Fatalf("GetUserByEmail: got id %v, want %v", fetched.ID, user.ID)
	}

	// 3. CreateSession
	family := uuid.New()
	session, err := store.CreateSession(ctx, db.CreateSessionParams{
		UserID:       user.ID,
		RefreshToken: "refresh-token-" + uuid.NewString(),
		Family:       family,
		ExpiresAt:    time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if session.IsRevoked {
		t.Fatal("CreateSession: new session should not be revoked")
	}

	fetchedSession, err := store.GetSessionByRefreshToken(ctx, session.RefreshToken)
	if err != nil {
		t.Fatalf("GetSessionByRefreshToken: %v", err)
	}
	if fetchedSession.IsRevoked {
		t.Fatal("GetSessionByRefreshToken: expected is_revoked = false")
	}

	// 4. RevokeSessionFamily
	if err := store.RevokeSessionFamily(ctx, family); err != nil {
		t.Fatalf("RevokeSessionFamily: %v", err)
	}
	revoked, err := store.GetSessionByRefreshToken(ctx, session.RefreshToken)
	if err != nil {
		t.Fatalf("GetSessionByRefreshToken after revoke: %v", err)
	}
	if !revoked.IsRevoked {
		t.Fatal("RevokeSessionFamily: expected is_revoked = true")
	}

	// 5. UpsertPlan idempotency
	limits, err := json.Marshal(map[string]int{"max_members": 5, "max_roles": 3})
	if err != nil {
		t.Fatalf("marshal limits: %v", err)
	}
	for i := range 2 {
		if err := store.UpsertPlan(ctx, db.UpsertPlanParams{Name: "free", Limits: limits}); err != nil {
			t.Fatalf("UpsertPlan (attempt %d): %v", i+1, err)
		}
	}
	plan, err := store.GetPlanByName(ctx, "free")
	if err != nil {
		t.Fatalf("GetPlanByName: %v", err)
	}
	if plan.Name != "free" {
		t.Fatalf("GetPlanByName: got name %q, want %q", plan.Name, "free")
	}
}
