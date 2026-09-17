package billing

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// freePlanName is the plan a cancelled subscription falls back to. It is the
// name cmd/seed creates, and the name plans are addressed by everywhere else
// a plan has to be named rather than chosen (GetPlanByName). A deployment
// that has renamed or removed it gets an error log and NO plan change --
// leaving the org on what it had is strictly better than guessing, and far
// better than deleting its subscription row.
const freePlanName = "free"

// handledEventTypes is the exact set POST /billing/webhook acts on.
//
// Anything else -- and a Stripe account will happily send hundreds of other
// types -- is answered 200 and dropped without being recorded in
// stripe_events. 200 rather than 4xx because Stripe retries a non-2xx for
// days and eventually disables the endpoint, which would take the ones that
// DO matter down with it.
var handledEventTypes = map[string]struct{}{
	eventCheckoutSessionCompleted: {},
	eventSubscriptionCreated:      {},
	eventSubscriptionUpdated:      {},
	eventSubscriptionDeleted:      {},
	eventInvoicePaid:              {},
	eventInvoicePaymentFailed:     {},
}

// planAssigner is the subset of *subscription.Service the webhook depends
// on, following internal/module/admin's subscriptionResolver: a narrow
// interface declared by the consumer and injected from server.go, so the
// org_subscriptions upsert has exactly one implementation (plan invariant
// 5).
//
// AssignPlanTx rather than AssignPlan, because the whole reconciliation --
// the stripe_events claim included -- runs in one transaction. See
// Service.reconcile for why that matters and subscription.PlanWriter for
// why extending the seam was the right resolution rather than growing an
// upsert here.
type planAssigner interface {
	AssignPlanTx(ctx context.Context, w subscription.PlanWriter, organizationID, planID uuid.UUID) error
}

var _ planAssigner = (*subscription.Service)(nil)

// HandleWebhook verifies, deduplicates, and reconciles one Stripe webhook
// delivery. payload MUST be the exact bytes Stripe sent -- see the handler.
//
// Error surface, all of it observable by an unauthenticated caller and
// therefore deliberately uninformative:
//
//   - no webhook secret configured -> 501 BILLING_NOT_CONFIGURED
//   - bad/absent/stale signature   -> 400 WEBHOOK_SIGNATURE_INVALID, and
//     nothing whatsoever is written: verification happens before the first
//     database call, not after.
//   - anything else                -> the error propagates to a 500, which
//     is what makes Stripe retry.
func (s *Service) HandleWebhook(ctx context.Context, payload []byte, signature string) error {
	if s.webhooks == nil {
		return apperror.New(apperror.BillingNotConfigured)
	}

	event, err := s.webhooks.ConstructEvent(payload, signature)
	if err != nil {
		// The SDK's error (ErrNotSigned / ErrInvalidHeader /
		// ErrNoValidSignature / ErrTooOld, or a decode failure). Never the
		// payload, never the signature header, never the secret.
		s.log.Warn("stripe webhook rejected", "error", err)
		return apperror.New(apperror.WebhookSignatureInvalid)
	}

	return s.reconcile(ctx, event)
}

