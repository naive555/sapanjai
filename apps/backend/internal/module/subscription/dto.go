package subscription

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PlanPriceResponse is one purchasable price under a plan — an
// interval/currency combination — embedded in PlanResponse.Prices. Billing
// plan step 9's addition to GET /plans.
//
// Deliberately NO stripe_price_id. That id is Stripe linkage with no
// business on a tenant-facing catalogue — the same reasoning that keeps
// stripe_customer_id/stripe_subscription_id off SubscriptionResponse below
// — and unlike those two identifiers, a Price id would be directly usable
// against Stripe's own API by anyone who read it off this response. See
// queries/plans.sql's ListActivePlanPricesForPublicPlans comment.
type PlanPriceResponse struct {
	UnitAmount int64  `json:"unitAmount"`
	Currency   string `json:"currency"`
	Interval   string `json:"interval"`
}

// PlanResponse is the plan embedded in SubscriptionResponse, and one
// element of GET /plans' array.
type PlanResponse struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Limits    json.RawMessage `json:"limits"`
	CreatedAt time.Time       `json:"createdAt"`

	// Prices is this plan's active, purchasable prices (billing plan step
	// 9) — empty, never null, for a plan with none configured yet (e.g. a
	// freshly published plan whose admin hasn't added a price, or the
	// "free" plan, which is never meant to be bought through Checkout at
	// all). Only populated on GET /plans' listing (Handler.listPlans);
	// SubscriptionResponse.Plan leaves it empty rather than paying for a
	// second query GET /subscription has no use for.
	Prices []PlanPriceResponse `json:"prices"`
}

// SubscriptionResponse is the GET /subscription body: the org's
// subscription with its plan embedded.
type SubscriptionResponse struct {
	ID             uuid.UUID       `json:"id"`
	OrganizationID uuid.UUID       `json:"organizationId"`
	PlanID         uuid.UUID       `json:"planId"`
	CustomLimits   json.RawMessage `json:"customLimits"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Plan           PlanResponse    `json:"plan"`

	// ---- Stripe state (billing plan step 9) ----
	//
	// Status mirrors Stripe's own subscription status string verbatim
	// (active/past_due/canceled/...) when a Subscription exists, else nil.
	// A lifecycle label, not an identifier — unlike stripe_subscription_id
	// and stripe_customer_id, which this response never carries at all
	// (GetOrgSubscription's comment explains why: they're Stripe linkage
	// with no business leaving the backend, the same reasoning
	// PlanPriceResponse's doc comment gives for omitting stripe_price_id).
	Status            *string    `json:"status"`
	CurrentPeriodEnd  *time.Time `json:"currentPeriodEnd"`
	CancelAtPeriodEnd bool       `json:"cancelAtPeriodEnd"`

	// HasActiveSubscription restates decision 4's canonical "is this org
	// paying" signal ("stripe_subscription_id IS NULL" means not paying)
	// as a boolean instead of exposing the id it's derived from. The
	// frontend uses it to decide whether "Manage billing" has anything to
	// open the Portal onto — an org that has never subscribed has no
	// invoices, no payment method, and nothing to manage.
	HasActiveSubscription bool `json:"hasActiveSubscription"`
}
