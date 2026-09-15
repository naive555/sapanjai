// Package billing implements /billing: starting a Stripe Checkout Session
// for a plan, and opening a Stripe Customer Portal session for an org that
// already has a Stripe Customer.
//
// Three boundaries define this package, all of them from
// .claude/plans/2026-09-13-billing-and-usage-metering.md:
//
//   - Stripe is the billing record; org_subscriptions.plan_id is the
//     entitlement record (invariant 1). This package never answers "what may
//     this org do" — subscription.Service.EffectiveLimits does, from Postgres
//     alone, and must keep working with Stripe down. The dependency runs one
//     way: billing may use subscription, never the reverse.
//   - This package owns no upsert of org_subscriptions (invariant 5).
//     Creating or re-pointing that row is subscription.Service.AssignPlan's
//     job, called from the webhook in step 7. The single write here
//     (ClaimOrgStripeCustomer) touches exactly one column on a row that
//     already exists.
//   - Stripe is told nothing about usage (decision 1). A tool call is a cap
//     enforced by EnforceLimit, not a metered charge. If anything in this
//     package ever starts reporting usage to Stripe, that is scope drift.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

var _ billingStore = (*database.Store)(nil)

// defaultCurrency is the only currency a plan can be bought in today (plan
// decision 3: THB only, one Price per plan). It is a constant rather than
// configuration because "which currency does this customer pay in" is a
// pricing decision with FX and parity consequences, not a deployment knob —
// but plan_prices is keyed by currency, so the first non-Thai customer is a
// row plus a Stripe Price, not a migration.
const defaultCurrency = "thb"

// Billing intervals, matching the plan_prices_interval_check constraint in
// migration 00013. defaultInterval applies when the request omits one.
const (
	intervalMonth   = "month"
	intervalYear    = "year"
	defaultInterval = intervalMonth
)

// metadataOrganizationID is the Stripe metadata key carrying the tenant id
// on the Customer, the Checkout Session, and the Subscription the session
// creates. The webhook (step 7) reads it to map an event back to an org.
// One spelling, defined once — a webhook looking for "org_id" while this
// writes "organization_id" is a silent, total reconciliation failure.
const metadataOrganizationID = "organization_id"

// metadataPlanID is the Stripe metadata key carrying the entitlement plan
// the checkout is for. It is what lets the webhook call AssignPlan with the
// right plan without reverse-engineering it from the Price.
const metadataPlanID = "plan_id"

// billingStore is the subset of *database.Store this service depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface — the convention every other module here follows.
//
// Note the absence of anything that writes plan_id, custom_limits, or
// status: this service reads the catalogue and claims one column. See the
// package comment's invariant 5.
type billingStore interface {
	GetPlanByID(ctx context.Context, id uuid.UUID) (db.Plan, error)
	GetActivePlanPrice(ctx context.Context, arg db.GetActivePlanPriceParams) (db.PlanPrice, error)
	GetOrgBillingRef(ctx context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error)
	ClaimOrgStripeCustomer(ctx context.Context, arg db.ClaimOrgStripeCustomerParams) (*string, error)
	GetOrganizationByID(ctx context.Context, id uuid.UUID) (db.Organization, error)

	// WithTx is the webhook's (webhook.go) alone. The stripe_events claim
	// and the state change it guards must commit or roll back together, or
	// a failure mid-processing leaves the event permanently marked handled
	// and turns Stripe's retry into a silent no-op. Every query the webhook
	// runs goes through the db.Querier this hands out, which is also how
	// subscription.Service.AssignPlanTx joins the same transaction — see
	// Service.reconcile.
	WithTx(ctx context.Context, fn func(q db.Querier) error) error
}

// Service implements POST /billing/checkout and POST /billing/portal.
type Service struct {
	store  billingStore
	stripe stripeAPI
	audit  *auditlog.Service
	log    *slog.Logger

	// webhooks verifies POST /billing/webhook bodies. Separate from stripe
	// above because STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET are
	// independently configurable: a deployment can hold one and not the
	// other, and each half degrades to BILLING_NOT_CONFIGURED on its own
	// rather than dragging the other down.
	webhooks stripeWebhooks

	// subs is the narrow seam onto subscription.Service. The webhook moves
	// plan_id only through it (plan invariant 5); nothing in this package
	// upserts org_subscriptions.
	subs planAssigner

	// appPublicURL is the browser-facing FRONTEND origin (config.AppPublicURL),
	// not this API's. Every URL Stripe redirects a human to — checkout
	// success, checkout cancel, portal return — is a page in apps/frontend,
	// and the API's own address is routinely unreachable from a browser
	// (compose's "http://api:3000", Railway's private domain).
	appPublicURL string
}