// reconcile claims the event and applies it, in ONE transaction.
//
// # Where the event is claimed, and why it is here
//
// stripe_events gives replay protection, but WHERE the claim happens decides
// what a mid-processing failure costs:
//
//   - Claim in its own transaction, then process: a failure afterwards
//     leaves the event permanently marked handled, so Stripe's retry finds
//     a duplicate id and does nothing. The state change is lost forever,
//     silently, with no error left anywhere to find it by.
//   - Process first, claim after: a crash in between reprocesses the event
//     on retry, which for a plan assignment is harmless but for anything
//     non-idempotent would not be.
//
// So both happen in one transaction (CLAUDE.md: "multi-step writes run in
// transactions"). A conflict on the claim means already-processed and the
// transaction commits having done nothing; a failure during processing
// rolls the claim back with it, so the retry Stripe was always going to
// send works exactly as designed.
//
// The collision that resolution creates with plan invariant 5 -- billing
// owns no upsert and must call subscription.Service.AssignPlan, which runs
// on the plain store and would open its OWN connection outside this
// transaction -- is resolved by extending the seam rather than duplicating
// it: subscription.Service.AssignPlanTx takes a writer, AssignPlan is that
// same method bound to the subscription service's own store, and this
// passes the tx-bound db.Querier. There is still exactly one
// org_subscriptions upsert in the codebase and it still lives in
// internal/module/subscription.
func (s *Service) reconcile(ctx context.Context, event webhookEvent) error {
	if _, ok := handledEventTypes[event.Type]; !ok {
		// Not an error and not recorded: nothing here will ever act on it,
		// so a row in stripe_events would be landfill.
		s.log.Debug("stripe webhook ignored: unhandled event type",
			"event_id", event.ID, "event_type", event.Type)
		return nil
	}

	var applied *appliedEvent
	err := s.store.WithTx(ctx, func(q db.Querier) error {
		if _, err := q.ClaimStripeEvent(ctx, db.ClaimStripeEventParams{ID: event.ID, Type: event.Type}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// ON CONFLICT DO NOTHING matched: this exact event has been
				// processed before. Commit having changed nothing.
				s.log.Info("stripe webhook replay ignored",
					"event_id", event.ID, "event_type", event.Type)
				return nil
			}
			return err
		}

		var applyErr error
		applied, applyErr = s.applyEvent(ctx, q, event)
		return applyErr
	})
	if err != nil {
		return err
	}

	// Audit AFTER the commit, on the pool rather than the transaction: a
	// best-effort write (CLAUDE.md) must not be able to roll back the
	// reconciliation it is describing, and taking a second connection while
	// holding the org_subscriptions row lock is exactly the shape that
	// deadlocks a saturated pool. There is no actor id -- Stripe is not a
	// user -- so the row carries the organization only.
	if applied != nil {
		metadata, _ := json.Marshal(map[string]string{
			"eventType": event.Type,
			"planId":    applied.PlanID,
			"status":    applied.Status,
		})
		orgID := applied.OrganizationID
		s.audit.Record(ctx, auditlog.ActionBillingSubscriptionSynced, nil, &orgID, metadata)
	}
	return nil
}

// appliedEvent is what a successful reconciliation wants to say afterwards.
// Deliberately no Stripe customer or subscription id: audit_logs is not a
// second billing record (plan invariant 1), and those ids are what the
// admin console's own views are for.
type appliedEvent struct {
	OrganizationID uuid.UUID
	PlanID         string
	Status         string
}

// applyEvent reconciles one verified, freshly-claimed event onto
// org_subscriptions, inside the caller's transaction.
//
// Returning (nil, nil) means "nothing to do, and retrying will not change
// that" -- an unmappable event, or one older than what is already stored.
// The caller commits, so the event stays claimed and a redelivery is a
// cheap no-op rather than another fruitless resolution attempt.
func (s *Service) applyEvent(ctx context.Context, q db.Querier, event webhookEvent) (*appliedEvent, error) {
	state := event.State

	organizationID, err := s.resolveOrganization(ctx, q, state)
	if err != nil {
		return nil, err
	}
	if organizationID == uuid.Nil {
		// Neither metadata nor any recorded Stripe id maps this to a
		// tenant. Retrying cannot fix that, so it is logged loudly and
		// dropped rather than retried forever.
		s.log.Error("stripe webhook could not be mapped to an organization",
			"event_id", event.ID, "event_type", event.Type)
		return nil, nil
	}

	current, err := q.GetOrgBillingSyncForUpdate(ctx, organizationID)
	hasRow := true
	switch {
	case err == nil:
		// Out-of-order protection (migration 00016). Stripe retries and can
		// deliver out of order, and stripe_events only stops an exact
		// replay -- a stale customer.subscription.updated is a different
		// event id and would otherwise regress the plan or the status.
		//
		// STRICTLY older is rejected; an equal timestamp is applied. Stripe
		// event timestamps are whole seconds, so a tie is genuinely
		// ambiguous and last-writer-wins is the only answer available
		// without a monotonic version Stripe does not publish.
		if current.StripeEventAt.Valid && event.Created.Before(current.StripeEventAt.Time) {
			s.log.Info("stripe webhook ignored as stale",
				"event_id", event.ID, "event_type", event.Type,
				"organization_id", organizationID.String())
			return nil, nil
		}
	case errors.Is(err, pgx.ErrNoRows):
		// No org_subscriptions row yet -- the normal state of an org that
		// has never been assigned a plan (billing never creates one; see
		// Service.ensureCustomer). AssignPlanTx below creates it.
		hasRow = false
	default:
		return nil, err
	}

	planID, err := s.resolvePlan(ctx, q, state)
	if err != nil {
		return nil, err
	}
	if planID != uuid.Nil {
		// The ONE entitlement write, and it goes through the subscription
		// service's own upsert -- whose ON CONFLICT sets plan_id and
		// updated_at and deliberately does not touch custom_limits, so an
		// admin override survives a billing event (plan invariant 2).
		if err := s.subs.AssignPlanTx(ctx, q, organizationID, planID); err != nil {
			return nil, err
		}
		hasRow = true
	}
	if !hasRow {
		// Nothing to update and no plan to create a row with (an invoice
		// event for an org whose subscription.created never arrived, or a
		// Price missing from the catalogue). Recording Stripe columns
		// would mean inventing an entitlement row, which is exactly what
		// invariant 5 reserves for AssignPlan.
		s.log.Error("stripe webhook has no plan and no subscription row to update",
			"event_id", event.ID, "event_type", event.Type,
			"organization_id", organizationID.String())
		return nil, nil
	}

	if err := q.UpdateOrgStripeSubscription(ctx, s.updateParams(organizationID, event)); err != nil {
		return nil, err
	}

	return &appliedEvent{
		OrganizationID: organizationID,
		PlanID:         uuidOrEmpty(planID),
		Status:         state.Status,
	}, nil
}

