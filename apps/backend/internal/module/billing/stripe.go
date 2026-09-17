package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
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

// ---- Webhook (step 7) ----
//
// Everything below maps Stripe's event payloads into this module's own
// plain Go vocabulary before anything else sees them. That mapping is the
// mechanical enforcement of plan invariant 1: subscription.Service — the
// single answer to "what may this org do" — is handed uuids, strings, and
// times, and could not import a Stripe type if it wanted to.

// StripeAPIVersion is the Stripe API version this build's SDK pins. An
// incoming webhook event must declare a version on the same release train
// or webhook.ConstructEvent rejects it — so this is the value a Stripe
// dashboard webhook endpoint has to be configured with, and the value to
// re-check against the dashboard whenever the SDK is bumped in go.mod.
//
// Exported for two reasons: it is genuine deployment information (step 10's
// docs need it), and it lets the webhook tests — in this package and in
// internal/server — build a payload this code will accept without importing
// stripe-go, keeping stripe.go the single file in the repo that does.
const StripeAPIVersion = stripe.APIVersion

// Event types POST /billing/webhook acts on. Anything else is answered 200
// and ignored without being recorded in stripe_events: a Stripe account
// sending us the full firehose must not fill that table with rows for
// events nothing will ever reconcile, and 200 (rather than 4xx) stops
// Stripe retrying and eventually disabling the endpoint.
const (
	eventCheckoutSessionCompleted = "checkout.session.completed"
	eventSubscriptionCreated      = "customer.subscription.created"
	eventSubscriptionUpdated      = "customer.subscription.updated"
	eventSubscriptionDeleted      = "customer.subscription.deleted"
	eventInvoicePaid              = "invoice.paid"
	eventInvoicePaymentFailed     = "invoice.payment_failed"
)

// subscriptionStatus values this module writes to org_subscriptions.status.
// They are Stripe's own spellings, stored verbatim for a staff member
// reading the column against the Stripe dashboard — but as plain strings,
// never stripe.SubscriptionStatus, so the column's type does not follow the
// SDK's.
const (
	statusPastDue  = "past_due"
	statusCanceled = "canceled"
)

// subscriptionState is one Stripe event flattened into the fields the
// reconciler needs. Every field is optional in the sense that different
// event types carry different subsets — invoice.payment_failed knows the
// status but not the period end, checkout.session.completed knows the
// ids but not the authoritative status — and an absent field means "leave
// whatever is stored alone", never "clear it". See
// UpdateOrgStripeSubscription's COALESCEs.
type subscriptionState struct {
	// OrganizationID and PlanID are the metadata copies this integration
	// planted at checkout (Service.Checkout). Both are untrusted-ish
	// strings here — they are parsed as uuids by the reconciler, and the
	// database's own customer/subscription → org mapping wins over
	// OrganizationID whenever both resolve (Service.resolveOrganization).
	OrganizationID string
	PlanID         string

	CustomerID     string
	SubscriptionID string
	Status         string

	// PriceID is the Price the subscription's first line item charges. It
	// is the PRIMARY plan resolution (GetPlanByStripePriceID) because a
	// Customer Portal plan switch changes it while leaving PlanID frozen
	// at whatever was bought originally.
	PriceID string

	CurrentPeriodEnd  *time.Time
	CancelAtPeriodEnd *bool

	// Cancelled marks customer.subscription.deleted, the one event that
	// clears stripe_subscription_id rather than repointing it (plan
	// decision 4: NULL is the canonical "not paying" signal).
	Cancelled bool
}

// webhookEvent is a verified Stripe event, reduced to what the reconciler
// uses. Created is the event's own Stripe-clock timestamp and is the
// out-of-order watermark (migration 00016).
type webhookEvent struct {
	ID      string
	Type    string
	Created time.Time
	State   subscriptionState
}