// NewService builds a billing Service. stripeClient and webhooks may each
// be nil, which is what server.go passes when STRIPE_SECRET_KEY /
// STRIPE_WEBHOOK_SECRET are unset: the routes still mount and still enforce
// their guards, but the ones that need the missing half answer
// apperror.BillingNotConfigured. That is deliberate — see
// config.Config.BillingEnabled.
func NewService(
	store billingStore,
	stripeClient stripeAPI,
	webhooks stripeWebhooks,
	subs planAssigner,
	audit *auditlog.Service,
	appPublicURL string,
	log *slog.Logger,
) *Service {
	return &Service{
		store:        store,
		stripe:       stripeClient,
		webhooks:     webhooks,
		subs:         subs,
		audit:        audit,
		appPublicURL: appPublicURL,
		log:          log,
	}
}

// Checkout starts a Stripe Checkout Session in subscription mode for
// organizationID against planID, and returns the hosted URL to redirect the
// browser to.
//
// organizationID comes from the caller's verified active organization
// (appmw.OrgID, set by RequirePermission after a membership lookup), never
// from the request body. That is the whole of this route's tenant
// isolation: there is no input through which a caller can name an
// organization they are not a member of, so a cross-tenant checkout is not
// rejected so much as unrepresentable.
//
// Order: configured, plan, price, customer, session. The Stripe calls come
// last so a bad plan id costs no API round trip.
//
// Note that this changes NOTHING about what organizationID is entitled to.
// A completed payment does, via the webhook (step 7) calling
// subscription.Service.AssignPlan. Abandoning the checkout leaves no trace
// beyond a Stripe Customer.
func (s *Service) Checkout(ctx context.Context, organizationID, actorID uuid.UUID, actorEmail, planID, interval string) (string, error) {
	if s.stripe == nil {
		return "", apperror.New(apperror.BillingNotConfigured)
	}

	plan, err := s.resolvePurchasablePlan(ctx, planID)
	if err != nil {
		return "", err
	}

	if interval == "" {
		interval = defaultInterval
	}
	if interval != intervalMonth && interval != intervalYear {
		// Unreachable through the HTTP handler (CheckoutRequest's
		// `oneof=month year` rejects anything else with 422 first). Checked
		// anyway because Checkout is an exported method on a service that
		// other server-side callers can reach without passing through that
		// tag, and because plan_prices_interval_check (migration 00013)
		// permits exactly these two — an unrecognised interval can only ever
		// match zero rows, so answering now saves a pointless query.
		return "", apperror.New(apperror.PlanNotPurchasable)
	}
	price, err := s.store.GetActivePlanPrice(ctx, db.GetActivePlanPriceParams{
		PlanID:          plan.ID,
		Currency:        defaultCurrency,
		BillingInterval: interval,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The plan resolved; the catalogue has no active Price for it in
			// this currency/interval. Distinct from NOT_FOUND on purpose —
			// this is an operator problem, and saying so is the difference
			// between a five-minute fix and an afternoon.
			return "", apperror.New(apperror.PlanNotPurchasable)
		}
		return "", err
	}

	customerID, err := s.ensureCustomer(ctx, organizationID, actorEmail)
	if err != nil {
		return "", err
	}

	orgIDStr := organizationID.String()
	tags := map[string]string{metadataOrganizationID: orgIDStr, metadataPlanID: plan.ID.String()}

	sessionURL, err := s.stripe.CreateCheckoutSession(ctx, checkoutInput{
		CustomerID:        customerID,
		PriceID:           price.StripePriceID,
		SuccessURL:        s.frontendURL("/subscription", "checkout", "success"),
		CancelURL:         s.frontendURL("/subscription", "checkout", "cancelled"),
		ClientReferenceID: orgIDStr,
		Metadata:          tags,
		// The Subscription outlives the Session; a renewal or cancellation
		// event months from now references only the Subscription, so the
		// org id has to be on that object too.
		SubscriptionMetadata: tags,
	})
	if err != nil {
		return "", s.stripeErr("create checkout session", err, organizationID)
	}

	metadata, _ := json.Marshal(map[string]string{
		"planId":   plan.ID.String(),
		"planName": plan.Name,
		"interval": interval,
	})
	s.audit.Record(ctx, auditlog.ActionBillingCheckoutStarted, &actorID, &organizationID, metadata)

	return sessionURL, nil
}

