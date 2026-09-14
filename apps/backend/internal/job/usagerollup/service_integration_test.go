package usagerollup_test

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

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/job/usagerollup"
	"github.com/sapanjai/backend/migrations"
)

// setupIntegrationStore skips unless DATABASE_URL is set, runs migrations
// against it, and returns a ready *database.Store — same pattern as
// internal/job/sessioncleanup/service_integration_test.go.
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

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedOrg creates a uuid-suffixed organization and registers its cleanup.
// usage_events/usage_rollups both cascade from organizations (migration
// 00014), so deleting the org takes its fixtures with it.
func seedOrg(t *testing.T, ctx context.Context, store *database.Store) uuid.UUID {
	t.Helper()

	org, err := store.CreateOrganization(ctx, db.CreateOrganizationParams{
		Name: "usagerollup-test-" + uuid.NewString(),
		Slug: "usagerollup-test-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, org.ID)
	})
	return org.ID
}

// insertUsageEvent inserts a usage_events row with an explicit occurred_at,
// bypassing sqlc's CreateUsageEvent (which always defaults to now()) so
// fixtures can place events in any period.
func insertUsageEvent(t *testing.T, ctx context.Context, store *database.Store, orgID uuid.UUID, tool string, occurredAt time.Time) uuid.UUID {
	t.Helper()

	id := uuid.New()
	_, err := store.Pool.Exec(ctx, `
		INSERT INTO usage_events (id, organization_id, tool, occurred_at)
		VALUES ($1, $2, $3, $4)
	`, id, orgID, tool, occurredAt.UTC())
	if err != nil {
		t.Fatalf("insert usage_events fixture: %v", err)
	}
	return id
}

// insertAuditLogToolCalled inserts an audit_logs row with action
// 'mcp.tool.called' and an explicit created_at, mirroring
// auditlog.Service.Record's shape closely enough for the drift cross-check.
func insertAuditLogToolCalled(t *testing.T, ctx context.Context, store *database.Store, orgID uuid.UUID, createdAt time.Time) uuid.UUID {
	t.Helper()

	id := uuid.New()
	_, err := store.Pool.Exec(ctx, `
		INSERT INTO audit_logs (id, organization_id, action, metadata, created_at)
		VALUES ($1, $2, 'mcp.tool.called', '{}'::jsonb, $3)
	`, id, orgID, createdAt.UTC())
	if err != nil {
		t.Fatalf("insert audit_logs fixture: %v", err)
	}
	return id
}

// seedRollup directly inserts a usage_rollups row, standing in for a rollup
// that was correctly computed on an earlier run while its period was still
// inside the recompute bound.
func seedRollup(t *testing.T, ctx context.Context, store *database.Store, orgID uuid.UUID, periodStart, periodEnd time.Time, tool string, callCount int32) {
	t.Helper()

	_, err := store.Pool.Exec(ctx, `
		INSERT INTO usage_rollups (organization_id, period_start, period_end, tool, call_count)
		VALUES ($1, $2, $3, $4, $5)
	`, orgID, periodStart.UTC(), periodEnd.UTC(), tool, callCount)
	if err != nil {
		t.Fatalf("insert usage_rollups fixture: %v", err)
	}
}

func rollupCallCount(t *testing.T, ctx context.Context, store *database.Store, orgID uuid.UUID, periodStart time.Time, tool string) (int32, bool) {
	t.Helper()

	var count int32
	err := store.Pool.QueryRow(ctx, `
		SELECT call_count FROM usage_rollups
		WHERE organization_id = $1 AND period_start = $2 AND tool = $3
	`, orgID, periodStart.UTC(), tool).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("query usage_rollups: %v", err)
	}
	return count, true
}

func usageEventExists(t *testing.T, ctx context.Context, store *database.Store, id uuid.UUID) bool {
	t.Helper()

	var exists bool
	if err := store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_events WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("check usage_events exists: %v", err)
	}
	return exists
}

