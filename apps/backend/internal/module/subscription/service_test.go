package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

type mockSubStore struct {
	getOrgSubscriptionWithPlan func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error)
	getOrgSubscription         func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error)
	upsertOrgSubscription      func(ctx context.Context, arg db.UpsertOrgSubscriptionParams) error
	listPlans                  func(ctx context.Context) ([]db.Plan, error)
}

func (m *mockSubStore) GetOrgSubscriptionWithPlan(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
	return m.getOrgSubscriptionWithPlan(ctx, organizationID)
}

func (m *mockSubStore) GetOrgSubscription(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error) {
	return m.getOrgSubscription(ctx, organizationID)
}

func (m *mockSubStore) UpsertOrgSubscription(ctx context.Context, arg db.UpsertOrgSubscriptionParams) error {
	return m.upsertOrgSubscription(ctx, arg)
}

func (m *mockSubStore) ListPlans(ctx context.Context) ([]db.Plan, error) {
	return m.listPlans(ctx)
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func appErrCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperror.Error, got %T (%v)", err, err)
	}
	return appErr.Code
}

func TestEnforceLimit_NoSubscriptionIsUnlimited(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{}, pgx.ErrNoRows
		},
	})

	if err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 1_000_000); err != nil {
		t.Fatalf("expected no error for an org with no subscription, got %v", err)
	}
}

func TestEnforceLimit_NegativeOneIsUnlimited(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{
				PlanLimits: mustMarshal(t, map[string]float64{"max_members": -1}),
			}, nil
		},
	})

	if err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 1_000_000); err != nil {
		t.Fatalf("expected no error for a -1 (enterprise) limit, got %v", err)
	}
}

func TestEnforceLimit_UnderLimitPasses(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{
				PlanLimits: mustMarshal(t, map[string]float64{"max_members": 5}),
			}, nil
		},
	})

	if err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 4); err != nil {
		t.Fatalf("expected no error when currentCount < limit, got %v", err)
	}
}

func TestEnforceLimit_AtLimitFails(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{
				PlanLimits: mustMarshal(t, map[string]float64{"max_members": 5}),
			}, nil
		},
	})

	err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 5)
	if code := appErrCode(t, err); code != apperror.LimitExceeded {
		t.Errorf("code = %q, want %q", code, apperror.LimitExceeded)
	}
}

func TestEnforceLimit_OverLimitFails(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{
				PlanLimits: mustMarshal(t, map[string]float64{"max_members": 5}),
			}, nil
		},
	})

	err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 6)
	if code := appErrCode(t, err); code != apperror.LimitExceeded {
		t.Errorf("code = %q, want %q", code, apperror.LimitExceeded)
	}
}

func TestEnforceLimit_CustomLimitsOverridePlan(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{
				PlanLimits:   mustMarshal(t, map[string]float64{"max_members": 5}),
				CustomLimits: mustMarshal(t, map[string]float64{"max_members": 100}),
			}, nil
		},
	})

	if err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 50); err != nil {
		t.Fatalf("expected custom_limits to override plan limits, got %v", err)
	}
}

func TestEnforceLimit_KeyMissingIsUnlimited(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{
				PlanLimits: mustMarshal(t, map[string]float64{"max_projects": 5}),
			}, nil
		},
	})

	if err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 1_000_000); err != nil {
		t.Fatalf("expected no limit for an unset key, got %v", err)
	}
}

func TestEnforceLimit_DatabaseErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	svc := NewService(&mockSubStore{
		getOrgSubscriptionWithPlan: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
			return db.GetOrgSubscriptionWithPlanRow{}, dbErr
		},
	})

	err := svc.EnforceLimit(context.Background(), uuid.New(), "max_members", 0)
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

func TestGetSubscription_NoSubscriptionReturnsNil(t *testing.T) {
	svc := NewService(&mockSubStore{
		getOrgSubscription: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error) {
			return db.GetOrgSubscriptionRow{}, pgx.ErrNoRows
		},
	})

	sub, err := svc.GetSubscription(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("expected no error for an org with no subscription, got %v", err)
	}
	if sub != nil {
		t.Errorf("expected a nil subscription, got %+v", sub)
	}
}

func TestGetSubscription_ReturnsRow(t *testing.T) {
	orgID := uuid.New()
	row := db.GetOrgSubscriptionRow{OrganizationID: orgID, PlanName: "pro"}
	svc := NewService(&mockSubStore{
		getOrgSubscription: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error) {
			if organizationID != orgID {
				t.Fatalf("GetOrgSubscription called with %v, want %v", organizationID, orgID)
			}
			return row, nil
		},
	})

	sub, err := svc.GetSubscription(context.Background(), orgID)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if sub == nil || sub.PlanName != "pro" {
		t.Errorf("GetSubscription() = %+v, want %+v", sub, row)
	}
}

func TestGetSubscription_DatabaseErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	svc := NewService(&mockSubStore{
		getOrgSubscription: func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error) {
			return db.GetOrgSubscriptionRow{}, dbErr
		},
	})

	_, err := svc.GetSubscription(context.Background(), uuid.New())
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

func TestAssignPlan_ForwardsToUpsert(t *testing.T) {
	orgID := uuid.New()
	planID := uuid.New()
	var gotArg db.UpsertOrgSubscriptionParams
	svc := NewService(&mockSubStore{
		upsertOrgSubscription: func(ctx context.Context, arg db.UpsertOrgSubscriptionParams) error {
			gotArg = arg
			return nil
		},
	})

	if err := svc.AssignPlan(context.Background(), orgID, planID); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if gotArg.OrganizationID != orgID || gotArg.PlanID != planID {
		t.Errorf("UpsertOrgSubscription called with %+v, want org=%v plan=%v", gotArg, orgID, planID)
	}
}

func TestListPlans_ReturnsRows(t *testing.T) {
	want := []db.Plan{{Name: "free"}, {Name: "pro"}, {Name: "enterprise"}}
	svc := NewService(&mockSubStore{
		listPlans: func(ctx context.Context) ([]db.Plan, error) {
			return want, nil
		},
	})

	got, err := svc.ListPlans(context.Background())
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListPlans() returned %d rows, want %d", len(got), len(want))
	}
}

func TestListPlans_DatabaseErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	svc := NewService(&mockSubStore{
		listPlans: func(ctx context.Context) ([]db.Plan, error) {
			return nil, dbErr
		},
	})

	_, err := svc.ListPlans(context.Background())
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}