// Portal opens a Stripe Customer Portal session for organizationID and
// returns the hosted URL.
//
// The portal is what buys upgrade, downgrade, cancellation, invoice history,
// and payment-method management without building any of it. That breadth is
// exactly why this route carries the same billing:write guard as checkout
// and never a weaker one: it can cancel the subscription and it exposes
// invoices carrying a billing address. Plan decision 2 calls it "arguably
// the more dangerous of the two".
//
// Like Checkout, organizationID is the caller's verified active org, so one
// org's portal link is unreachable from another org's session.
func (s *Service) Portal(ctx context.Context, organizationID, actorID uuid.UUID, actorEmail string) (string, error) {
	if s.stripe == nil {
		return "", apperror.New(apperror.BillingNotConfigured)
	}

	customerID, err := s.ensureCustomer(ctx, organizationID, actorEmail)
	if err != nil {
		return "", err
	}

	portalURL, err := s.stripe.CreatePortalSession(ctx, portalInput{
		CustomerID: customerID,
		ReturnURL:  s.frontendURL("/subscription", "", ""),
	})
	if err != nil {
		return "", s.stripeErr("create portal session", err, organizationID)
	}

	s.audit.Record(ctx, auditlog.ActionBillingPortalOpened, &actorID, &organizationID, nil)

	return portalURL, nil
}

// resolvePurchasablePlan loads planID and rejects anything a tenant may not
// buy for itself.
//
// A malformed id, a nonexistent plan, and a non-public plan all resolve to
// the same NOT_FOUND: a private plan (plans.is_public = false, migration
// 00013) is typically a negotiated enterprise tier, and letting a caller
// distinguish "no such plan" from "that plan exists but isn't for you" turns
// this route into an oracle for the plan catalogue.
func (s *Service) resolvePurchasablePlan(ctx context.Context, planID string) (db.Plan, error) {
	id, err := uuid.Parse(planID)
	if err != nil {
		return db.Plan{}, apperror.New(apperror.NotFound)
	}

	plan, err := s.store.GetPlanByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Plan{}, apperror.New(apperror.NotFound)
		}
		return db.Plan{}, err
	}
	if !plan.IsPublic {
		return db.Plan{}, apperror.New(apperror.NotFound)
	}
	return plan, nil
}

