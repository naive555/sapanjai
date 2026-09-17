package billing

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// mockLimitResolver satisfies limitResolver without pulling in a real
// subscription.Service — the webhook tests already exercise the real one
// (webhook_test.go), so a hand-mock is enough here to drive GetLimit's
// three possible answers (a numeric limit, -1/unlimited, nil/no
// subscription) independently of subscription.Service's own merge logic,
// which is unit-tested in its own package.
type mockLimitResolver struct {
	limit *float64
	err   error
}

func (m *mockLimitResolver) GetLimit(context.Context, uuid.UUID, string) (*float64, error) {
	return m.limit, m.err
}

var _ limitResolver = (*mockLimitResolver)(nil)

func usageStore(count int64, rollups []db.ListUsageRollupsForOrgPeriodRow) *mockBillingStore {
	return &mockBillingStore{
		countUsageEventsForOrgSince: func(_ context.Context, _ db.CountUsageEventsForOrgSinceParams) (int64, error) {
			return count, nil
		},
		listUsageRollupsForOrgPeriod: func(_ context.Context, _ db.ListUsageRollupsForOrgPeriodParams) ([]db.ListUsageRollupsForOrgPeriodRow, error) {
			return rollups, nil
		},
	}
}

func float64Ptr(v float64) *float64 { return &v }

// TestUsage_LiveCallCountAndByToolBreakdown pins the two-table split:
// callCount comes from CountUsageEventsForOrgSince (usage_events, live),
// byTool comes from ListUsageRollupsForOrgPeriod (usage_rollups, allowed
// to lag) — see both queries' comments for why.
func TestUsage_LiveCallCountAndByToolBreakdown(t *testing.T) {
	orgID := uuid.New()
	store := usageStore(47, []db.ListUsageRollupsForOrgPeriodRow{
		{Tool: "sheets_query_rows", CallCount: 30},
		{Tool: "drive_list_folder", CallCount: 17},
	})
	svc := NewService(store, nil, nil, nil, &mockLimitResolver{limit: float64Ptr(50)}, newTestAudit(), testPublicURL, newTestLog())

	got, err := svc.Usage(context.Background(), orgID)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if got.CallCount != 47 {
		t.Fatalf("CallCount = %d, want 47", got.CallCount)
	}
	if got.Limit == nil || *got.Limit != 50 {
		t.Fatalf("Limit = %v, want 50", got.Limit)
	}
	if len(got.ByTool) != 2 {
		t.Fatalf("ByTool = %v, want 2 entries", got.ByTool)
	}
	if got.ByTool[0].Tool != "sheets_query_rows" || got.ByTool[0].CallCount != 30 {
		t.Errorf("ByTool[0] = %+v, want sheets_query_rows/30", got.ByTool[0])
	}
	if got.ByTool[1].Tool != "drive_list_folder" || got.ByTool[1].CallCount != 17 {
		t.Errorf("ByTool[1] = %+v, want drive_list_folder/17", got.ByTool[1])
	}
	// The period always exactly bounds the callCount/byTool query window,
	// half-open, and never crosses a UTC month boundary in either
	// direction — the property currentBillingPeriodStart (mcp package)
	// exists to guarantee for the gateway's own quota check too.
	if got.PeriodEnd.Sub(got.PeriodStart) <= 0 {
		t.Fatalf("PeriodEnd (%v) must be after PeriodStart (%v)", got.PeriodEnd, got.PeriodStart)
	}
	if got.PeriodStart.Location() != got.PeriodEnd.Location() {
		t.Fatalf("period boundaries must share a location")
	}
}

// TestUsage_UnlimitedEncodings pins the deliberate collapse of
// EnforceLimit's two "no limit" cases (a -1 plan limit, and no subscription
// at all) onto the same JSON null, documented on UsageResponse.Limit.
func TestUsage_UnlimitedEncodings(t *testing.T) {
	cases := map[string]*mockLimitResolver{
		"minus one sentinel":     {limit: float64Ptr(-1)},
		"no subscription at all": {limit: nil},
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			store := usageStore(5, nil)
			svc := NewService(store, nil, nil, nil, resolver, newTestAudit(), testPublicURL, newTestLog())

			got, err := svc.Usage(context.Background(), uuid.New())
			if err != nil {
				t.Fatalf("Usage: %v", err)
			}
			if got.Limit != nil {
				t.Fatalf("Limit = %v, want nil (unlimited)", *got.Limit)
			}
		})
	}
}

// TestUsage_EmptyRollupsIsEmptySliceNotNil pins ByTool's "empty, never
// null" contract (UsageResponse.ByTool's comment) for the case a brand-new
// month has usage_events but the rollup job has not run yet.
func TestUsage_EmptyRollupsIsEmptySliceNotNil(t *testing.T) {
	store := usageStore(3, nil)
	svc := NewService(store, nil, nil, nil, &mockLimitResolver{limit: float64Ptr(100)}, newTestAudit(), testPublicURL, newTestLog())

	got, err := svc.Usage(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if got.ByTool == nil {
		t.Fatal("ByTool = nil, want an empty (non-nil) slice so it serializes as [] not null")
	}
	if len(got.ByTool) != 0 {
		t.Fatalf("ByTool = %v, want empty", got.ByTool)
	}
}

// TestUsage_CountErrorPropagates and TestUsage_LimitErrorPropagates pin
// that a genuine infra failure surfaces to the handler as an error (which
// resolves to a 500) rather than being silently swallowed into a zero or
// an unlimited answer — unlike the mcp gateway's OWN quota check
// (internal/module/mcp/service.go:286-309), which fails OPEN on the same
// kind of error because invariant 3 forbids ever blocking a tools/call.
// GET /billing/usage is a read-only reporting endpoint with no call to let
// through, so there is no equivalent reason to paper over the failure here
// — the caller should see that the number could not be produced.
func TestUsage_CountErrorPropagates(t *testing.T) {
	wantErr := context.DeadlineExceeded
	store := &mockBillingStore{
		countUsageEventsForOrgSince: func(context.Context, db.CountUsageEventsForOrgSinceParams) (int64, error) {
			return 0, wantErr
		},
	}
	svc := NewService(store, nil, nil, nil, &mockLimitResolver{limit: float64Ptr(10)}, newTestAudit(), testPublicURL, newTestLog())

	if _, err := svc.Usage(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestUsage_LimitErrorPropagates(t *testing.T) {
	wantErr := context.DeadlineExceeded
	store := usageStore(1, nil)
	svc := NewService(store, nil, nil, nil, &mockLimitResolver{err: wantErr}, newTestAudit(), testPublicURL, newTestLog())

	if _, err := svc.Usage(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected an error, got nil")
	}
}