func attrInt64(t *testing.T, attrs []any, key string) int64 {
	t.Helper()
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key {
			v, ok := attrs[i+1].(int64)
			if !ok {
				t.Fatalf("Attrs[%q] is not an int64: %v (%T)", key, attrs[i+1], attrs[i+1])
			}
			return v
		}
	}
	t.Fatalf("Attrs missing key %q: %v", key, attrs)
	return 0
}

// currentMonthStartUTC mirrors the package's own rollupSince bucketing so
// tests can compute the same period_start a real run would.
func currentMonthStartUTC(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// TestUsageRollup_IdempotentAcrossTwoRuns encodes the plan's headline
// metering expectation: running the rollup twice over the same window must
// not double the count. Regressing this (e.g. an INSERT instead of an
// UPSERT, or a conflict target that isn't the natural key) would double
// every customer's monthly count on the very next scheduled run.
func TestUsageRollup_IdempotentAcrossTwoRuns(t *testing.T) {
	store, ctx := setupIntegrationStore(t)
	orgID := seedOrg(t, ctx, store)

	const tool = "sheets_query_rows"
	now := time.Now().UTC()
	periodStart := currentMonthStartUTC(now)

	const n = 4
	for i := 0; i < n; i++ {
		insertUsageEvent(t, ctx, store, orgID, tool, now)
	}

	job := usagerollup.New(store, newTestLogger(), time.Hour, 2160*time.Hour, 1000)

	if _, err := job.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	count1, ok := rollupCallCount(t, ctx, store, orgID, periodStart, tool)
	if !ok {
		t.Fatal("expected a usage_rollups row after the first run")
	}
	if count1 != n {
		t.Fatalf("after 1st run: call_count = %d, want %d", count1, n)
	}

	if _, err := job.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	count2, ok := rollupCallCount(t, ctx, store, orgID, periodStart, tool)
	if !ok {
		t.Fatal("expected the usage_rollups row to still exist after the second run")
	}
	if count2 != n {
		t.Fatalf("after 2nd run: call_count = %d, want %d (not doubled)", count2, n)
	}
}

// TestUsageRollup_DriftCrossCheck_ReportsDriftWhenRowsDeletedUnderneath
// proves the audit_logs cross-check actually moves when usage_events rows
// disappear out from under it — "two independent counters disagreeing is
// the detection mechanism" for undercounting (plan's "the metering problem,
// stated plainly").
//
// Measured as a delta rather than an absolute value: both counting queries
// are deliberately cross-org (no organization_id predicate, since that's
// the index they need to use — see usage.sql), so a shared test database
// may carry rows from other fixtures/tests. A relative measurement is
// robust to that; an exact-zero assertion would not be.
func TestUsageRollup_DriftCrossCheck_ReportsDriftWhenRowsDeletedUnderneath(t *testing.T) {
	store, ctx := setupIntegrationStore(t)
	orgID := seedOrg(t, ctx, store)

	const tool = "sheets_query_rows"
	now := time.Now().UTC()

	const n = 5
	var eventIDs []uuid.UUID
	for i := 0; i < n; i++ {
		eventIDs = append(eventIDs, insertUsageEvent(t, ctx, store, orgID, tool, now))
	}
	for i := 0; i < n; i++ {
		insertAuditLogToolCalled(t, ctx, store, orgID, now)
	}

	job := usagerollup.New(store, newTestLogger(), time.Hour, 2160*time.Hour, 1000)

	res1, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	baselineDrift := attrInt64(t, res1.Attrs, "drift")

	// Simulate rows "deleted underneath it" (a lost write elsewhere, a
	// manual delete — the cross-check doesn't care why): remove some of the
	// usage_events rows just inserted, but leave every matching audit_logs
	// row in place.
	const deleted = 2
	for _, id := range eventIDs[:deleted] {
		if _, err := store.Pool.Exec(ctx, `DELETE FROM usage_events WHERE id = $1`, id); err != nil {
			t.Fatalf("delete usage_events fixture: %v", err)
		}
	}

	res2, err := job.Run(ctx)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	driftAfterDelete := attrInt64(t, res2.Attrs, "drift")

	if got, want := driftAfterDelete-baselineDrift, int64(deleted); got != want {
		t.Fatalf("drift increased by %d after deleting %d usage_events rows, want %d", got, deleted, want)
	}
}

// TestUsageRollup_Prune_DeletesOnlyPastRetentionRows is the prune step in
// isolation: an old row past retention goes, a recent row inside it stays.
func TestUsageRollup_Prune_DeletesOnlyPastRetentionRows(t *testing.T) {
	store, ctx := setupIntegrationStore(t)
	orgID := seedOrg(t, ctx, store)

	now := time.Now()
	retention := 24 * time.Hour

	old := insertUsageEvent(t, ctx, store, orgID, "sheets_query_rows", now.Add(-48*time.Hour))
	recent := insertUsageEvent(t, ctx, store, orgID, "sheets_query_rows", now.Add(-1*time.Hour))

	job := usagerollup.New(store, newTestLogger(), time.Hour, retention, 1000)

	if _, err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if usageEventExists(t, ctx, store, old) {
		t.Error("expected the past-retention usage_events row to be deleted")
	}
	if !usageEventExists(t, ctx, store, recent) {
		t.Error("expected the in-window usage_events row to be kept")
	}
}

// TestUsageRollup_NeverOverwritesAPartiallyPrunedPeriodWithASmallerCount is
// the correctness trap from the package doc, encoded directly: a period
// whose raw events have already been partly removed must never have its
// durable rollup overwritten with the smaller count computed from what
// remains.
//
// It exercises the real bound (rollupSince/rollupLookbackMonths), not a
// mocked clock: the fixture period is placed a full 4 calendar months
// before the real current month, which is outside rollupLookbackMonths (3)
// by construction, so a correct job must never touch it. If a future change
// widened the lookback (or removed the bound entirely), this test would
// catch the overwrite it exists to prevent.
func TestUsageRollup_NeverOverwritesAPartiallyPrunedPeriodWithASmallerCount(t *testing.T) {
	store, ctx := setupIntegrationStore(t)
	orgID := seedOrg(t, ctx, store)

	const tool = "sheets_query_rows"
	now := time.Now().UTC()
	currentMonthStart := currentMonthStartUTC(now)
	oldPeriodStart := currentMonthStart.AddDate(0, -4, 0)
	oldPeriodEnd := oldPeriodStart.AddDate(0, 1, 0)

	// The "true" historical count, as if correctly rolled up while this
	// period was still inside the recompute bound.
	const trueCount = int32(10)
	seedRollup(t, ctx, store, orgID, oldPeriodStart, oldPeriodEnd, tool, trueCount)

	// Only a handful of raw events for that period survive today — as if a
	// retention prune already removed the rest. A buggy job that widened
	// its recompute window to reach this period would recompute call_count
	// from just these, silently shrinking the durable record.
	insertUsageEvent(t, ctx, store, orgID, tool, oldPeriodStart.Add(2*24*time.Hour))
	insertUsageEvent(t, ctx, store, orgID, tool, oldPeriodStart.Add(3*24*time.Hour))
	insertUsageEvent(t, ctx, store, orgID, tool, oldPeriodStart.Add(4*24*time.Hour))

	// A long retention so this run's own prune step does not delete the
	// survivors out from under the assertion below — the trap under test
	// is the RE-AGGREGATION overwrite, not the prune.
	job := usagerollup.New(store, newTestLogger(), time.Hour, 8760*time.Hour, 1000)

	if _, err := job.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, ok := rollupCallCount(t, ctx, store, orgID, oldPeriodStart, tool)
	if !ok {
		t.Fatal("expected the pre-seeded usage_rollups row to still exist")
	}
	if got != trueCount {
		t.Fatalf("usage_rollups.call_count = %d, want unchanged %d (the old period must not be re-aggregated)", got, trueCount)
	}
}
