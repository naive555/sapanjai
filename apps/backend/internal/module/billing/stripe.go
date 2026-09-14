package billing

import (
	"context"
	"fmt"

	"github.com/stripe/stripe-go/v86"
)

// This file is the module's entire Stripe surface. Nothing else in
// internal/module/billing — and nothing anywhere else in the codebase —
// imports github.com/stripe/stripe-go, which is what makes the service
// testable without a network call and what keeps plan invariant 1 ("nothing
// in subscription.Service ever imports a Stripe type") a property of the
// import graph rather than a rule somebody has to remember.

// integrationIdentifier is sent on every Checkout Session so Stripe can
// attribute sessions to this integration in the Dashboard's payment-flow
// reporting. The 8-letter suffix is random but FIXED — generated once, at
// the time this code was written, and hardcoded. A per-request random value
// would defeat the entire point: the identifier exists so flows created by
// this integration are comparable to each other over time. Do not regenerate
// it, and do not make it configurable.
const integrationIdentifier = "sapanjai_billing_fepumlri"

// checkoutInput is everything Service.Checkout needs Stripe to know, in this
// module's own vocabulary. Note what is absent and must stay absent:
//
//   - PaymentMethodTypes. Omitting payment_method_types is what enables
//     dynamic payment methods configured from the Stripe Dashboard.
//     Hardcoding ["card"] silently removes PromptPay, which for Thai
//     customers is not a rounding error. There is deliberately no field
//     here to set it through.
//   - AutomaticTax. Plan decision 3: THB only, Stripe Tax OFF at launch,
//     Thai VAT handled through the selling entity's own registration.
//     automatic_tax calculates nothing AND errors nothing without an active
//     tax registration, so switching it on without one is strictly worse
//     than leaving it off — it looks handled and collects zero. Again, no
//     field to set it through.
type checkoutInput struct {
	CustomerID string
	PriceID    string
	SuccessURL string
	CancelURL  string

	// ClientReferenceID and Metadata both carry the organization id. The
	// webhook (step 7) resolves an incoming event back to an org, and it
	// gets three independent chances to do so: client_reference_id on the
	// session, metadata on the session, and metadata on the subscription
	// the session creates (SubscriptionMetadata below) — which is the only
	// one still attached to the object that customer.subscription.updated
	// and invoice.paid events reference months later.
	ClientReferenceID    string
	Metadata             map[string]string
	SubscriptionMetadata map[string]string
}

// customerInput is the lazily-created Stripe Customer for an organization
// (plan decision 4). IdempotencyKey is not decoration: see
// Service.ensureCustomer for why it is derived deterministically from the
// organization id rather than generated per attempt.
type customerInput struct {
	OrganizationID string
	Name           string
	Email          string
	IdempotencyKey string
}

// portalInput is a Customer Portal session. The portal is what buys upgrade,
// downgrade, cancel, invoice history, and payment-method management without
// building any of it here.
type portalInput struct {
	CustomerID string
	ReturnURL  string
}

// stripeAPI is the narrow seam every Stripe call goes through, following the
// connectorGetter/rateLimiter/limitEnforcer convention used elsewhere in
// this codebase: the production implementation is a thin adapter over the
// SDK, and tests hand-mock the interface so no test can reach the network
// even by accident.
type stripeAPI interface {
	// CreateCustomer returns the new Customer's id.
	CreateCustomer(ctx context.Context, in customerInput) (string, error)
	// CreateCheckoutSession returns the hosted Checkout URL to redirect to.
	CreateCheckoutSession(ctx context.Context, in checkoutInput) (string, error)
	// CreatePortalSession returns the hosted Customer Portal URL.
	CreatePortalSession(ctx context.Context, in portalInput) (string, error)
}

// stripeClient adapts *stripe.Client to stripeAPI.
//
// It holds an instantiated client rather than setting the deprecated global
// stripe.Key. The API version is not set here either: the SDK pins
// stripe.APIVersion ("2026-08-26.dahlia" in v86.4.0) on every request, so the
// version this code is written against travels with the dependency and a
// version bump is a go.mod change reviewed like any other.
type stripeClient struct {
	sc *stripe.Client
}

// NewStripeClient builds the production stripeAPI over apiKey. Returns nil
// when apiKey is empty — server.go passes that straight through to
// NewService, which mounts the routes but answers BILLING_NOT_CONFIGURED.
func NewStripeClient(apiKey string) stripeAPI {
	if apiKey == "" {
		return nil
	}
	return &stripeClient{sc: stripe.NewClient(apiKey)}
}

func (c *stripeClient) CreateCustomer(ctx context.Context, in customerInput) (string, error) {
	params := &stripe.CustomerCreateParams{
		Metadata: map[string]string{metadataOrganizationID: in.OrganizationID},
	}
	if in.Name != "" {
		params.Name = stripe.String(in.Name)
	}
	if in.Email != "" {
		params.Email = stripe.String(in.Email)
	}
	if in.IdempotencyKey != "" {
		params.SetIdempotencyKey(in.IdempotencyKey)
	}

	cus, err := c.sc.V1Customers.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe: create customer: %w", err)
	}
	return cus.ID, nil
}

func (c *stripeClient) CreateCheckoutSession(ctx context.Context, in checkoutInput) (string, error) {
	params := &stripe.CheckoutSessionCreateParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:   stripe.String(in.CustomerID),
		SuccessURL: stripe.String(in.SuccessURL),
		CancelURL:  stripe.String(in.CancelURL),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			// A Price id, never the deprecated `plan` object, and never an
			// inline PriceData: the Price exists in Stripe under the plan's
			// own Product, so every invoice line item reads that plan's
			// name (plan step 1's reason for plan_prices being a table).
			Price:    stripe.String(in.PriceID),
			Quantity: stripe.Int64(1),
		}},
		ClientReferenceID:     stripe.String(in.ClientReferenceID),
		Metadata:              in.Metadata,
		IntegrationIdentifier: stripe.String(integrationIdentifier),
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: in.SubscriptionMetadata,
		},
	}

	sess, err := c.sc.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe: create checkout session: %w", err)
	}
	return sess.URL, nil
}

func (c *stripeClient) CreatePortalSession(ctx context.Context, in portalInput) (string, error) {
	sess, err := c.sc.V1BillingPortalSessions.Create(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(in.CustomerID),
		ReturnURL: stripe.String(in.ReturnURL),
	})
	if err != nil {
		return "", fmt.Errorf("stripe: create portal session: %w", err)
	}
	return sess.URL, nil
}