// stripeWebhooks verifies and decodes an incoming webhook body. Separate
// from stripeAPI because it is a different kind of dependency — a signing
// secret and a pure function, no network — and because the API key and the
// webhook secret are independently configurable: a deployment can have one
// without the other, and each degrades on its own.
type stripeWebhooks interface {
	// ConstructEvent verifies signature against payload and returns the
	// decoded event. payload MUST be the exact bytes Stripe sent.
	ConstructEvent(payload []byte, signature string) (webhookEvent, error)
}

// stripeWebhookVerifier is the production stripeWebhooks over the SDK's
// webhook helper, which does the HMAC-SHA256 comparison in constant time
// and enforces the 5-minute default timestamp tolerance (so a captured body
// and header cannot be replayed at leisure).
type stripeWebhookVerifier struct {
	secret string
}

// NewStripeWebhooks builds the production stripeWebhooks over secret.
// Returns nil when secret is empty — server.go passes that straight to
// NewService, which mounts POST /billing/webhook anyway and answers
// BILLING_NOT_CONFIGURED, exactly as the two guarded routes do without
// STRIPE_SECRET_KEY.
func NewStripeWebhooks(secret string) stripeWebhooks {
	if secret == "" {
		return nil
	}
	return &stripeWebhookVerifier{secret: secret}
}

func (v *stripeWebhookVerifier) ConstructEvent(payload []byte, signature string) (webhookEvent, error) {
	// webhook.ConstructEvent, not ValidatePayload + our own json.Unmarshal:
	// it is the SDK's constant-time comparison, its tolerance check, and
	// its thin-event-notification guard in one call.
	//
	// IgnoreAPIVersionMismatch is deliberately NOT set. A Stripe webhook
	// endpoint configured against an API version from a different release
	// train sends object shapes this SDK may deserialize wrongly — most of
	// the fields read below moved at least once (current_period_end from
	// the Subscription to its items; the Invoice's subscription link into
	// invoice.parent). Failing loudly at the first delivery is far better
	// than silently reconciling zero values into org_subscriptions.
	ev, err := webhook.ConstructEvent(payload, signature, v.secret)
	if err != nil {
		// Returned as-is for the caller to log. It never contains the
		// secret or the payload: the SDK's errors are ErrNotSigned /
		// ErrInvalidHeader / ErrNoValidSignature / ErrTooOld, or a JSON
		// parse error.
		return webhookEvent{}, err
	}

	out := webhookEvent{ID: ev.ID, Type: string(ev.Type), Created: time.Unix(ev.Created, 0).UTC()}

	var raw json.RawMessage
	if ev.Data != nil {
		raw = ev.Data.Raw
	}

	switch out.Type {
	case eventCheckoutSessionCompleted:
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(raw, &sess); err != nil {
			return webhookEvent{}, fmt.Errorf("stripe: decode checkout session: %w", err)
		}
		out.State = checkoutSessionState(&sess)

	case eventSubscriptionCreated, eventSubscriptionUpdated, eventSubscriptionDeleted:
		var sub stripe.Subscription
		if err := json.Unmarshal(raw, &sub); err != nil {
			return webhookEvent{}, fmt.Errorf("stripe: decode subscription: %w", err)
		}
		out.State = subscriptionObjectState(&sub)
		out.State.Cancelled = out.Type == eventSubscriptionDeleted

	case eventInvoicePaid, eventInvoicePaymentFailed:
		var inv stripe.Invoice
		if err := json.Unmarshal(raw, &inv); err != nil {
			return webhookEvent{}, fmt.Errorf("stripe: decode invoice: %w", err)
		}
		out.State = invoiceState(&inv, out.Type == eventInvoicePaid)
	}

	return out, nil
}