// ensureCustomer returns organizationID's Stripe Customer id, creating it on
// first use (plan decision 4: Customer yes, Subscription no, and lazily —
// not at org creation, which would fill the Stripe dashboard with a Customer
// per spam signup).
//
// # Not creating two Customers for one org
//
// Two guarantees, because neither alone covers every case:
//
//  1. A deterministic Stripe idempotency key derived from the organization
//     id. Two concurrent creates carrying the same Idempotency-Key return
//     the SAME Customer object from Stripe rather than creating two. This is
//     the guarantee that holds even for an org that has no org_subscriptions
//     row yet — the common case for a brand-new org, since nothing creates
//     that row until a plan is assigned.
//  2. A conditional claim in Postgres — ClaimOrgStripeCustomer's
//     "WHERE stripe_customer_id IS NULL". Of two concurrent writers exactly
//     one matches a row; the loser gets zero rows back, re-reads, and adopts
//     the winner's id. So the database records exactly one Customer per org
//     no matter the interleaving, and never silently overwrites a recorded
//     Customer with a second one.
//
// Together: Stripe cannot mint two objects for one org in the same window,
// and Postgres cannot record two. The residue, stated honestly: for an org
// with no org_subscriptions row, there is nowhere to persist the id, so a
// SECOND checkout attempt after the idempotency key's retention window has
// passed would create a second Stripe Customer. That is an abandoned-cart
// case only — the first checkout that actually completes makes the webhook
// create the row (step 7), after which the id persists and is reused
// forever. The cost is a stray Customer object with no subscription, not a
// double charge and not a mis-attributed payment: every Customer carries
// metadata.organization_id, and the Session carries client_reference_id.
//
// Deliberately NOT solved by inserting an org_subscriptions row here. That
// row is the entitlement record; an org without one resolves to "unlimited"
// through EffectiveLimits, so inserting a free-plan row as a side effect of
// clicking "Upgrade" or "Manage billing" would quietly tighten
// max_members/max_connectors/max_tool_calls_per_month on that org. Creating
// the row is AssignPlan's job, at the moment a subscription actually starts
// (invariant 5).
func (s *Service) ensureCustomer(ctx context.Context, organizationID uuid.UUID, actorEmail string) (string, error) {
	ref, err := s.store.GetOrgBillingRef(ctx, organizationID)
	switch {
	case err == nil:
		if ref.StripeCustomerID != nil && *ref.StripeCustomerID != "" {
			return *ref.StripeCustomerID, nil
		}
	case errors.Is(err, pgx.ErrNoRows):
		// No subscription row yet. Fall through and create the Customer;
		// the claim below will no-op and the id will be recorded by the
		// webhook when a subscription starts.
	default:
		return "", err
	}

	var orgName string
	if org, orgErr := s.store.GetOrganizationByID(ctx, organizationID); orgErr == nil {
		orgName = org.Name
	} else if !errors.Is(orgErr, pgx.ErrNoRows) {
		return "", orgErr
	}

	customerID, err := s.stripe.CreateCustomer(ctx, customerInput{
		OrganizationID: organizationID.String(),
		Name:           orgName,
		Email:          actorEmail,
		IdempotencyKey: customerIdempotencyKey(organizationID),
	})
	if err != nil {
		return "", s.stripeErr("create customer", err, organizationID)
	}

	claimed, err := s.store.ClaimOrgStripeCustomer(ctx, db.ClaimOrgStripeCustomerParams{
		OrganizationID:   organizationID,
		StripeCustomerID: customerID,
	})
	switch {
	case err == nil:
		if claimed != nil && *claimed != "" {
			return *claimed, nil
		}
		return customerID, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Either there is no org_subscriptions row (nothing to claim), or a
		// concurrent request already claimed it. Re-read to tell the two
		// apart: a recorded id wins over the one just created, so the
		// database's answer stays the single answer.
		if ref, readErr := s.store.GetOrgBillingRef(ctx, organizationID); readErr == nil &&
			ref.StripeCustomerID != nil && *ref.StripeCustomerID != "" {
			return *ref.StripeCustomerID, nil
		}
		return customerID, nil
	default:
		return "", err
	}
}

// customerIdempotencyKey derives a stable Stripe Idempotency-Key from the
// organization id. Deterministic on purpose: stripe.NewIdempotencyKey()
// would generate a fresh value per attempt, which protects against a
// retried HTTP request but not against two separate requests both deciding
// to create a Customer for the same org — which is precisely the race this
// needs to survive.
func customerIdempotencyKey(organizationID uuid.UUID) string {
	return "sapanjai-org-customer-" + organizationID.String()
}

// frontendURL builds an absolute URL on the browser-facing frontend origin,
// optionally with one query parameter.
//
// Stripe's success_url supports a {CHECKOUT_SESSION_ID} template, and this
// deliberately does not use it: the frontend has no business fetching a
// Stripe session, and a success page must never be the thing that grants
// entitlements. Payment truth arrives by webhook (step 7), which is the only
// path that survives a customer closing the tab at the moment of payment.
func (s *Service) frontendURL(path, queryKey, queryValue string) string {
	u := s.appPublicURL + path
	if queryKey != "" {
		u += "?" + url.QueryEscape(queryKey) + "=" + url.QueryEscape(queryValue)
	}
	return u
}

// stripeErr logs an upstream Stripe failure and converts it to the single
// BILLING_PROVIDER_ERROR (502) code callers see.
//
// The error is logged, never returned to the client: a Stripe error message
// can quote request parameters back, and while the SDK does not put the API
// key in the error, "surface the upstream string verbatim" is how
// credential-adjacent detail leaks in general. Logged fields are individual
// scalars, not a struct — per CLAUDE.md, slog redaction matches leaf keys and
// cannot reach inside a wholesale-logged value.
func (s *Service) stripeErr(op string, err error, organizationID uuid.UUID) error {
	s.log.Error("stripe request failed",
		"op", op,
		"organization_id", organizationID.String(),
		"error", err,
	)
	return apperror.New(apperror.BillingProviderError)
}
