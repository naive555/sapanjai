package billing

import "time"

// CheckoutRequest is the POST /billing/checkout body.
//
// PlanID is a string carrying a UUID rather than a uuid.UUID, following
// rbac.AssignRequest and admin's AssignPlanRequest: `required,uuid` on a
// string gives a clean 422 "Validation failed" for a malformed id, where
// `required` on a uuid.UUID would have to reason about a zero [16]byte.
//
// Interval is optional and defaults to "month" (Service.Checkout). It selects
// which plan_prices row to charge; currency is not a parameter at all, since
// plan decision 3 is THB only. There is deliberately NO field for a currency,
// a coupon, a trial, a quantity, or a price id: every one of those is a way
// for a tenant to influence what it is charged, and the price a plan sells
// for is server-side catalogue data.
type CheckoutRequest struct {
	PlanID   string `json:"planId" validate:"required,uuid"`
	Interval string `json:"interval" validate:"omitempty,oneof=month year"`
}

// RedirectResponse is the body of both /billing routes: the hosted Stripe
// URL the browser should be sent to.
//
// One shape for both because the frontend does the same thing with either —
// window.location.assign(url). Hosted Checkout and the hosted Portal are
// redirects, not embedded flows, which is why apps/frontend loads no
// Stripe.js and needs no CSP work; keep it that way.
type RedirectResponse struct {
	URL string `json:"url"`
}

// WebhookResponse is POST /billing/webhook's 200 body.
//
// Stripe only reads the status code, so the body exists for humans: a
// `curl` or a Stripe CLI `trigger` against a misconfigured deployment gets
// something legible back instead of an empty 200 that could equally have
// come from a proxy. There is deliberately nothing in it about what the
// event did — an unauthenticated caller who guesses a body should learn
// nothing, and a caller holding the signing secret can read the audit log.
type WebhookResponse struct {
	Received bool `json:"received"`
}

// UsageResponse is GET /billing/usage's body: the caller's active
// organization's tool-call usage for the current UTC calendar month.
//
// Answers from Postgres alone (plan invariant 1), same as GET /subscription
// — this route makes no Stripe call. See Service.Usage for how each field
// is resolved and why CallCount and ByTool deliberately read from two
// different tables.
type UsageResponse struct {
	// PeriodStart/PeriodEnd bound the current UTC calendar month as
	// [PeriodStart, PeriodEnd) — the exact boundary
	// date_trunc('month', occurred_at) buckets on, and the same window
	// CountUsageEventsForOrgSince and internal/job/usagerollup both use.
	// Exposed so the frontend never has to derive "this month" itself in
	// the viewer's local timezone and risk disagreeing with what the
	// backend actually enforces.
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`

	// CallCount is the LIVE usage_events count for the current period —
	// the exact query and window internal/module/mcp/service.go's quota
	// check runs before every tools/call dispatch. Deliberately not a
	// usage_rollups sum: that rollup for the still-open current month can
	// be up to USAGE_ROLLUP_INTERVAL stale, and a meter that reads under
	// the cap it is metering is a support ticket — a customer staring at
	// "47 / 50" while their 48th call is refused a moment later.
	CallCount int64 `json:"callCount"`

	// Limit is max_tool_calls_per_month, resolved through
	// subscription.Service.GetLimit — plan limits overlaid by
	// custom_limits, the same precedence EnforceLimit checks against.
	//
	// nil (JSON null) means unlimited, and deliberately collapses BOTH of
	// EnforceLimit's existing "no limit" cases into the one encoding
	// rather than inventing a third state on the wire: a plan whose limits
	// omit the key, and an org with no subscription row at all. The
	// existing -1 sentinel (used elsewhere as "enterprise/unlimited") is
	// translated to nil here rather than round-tripped as -1, because a
	// frontend rendering "-1 calls remaining" is worse than one null
	// check — see the subscription page's formatLimit "∞" convention,
	// which this mirrors.
	Limit *int64 `json:"limit"`

	// ByTool is a per-tool breakdown for the current period, read from
	// usage_rollups (ListUsageRollupsForOrgPeriod) rather than
	// usage_events — see that query's comment for why this field is
	// allowed to lag CallCount by up to USAGE_ROLLUP_INTERVAL: it is a
	// breakdown a customer finds informative, not the number enforcement
	// depends on. Empty, never null, before the rollup job has folded the
	// current period's events even once (e.g. an org's first day on a new
	// month).
	ByTool []ToolUsageResponse `json:"byTool"`
}

// ToolUsageResponse is one usage_rollups row: a tool name and how many
// times it was called in the current period. See UsageResponse.ByTool.
type ToolUsageResponse struct {
	Tool      string `json:"tool"`
	CallCount int32  `json:"callCount"`
}