// checkoutSessionState flattens a completed Checkout Session.
//
// It deliberately sets no Status: a Session says the customer finished
// paying, not what state the Subscription ended up in (trialing, active, or
// incomplete if the first charge is still settling). The
// customer.subscription.created event that arrives alongside it is the
// authority on that, and the COALESCE in UpdateOrgStripeSubscription is
// what lets this event contribute only the ids it actually knows.
func checkoutSessionState(sess *stripe.CheckoutSession) subscriptionState {
	st := subscriptionState{
		// client_reference_id first, session metadata second: both were
		// written by Service.Checkout, but client_reference_id is a
		// first-class Session field that the Stripe dashboard surfaces and
		// that nothing else in this integration writes.
		OrganizationID: firstNonEmpty(sess.ClientReferenceID, sess.Metadata[metadataOrganizationID]),
		PlanID:         sess.Metadata[metadataPlanID],
	}
	if sess.Customer != nil {
		st.CustomerID = sess.Customer.ID
	}
	if sess.Subscription != nil {
		// In a webhook payload `subscription` is an id string, which the
		// SDK unmarshals into a Subscription carrying only ID. Reading
		// anything else off it here would silently be a zero value.
		st.SubscriptionID = sess.Subscription.ID
	}
	return st
}

// subscriptionObjectState flattens a customer.subscription.* payload, the
// authoritative view of what the customer is paying for.
func subscriptionObjectState(sub *stripe.Subscription) subscriptionState {
	cancelAtPeriodEnd := sub.CancelAtPeriodEnd
	st := subscriptionState{
		OrganizationID:    sub.Metadata[metadataOrganizationID],
		PlanID:            sub.Metadata[metadataPlanID],
		SubscriptionID:    sub.ID,
		Status:            string(sub.Status),
		CancelAtPeriodEnd: &cancelAtPeriodEnd,
	}
	if sub.Customer != nil {
		st.CustomerID = sub.Customer.ID
	}
	// current_period_end lives on the subscription ITEM in this API version
	// (it moved off the Subscription object), and the Price id is on the
	// same item. One line item per subscription here — Service.Checkout
	// sends exactly one, quantity 1 — so the first item is the whole story.
	if sub.Items != nil && len(sub.Items.Data) > 0 {
		item := sub.Items.Data[0]
		if item.Price != nil {
			st.PriceID = item.Price.ID
		}
		if item.CurrentPeriodEnd > 0 {
			end := time.Unix(item.CurrentPeriodEnd, 0).UTC()
			st.CurrentPeriodEnd = &end
		}
	}
	return st
}

// invoiceState flattens invoice.paid / invoice.payment_failed.
//
// An invoice carries no subscription status of its own, so this synthesizes
// the one thing the pair unambiguously means: a failed payment puts the
// org in past_due immediately, rather than waiting for the
// customer.subscription.updated that follows. A successful payment
// contributes NO status — "paid" does not imply "active" (a paid invoice on
// a subscription cancelling at period end is still cancelling), so it lets
// the subscription events own that column and only refreshes the period.
func invoiceState(inv *stripe.Invoice, paid bool) subscriptionState {
	st := subscriptionState{}
	if !paid {
		st.Status = statusPastDue
	}
	if inv.Customer != nil {
		st.CustomerID = inv.Customer.ID
	}
	// The subscription link moved under invoice.parent in this API version;
	// there is no top-level invoice.subscription any more.
	if inv.Parent != nil && inv.Parent.SubscriptionDetails != nil {
		if sub := inv.Parent.SubscriptionDetails.Subscription; sub != nil {
			st.SubscriptionID = sub.ID
		}
		// The metadata snapshot frozen onto the invoice at finalization —
		// the org id copy that survives even when the Subscription object
		// itself is no longer fetched.
		//
		// The ORG id only. PlanID is deliberately NOT read here even though
		// the snapshot carries it: that snapshot is frozen, so a customer
		// who changed plan through the Customer Portal has invoices whose
		// metadata still names the plan they originally bought. Feeding
		// that to resolvePlan would walk the change backwards on the next
		// renewal. An invoice has no Price of its own either, so leaving
		// this empty is what makes "an invoice never moves plan_id" a
		// property of the mapping rather than a rule in the reconciler.
		st.OrganizationID = inv.Parent.SubscriptionDetails.Metadata[metadataOrganizationID]
	}
	return st
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