// updateParams maps the event onto the Stripe-linkage columns.
//
// Every optional field left nil means "leave the stored value alone"
// (UpdateOrgStripeSubscription COALESCEs each one), because the handled
// event types carry different subsets and an absent field must never erase
// what an earlier event learned.
func (s *Service) updateParams(organizationID uuid.UUID, event webhookEvent) db.UpdateOrgStripeSubscriptionParams {
	state := event.State
	params := db.UpdateOrgStripeSubscriptionParams{
		OrganizationID: organizationID,
		// The watermark this event establishes. Written on every applied
		// event, including one that only refreshed a status, so a later
		// straggler is measured against the newest thing actually applied.
		StripeEventAt:     event.Created,
		ClearSubscription: state.Cancelled,
	}

	if state.CustomerID != "" {
		customerID := state.CustomerID
		params.StripeCustomerID = &customerID
	}

	if state.Cancelled {
		// Plan decision 4: stripe_subscription_id IS NULL is the canonical
		// "not paying" signal, so a cancellation clears the column (via
		// ClearSubscription above) instead of leaving it pointing at a dead
		// Stripe object. The Customer is kept -- it is reused if they come
		// back. cancel_at_period_end goes false because there is no longer
		// a period to cancel at; leaving it true would read as "cancelling"
		// forever next to a NULL subscription.
		status, cancelAtPeriodEnd := statusCanceled, false
		params.Status = &status
		params.CancelAtPeriodEnd = &cancelAtPeriodEnd
		return params
	}

	if state.SubscriptionID != "" {
		subscriptionID := state.SubscriptionID
		params.StripeSubscriptionID = &subscriptionID
	}
	if state.Status != "" {
		status := state.Status
		params.Status = &status
	}
	if state.CurrentPeriodEnd != nil {
		params.CurrentPeriodEnd = pgtype.Timestamp{Time: *state.CurrentPeriodEnd, Valid: true}
	}
	if state.CancelAtPeriodEnd != nil {
		params.CancelAtPeriodEnd = state.CancelAtPeriodEnd
	}
	return params
}

