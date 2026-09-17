package billing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/subscription"
)

// maxToolCallsPerMonthLimitKey mirrors mcp.maxToolCallsPerMonthLimitKey —
// the same plans.limits key, restated here rather than imported for the
// same reason currentUsagePeriod below restates mcp's billing-period
// boundary instead of importing it: billing must not depend on mcp (nor
// the reverse — mcp already depends on subscription, and a mcp->billing
// edge would risk a cycle the moment billing needed anything mcp-shaped).
// If this key is ever renamed, it must be renamed in both places.
const maxToolCallsPerMonthLimitKey = "max_tool_calls_per_month"

// currentUsagePeriod returns the UTC calendar-month bucket GET
// /billing/usage reports on: [start, end).
//
// This MUST bucket on the exact same boundary
// internal/module/mcp/service.go's currentBillingPeriodStart does
// (date_trunc('month', occurred_at)) and internal/job/usagerollup folds
// usage_events on — three independent call sites computing "the current
// month" have to agree on where it starts, or this endpoint's callCount
// could read under (or over) the number the gateway is actually enforcing
// at that exact moment. Per CLAUDE.md/the plan brief: "a meter that reads
// under the cap it is metering is a support ticket." Duplicated rather
// than shared via an exported helper for the same reason
// maxToolCallsPerMonthLimitKey above is duplicated — if this boundary ever
// needs to move, it moves in both places, deliberately, not by accident
// through a shared import edge that shouldn't exist.
func currentUsagePeriod() (start, end time.Time) {
	now := time.Now().UTC()
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, 1, 0)
	return start, end
}

// limitResolver is the subset of *subscription.Service GET /billing/usage
// depends on to answer "what is this org's monthly tool-call cap" — the
// billing:read mirror of planAssigner's billing:write seam (webhook.go),
// narrowed the same way: one method, declared by this consumer, injected
// from server.go, so billing still never imports subscription beyond an
// interface it declares itself (plan invariant 1: entitlement resolution
// stays subscription.Service's alone).
//
// Not EffectiveLimits (admin's subscriptionResolver takes that one): there
// is no per-key merge to render here, only the one
// max_tool_calls_per_month scalar the mcp gateway itself enforces, and
// GetLimit already resolves it with the same custom-over-plan precedence.
type limitResolver interface {
	GetLimit(ctx context.Context, organizationID uuid.UUID, key string) (*float64, error)
}

var _ limitResolver = (*subscription.Service)(nil)

// Usage answers GET /billing/usage for organizationID: the live tool-call
// count for the current UTC calendar month, the resolved monthly cap, and
// a per-tool breakdown.
//
// Three reads, no writes, and no Stripe call (plan invariant 1 — this
// package still answers "what may this org do" from Postgres alone, at
// gateway-adjacent latency, with Stripe down). Order doesn't matter between
// them; they're independent.
func (s *Service) Usage(ctx context.Context, organizationID uuid.UUID) (UsageResponse, error) {
	periodStart, periodEnd := currentUsagePeriod()

	count, err := s.store.CountUsageEventsForOrgSince(ctx, db.CountUsageEventsForOrgSinceParams{
		OrganizationID: organizationID,
		Since:          periodStart,
	})
	if err != nil {
		return UsageResponse{}, err
	}

	limit, err := s.limits.GetLimit(ctx, organizationID, maxToolCallsPerMonthLimitKey)
	if err != nil {
		return UsageResponse{}, err
	}

	rollups, err := s.store.ListUsageRollupsForOrgPeriod(ctx, db.ListUsageRollupsForOrgPeriodParams{
		OrganizationID: organizationID,
		PeriodStart:    periodStart,
	})
	if err != nil {
		return UsageResponse{}, err
	}

	byTool := make([]ToolUsageResponse, len(rollups))
	for i, r := range rollups {
		byTool[i] = ToolUsageResponse{Tool: r.Tool, CallCount: r.CallCount}
	}

	resp := UsageResponse{
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		CallCount:   count,
		ByTool:      byTool,
	}
	// -1 is the existing "enterprise/unlimited" sentinel (EnforceLimit); a
	// nil *limit is GetLimit's own "no subscription at all" case. Both
	// collapse to nil here — see UsageResponse.Limit's comment for why.
	if limit != nil && *limit != -1 {
		l := int64(*limit)
		resp.Limit = &l
	}
	return resp, nil
}
