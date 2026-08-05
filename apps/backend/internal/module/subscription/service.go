// Package subscription resolves and enforces subscription plan-limit values
// and serves the /subscription module, mirroring SubscriptionService in the
// source app (src/modules/subscription/service.ts).
package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/junctera/backend/internal/infra/database"
	"github.com/junctera/backend/internal/infra/database/db"
	"github.com/junctera/backend/internal/shared/apperror"
)

var _ subStore = (*database.Store)(nil)

// subStore is the subset of *database.Store this service depends on.
type subStore interface {
	GetOrgSubscriptionWithPlan(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error)
	GetOrgSubscription(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error)
	UpsertOrgSubscription(ctx context.Context, arg db.UpsertOrgSubscriptionParams) error
	ListPlans(ctx context.Context) ([]db.Plan, error)
}

// Service resolves and enforces subscription plan limits.
type Service struct {
	store subStore
}

// NewService builds a subscription Service.
func NewService(store subStore) *Service {
	return &Service{store: store}
}

// GetLimit resolves the numeric limit for key, plan limits overlaid by
// custom_limits (custom wins on conflict). Returns nil when the org has no
// subscription at all — the caller treats that as unlimited, mirroring the
// source's `if (!sub) return null`.
func (s *Service) GetLimit(ctx context.Context, organizationID uuid.UUID, key string) (*float64, error) {
	row, err := s.store.GetOrgSubscriptionWithPlan(ctx, organizationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	limits := map[string]float64{}
	if len(row.PlanLimits) > 0 {
		_ = json.Unmarshal(row.PlanLimits, &limits)
	}
	if len(row.CustomLimits) > 0 {
		custom := map[string]float64{}
		if json.Unmarshal(row.CustomLimits, &custom) == nil {
			maps.Copy(limits, custom)
		}
	}

	if v, ok := limits[key]; ok {
		return &v, nil
	}
	return nil, nil
}

// GetSubscription returns organizationID's subscription with its plan
// embedded, or (nil, nil) when the org has no subscription — the handler
// serializes that as JSON null, mirroring the source's
// `db.query.orgSubscriptions.findFirst` returning undefined.
func (s *Service) GetSubscription(ctx context.Context, organizationID uuid.UUID) (*db.GetOrgSubscriptionRow, error) {
	row, err := s.store.GetOrgSubscription(ctx, organizationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// AssignPlan upserts organizationID's subscription to planID, creating the
// row if none exists or switching the plan if one does. Mirrors
// SubscriptionService.assignPlan. A well-formed but nonexistent planID
// surfaces as a foreign-key violation (500), matching the source, which has
// no PLAN_NOT_FOUND check either.
func (s *Service) AssignPlan(ctx context.Context, organizationID, planID uuid.UUID) error {
	return s.store.UpsertOrgSubscription(ctx, db.UpsertOrgSubscriptionParams{
		OrganizationID: organizationID,
		PlanID:         planID,
	})
}

// ListPlans returns every available subscription plan, oldest first (seed
// insertion order). Not present in the source app — added in Phase 6 so the
// frontend can populate a plan picker; see docs/03 "Deviations resolved
// during Phase 6".
func (s *Service) ListPlans(ctx context.Context) ([]db.Plan, error) {
	return s.store.ListPlans(ctx)
}

// EnforceLimit returns apperror.LimitExceeded when currentCount has reached
// or passed the resolved limit. No subscription, no limit for key, or a
// limit of -1 (enterprise/unlimited) all pass. Mirrors
// SubscriptionService.enforceLimit exactly.
func (s *Service) EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
	limit, err := s.GetLimit(ctx, organizationID, key)
	if err != nil {
		return err
	}
	if limit == nil || *limit == -1 {
		return nil
	}
	if float64(currentCount) >= *limit {
		return apperror.New(apperror.LimitExceeded)
	}
	return nil
}