// resolveOrganization maps an event back to a tenant, database first.
//
// Three sources, and the order is the tenant-isolation property:
//
//  1. org_subscriptions.stripe_subscription_id
//  2. org_subscriptions.stripe_customer_id
//  3. the organization_id this integration wrote into Stripe metadata (and
//     into the Session's client_reference_id) at checkout.
//
// 1 and 2 are backed by the partial unique indexes from migration 00013, so
// each resolves to exactly one organization and an event carrying org A's
// identifiers cannot land on org B's row no matter what its metadata says.
// That is why they come first: metadata is editable in the Stripe dashboard
// and is a snapshot, while the recorded linkage is this system's own,
// uniqueness-enforced mapping.
//
// 3 is not redundant -- it is the ONLY source that works for the first
// event of a new subscription, when nothing has been recorded yet (an org
// with no org_subscriptions row has nowhere to have stored a customer id;
// see Service.ensureCustomer). It is also the copy that stays attached to
// the Subscription object months later, which is why Service.Checkout
// writes it to the subscription as well as the session.
//
// A disagreement between 3 and the recorded linkage is logged and the
// database wins.
func (s *Service) resolveOrganization(ctx context.Context, q db.Querier, state subscriptionState) (uuid.UUID, error) {
	var recorded uuid.UUID

	if state.SubscriptionID != "" {
		id, err := q.FindOrgByStripeSubscriptionID(ctx, state.SubscriptionID)
		switch {
		case err == nil:
			recorded = id
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return uuid.Nil, err
		}
	}
	if recorded == uuid.Nil && state.CustomerID != "" {
		id, err := q.FindOrgByStripeCustomerID(ctx, state.CustomerID)
		switch {
		case err == nil:
			recorded = id
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return uuid.Nil, err
		}
	}

	var fromMetadata uuid.UUID
	if state.OrganizationID != "" {
		if id, err := uuid.Parse(state.OrganizationID); err == nil {
			fromMetadata = id
		}
	}

	if recorded != uuid.Nil {
		if fromMetadata != uuid.Nil && fromMetadata != recorded {
			s.log.Warn("stripe webhook metadata names a different organization than the recorded Stripe linkage; using the recorded one",
				"recorded_organization_id", recorded.String(),
				"metadata_organization_id", fromMetadata.String())
		}
		return recorded, nil
	}
	return fromMetadata, nil
}

// resolvePlan decides which entitlement plan the event puts the org on, or
// uuid.Nil for "do not touch plan_id".
//
// Price id first, metadata second, and that order is load-bearing: the
// Customer Portal lets a customer switch plans with this application not
// involved at all, which changes the Price on the subscription while
// leaving the plan_id stamped into metadata at checkout frozen at whatever
// they originally bought. Trusting metadata there bills them for the new
// plan and entitles them to the old one.
//
// Invoice events resolve NOTHING here by construction -- invoiceState
// deliberately does not carry a plan id, and an invoice has no Price of its
// own -- so invoice.paid can refresh a status and a period without ever
// being able to walk a Portal-driven plan change backwards.
func (s *Service) resolvePlan(ctx context.Context, q db.Querier, state subscriptionState) (uuid.UUID, error) {
	if state.Cancelled {
		plan, err := q.GetPlanByName(ctx, freePlanName)
		switch {
		case err == nil:
			return plan.ID, nil
		case errors.Is(err, pgx.ErrNoRows):
			s.log.Error("cancelled subscription cannot be downgraded: no plan named " + freePlanName)
			return uuid.Nil, nil
		default:
			return uuid.Nil, err
		}
	}

	if state.PriceID != "" {
		plan, err := q.GetPlanByStripePriceID(ctx, state.PriceID)
		switch {
		case err == nil:
			return plan.ID, nil
		case errors.Is(err, pgx.ErrNoRows):
			// A Price that exists in Stripe but not in plan_prices. Worth
			// shouting about -- somebody is being charged for something
			// this catalogue cannot name -- but the metadata fallback below
			// may still land it on the right plan.
			s.log.Error("stripe price is not in the plan_prices catalogue",
				"stripe_price_id", state.PriceID)
		default:
			return uuid.Nil, err
		}
	}

	if state.PlanID == "" {
		return uuid.Nil, nil
	}
	planID, err := uuid.Parse(state.PlanID)
	if err != nil {
		s.log.Error("stripe metadata plan_id is not a uuid")
		return uuid.Nil, nil
	}
	// Confirmed against the catalogue before it is assigned: metadata is a
	// string somebody could have edited in the Stripe dashboard, and
	// UpsertOrgSubscription would otherwise fail on the foreign key in a
	// way that reads as an internal error.
	switch _, err := q.GetPlanByID(ctx, planID); {
	case err == nil:
		return planID, nil
	case errors.Is(err, pgx.ErrNoRows):
		s.log.Error("stripe metadata plan_id names no plan", "plan_id", planID.String())
		return uuid.Nil, nil
	default:
		return uuid.Nil, err
	}
}

func uuidOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
