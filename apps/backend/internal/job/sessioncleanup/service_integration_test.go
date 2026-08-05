package sessioncleanup_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/junctera/backend/internal/infra/database"
	"github.com/junctera/backend/internal/infra/database/db"
	"github.com/junctera/backend/internal/job/sessioncleanup"
	"github.com/junctera/backend/migrations"
)

// setupIntegrationStore skips unless DATABASE_URL is set, runs migrations
// against it, and returns a ready *database.Store — same pattern as
// internal/infra/database/database_test.go and
// internal/server/auth_integration_test.go.
func setupIntegrationStore(t *testing.T) (*database.Store, context.Context) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open database/sql: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

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
	t.Cleanup(pool.Close)

	return database.NewStore(pool), ctx
}

// seedUser creates a uuid-suffixed user and registers cleanup of it (and,
// via ON DELETE CASCADE, any sessions still attached to it when the test
// ends).
func seedUser(t *testing.T, ctx context.Context, store *database.Store) uuid.UUID {
	t.Helper()

	email := "sessioncleanup-" + uuid.NewString() + "@example.com"
	user, err := store.CreateUser(ctx, db.CreateUserParams{
		Email:        email,
		PasswordHash: "bcrypt-hash-placeholder",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user.ID)
	})
	return user.ID
}

// insertSession inserts a session row with explicit expires_at/created_at,
// bypassing sqlc's CreateSession (which cannot backdate created_at) so
// fixtures can simulate rows of any age.
//
// expires_at/created_at are `timestamp` (no time zone) columns: pgx encodes
// a time.Time's wall-clock digits as-is, dropping its offset, so a
// non-UTC-located value stores the wrong instant relative to the session's
// `now()` (UTC). The auth handler already works around this the same way
// (see its time.Now().UTC() calls) — .UTC() here keeps the fixtures
// consistent with that convention.
func insertSession(t *testing.T, ctx context.Context, store *database.Store, userID uuid.UUID, expiresAt time.Time, revoked bool, createdAt time.Time) uuid.UUID {
	t.Helper()

	id := uuid.New()
	_, err := store.Pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, refresh_token, family, is_revoked, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, id, userID, "refresh-"+uuid.NewString(), uuid.New(), revoked, expiresAt.UTC(), createdAt.UTC())
	if err != nil {
		t.Fatalf("insert session fixture: %v", err)
	}
	return id
}

func sessionExists(t *testing.T, ctx context.Context, store *database.Store, id uuid.UUID) bool {
	t.Helper()

	var exists bool
	if err := store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("check session exists: %v", err)
	}
	return exists
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// lastAttrInt extracts an int value from a Result.Attrs slog-style
// key/value slice ("batches", 3, "drained", true, ...).
func lastAttrInt(attrs []any, key string) (int, bool) {
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key {
			if v, ok := attrs[i+1].(int); ok {
				return v, true
			}
		}
	}
	return 0, false
}

// TestSessionCleanup_DeletesExpiredAndOldRevoked_KeepsActiveAndRecentlyRevoked
// encodes the exact fixture matrix from the Phase 7 plan: expired and
// long-revoked sessions are deleted; active and recently-revoked sessions
// (still inside the retention window) survive.
func TestSessionCleanup_DeletesExpiredAndOldRevoked_KeepsActiveAndRecentlyRevoked(t *testing.T) {
	store, ctx := setupIntegrationStore(t)
	userID := seedUser(t, ctx, store)

	now := time.Now()
	retention := 30 * 24 * time.Hour

	expired := insertSession(t, ctx, store, userID, now.Add(-1*time.Hour), false, now)
	revokedOld := insertSession(t, ctx, store, userID, now.Add(24*time.Hour), true, now.Add(-60*24*time.Hour))
	revokedRecent := insertSession(t, ctx, store, userID, now.Add(24*time.Hour), true, now.Add(-1*24*time.Hour))
	active := insertSession(t, ctx, store, userID, now.Add(24*time.Hour), false, now)

	job := sessioncleanup.New(store, newTestLogger(), time.Hour, retention, 1000)

	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Other tests/processes may have their own expired rows in flight; only
	// assert a floor, never an exact global count.
	if res.Affected < 2 {
		t.Fatalf("expected at least 2 rows deleted (this fixture's expired + revoked-old), got %d", res.Affected)
	}

	if sessionExists(t, ctx, store, expired) {
		t.Error("expected the expired session to be deleted")
	}
	if sessionExists(t, ctx, store, revokedOld) {
		t.Error("expected the old-revoked session to be deleted")
	}
	if !sessionExists(t, ctx, store, revokedRecent) {
		t.Error("expected the recently-revoked session to be kept")
	}
	if !sessionExists(t, ctx, store, active) {
		t.Error("expected the active session to be kept")
	}
}

// TestSessionCleanup_BatchesAcrossMultipleRuns proves the batching loop
// actually issues multiple DELETE statements against a real table rather
// than relying on the mock's call-count bookkeeping.
func TestSessionCleanup_BatchesAcrossMultipleRuns(t *testing.T) {
	store, ctx := setupIntegrationStore(t)
	userID := seedUser(t, ctx, store)

	now := time.Now()
	var ids []uuid.UUID
	for range 5 {
		ids = append(ids, insertSession(t, ctx, store, userID, now.Add(-1*time.Hour), false, now))
	}

	job := sessioncleanup.New(store, newTestLogger(), time.Hour, 30*24*time.Hour, 2)

	res, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Affected < 5 {
		t.Fatalf("expected at least 5 rows deleted, got %d", res.Affected)
	}

	batches, ok := lastAttrInt(res.Attrs, "batches")
	if !ok || batches < 3 {
		t.Fatalf("expected at least 3 batches for 5 rows at batchSize=2, got %v (attrs=%v)", batches, res.Attrs)
	}

	for _, id := range ids {
		if sessionExists(t, ctx, store, id) {
			t.Errorf("expected seeded session %s to be deleted", id)
		}
	}
}
