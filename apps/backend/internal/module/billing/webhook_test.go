package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// No test in this file reaches the network. The signature is computed here
// from Stripe's documented v1 scheme (HMAC-SHA256 over "<timestamp>.<body>")
// rather than by calling the SDK helper the production code calls, so the
// two derivations have to agree independently; everything else is an
// in-memory fake behind the module's own narrow seams.

const testWebhookSecret = "whsec_test_dGhpc2lzbm90YXJlYWxzZWNyZXQ"

// ---- in-memory database ----

// fakeSubRow is one org_subscriptions row.
type fakeSubRow struct {
	PlanID               uuid.UUID
	CustomLimits         json.RawMessage
	StripeCustomerID     *string
	StripeSubscriptionID *string
	Status               *string
	CurrentPeriodEnd     pgtype.Timestamp
	CancelAtPeriodEnd    bool
	StripeEventAt        pgtype.Timestamp
}

func (r fakeSubRow) clone() fakeSubRow { return r }

// equal compares two rows field by field. fakeSubRow carries a
// json.RawMessage (custom_limits), so == is not available on it.
func (r fakeSubRow) equal(o fakeSubRow) bool {
	return r.PlanID == o.PlanID &&
		string(r.CustomLimits) == string(o.CustomLimits) &&
		str(r.StripeCustomerID) == str(o.StripeCustomerID) &&
		str(r.StripeSubscriptionID) == str(o.StripeSubscriptionID) &&
		str(r.Status) == str(o.Status) &&
		r.CurrentPeriodEnd == o.CurrentPeriodEnd &&
		r.CancelAtPeriodEnd == o.CancelAtPeriodEnd &&
		r.StripeEventAt == o.StripeEventAt
}

// fakeDB is a tiny in-memory stand-in for the tables the webhook touches.
// It embeds db.Querier (nil) so only the handful of methods the reconciler
// actually calls need implementing — anything else panics, which is the
// point: an unnoticed new query shows up as a loud nil-interface panic
// rather than as a silently untested code path.
type fakeDB struct {
	db.Querier

	events      map[string]string
	subs        map[uuid.UUID]fakeSubRow
	plans       map[uuid.UUID]db.Plan
	plansByName map[string]db.Plan
	priceToPlan map[string]uuid.UUID

	// upsertErr, when set, makes UpsertOrgSubscription fail — the
	// mid-processing failure the claim-inside-the-transaction design exists
	// to survive.
	upsertErr error

	claims  int
	upserts int
	updates int
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		events:      map[string]string{},
		subs:        map[uuid.UUID]fakeSubRow{},
		plans:       map[uuid.UUID]db.Plan{},
		plansByName: map[string]db.Plan{},
		priceToPlan: map[string]uuid.UUID{},
	}
}

// addPlan registers a plan, optionally reachable by a Stripe Price id.
func (f *fakeDB) addPlan(name, priceID string) db.Plan {
	plan := db.Plan{ID: uuid.New(), Name: name, IsPublic: true, Limits: json.RawMessage(`{"max_members":5}`)}
	f.plans[plan.ID] = plan
	f.plansByName[name] = plan
	if priceID != "" {
		f.priceToPlan[priceID] = plan.ID
	}
	return plan
}

func (f *fakeDB) snapshot() *fakeDB {
	cp := *f
	cp.events = map[string]string{}
	for k, v := range f.events {
		cp.events[k] = v
	}
	cp.subs = map[uuid.UUID]fakeSubRow{}
	for k, v := range f.subs {
		cp.subs[k] = v.clone()
	}
	return &cp
}

// restore rolls the mutable state back to a snapshot, so the fake WithTx
// below has real rollback semantics rather than merely claiming to.
func (f *fakeDB) restore(s *fakeDB) {
	f.events = s.events
	f.subs = s.subs
}

func (f *fakeDB) ClaimStripeEvent(_ context.Context, arg db.ClaimStripeEventParams) (string, error) {
	f.claims++
	if _, ok := f.events[arg.ID]; ok {
		return "", pgx.ErrNoRows // ON CONFLICT DO NOTHING ... RETURNING
	}
	f.events[arg.ID] = arg.Type
	return arg.ID, nil
}

func (f *fakeDB) GetOrgBillingSyncForUpdate(_ context.Context, organizationID uuid.UUID) (db.GetOrgBillingSyncForUpdateRow, error) {
	row, ok := f.subs[organizationID]
	if !ok {
		return db.GetOrgBillingSyncForUpdateRow{}, pgx.ErrNoRows
	}
	return db.GetOrgBillingSyncForUpdateRow{
		OrganizationID:       organizationID,
		PlanID:               row.PlanID,
		StripeCustomerID:     row.StripeCustomerID,
		StripeSubscriptionID: row.StripeSubscriptionID,
		Status:               row.Status,
		CurrentPeriodEnd:     row.CurrentPeriodEnd,
		CancelAtPeriodEnd:    row.CancelAtPeriodEnd,
		StripeEventAt:        row.StripeEventAt,
	}, nil
}

func (f *fakeDB) FindOrgByStripeCustomerID(_ context.Context, customerID string) (uuid.UUID, error) {
	for orgID, row := range f.subs {
		if row.StripeCustomerID != nil && *row.StripeCustomerID == customerID {
			return orgID, nil
		}
	}
	return uuid.Nil, pgx.ErrNoRows
}

func (f *fakeDB) FindOrgByStripeSubscriptionID(_ context.Context, subscriptionID string) (uuid.UUID, error) {
	for orgID, row := range f.subs {
		if row.StripeSubscriptionID != nil && *row.StripeSubscriptionID == subscriptionID {
			return orgID, nil
		}
	}
	return uuid.Nil, pgx.ErrNoRows
}

func (f *fakeDB) GetPlanByID(_ context.Context, id uuid.UUID) (db.Plan, error) {
	if plan, ok := f.plans[id]; ok {
		return plan, nil
	}
	return db.Plan{}, pgx.ErrNoRows
}

func (f *fakeDB) GetPlanByName(_ context.Context, name string) (db.Plan, error) {
	if plan, ok := f.plansByName[name]; ok {
		return plan, nil
	}
	return db.Plan{}, pgx.ErrNoRows
}

func (f *fakeDB) GetPlanByStripePriceID(_ context.Context, priceID string) (db.Plan, error) {
	if planID, ok := f.priceToPlan[priceID]; ok {
		return f.plans[planID], nil
	}
	return db.Plan{}, pgx.ErrNoRows
}

// UpsertOrgSubscription mirrors the real ON CONFLICT: it sets plan_id and
// deliberately leaves custom_limits alone. That is the mechanism plan
// invariant 2 rests on, so the fake has to reproduce it or the
// custom-limits test would be testing the fake.
func (f *fakeDB) UpsertOrgSubscription(_ context.Context, arg db.UpsertOrgSubscriptionParams) error {
	f.upserts++
	if f.upsertErr != nil {
		return f.upsertErr
	}
	row := f.subs[arg.OrganizationID]
	row.PlanID = arg.PlanID
	f.subs[arg.OrganizationID] = row
	return nil
}

// UpdateOrgStripeSubscription mirrors the real query's COALESCE semantics:
// a nil parameter means "leave the stored value alone".
func (f *fakeDB) UpdateOrgStripeSubscription(_ context.Context, arg db.UpdateOrgStripeSubscriptionParams) error {
	f.updates++
	row, ok := f.subs[arg.OrganizationID]
	if !ok {
		return nil // the real UPDATE matches no row
	}
	if arg.StripeCustomerID != nil {
		row.StripeCustomerID = arg.StripeCustomerID
	}
	switch {
	case arg.ClearSubscription:
		row.StripeSubscriptionID = nil
	case arg.StripeSubscriptionID != nil:
		row.StripeSubscriptionID = arg.StripeSubscriptionID
	}
	if arg.Status != nil {
		row.Status = arg.Status
	}
	if arg.CurrentPeriodEnd.Valid {
		row.CurrentPeriodEnd = arg.CurrentPeriodEnd
	}
	if arg.CancelAtPeriodEnd != nil {
		row.CancelAtPeriodEnd = *arg.CancelAtPeriodEnd
	}
	row.StripeEventAt = pgtype.Timestamp{Time: arg.StripeEventAt, Valid: true}
	f.subs[arg.OrganizationID] = row
	return nil
}

// ---- service under test ----

// newWebhookService wires the real Service over the fake database, the real
// signature verifier (so the signing path is exercised for real), and the
// real subscription.Service — the last one deliberately, since the whole
// point of plan invariant 5 is that the upsert is that service's and not
// this one's.
func newWebhookService(f *fakeDB, secret string) *Service {
	store := &mockBillingStore{
		withTx: func(ctx context.Context, fn func(q db.Querier) error) error {
			before := f.snapshot()
			if err := fn(f); err != nil {
				f.restore(before) // ROLLBACK
				return err
			}
			return nil
		},
	}
	return NewService(store, nil, NewStripeWebhooks(secret), subscription.NewService(fakeSubStore{f}),
		newTestAudit(), testPublicURL, newTestLog())
}

// fakeSubStore satisfies the unexported subStore that subscription.NewService
// takes. Only UpsertOrgSubscription is ever reached from the webhook path.
type fakeSubStore struct{ *fakeDB }

func (fakeSubStore) GetOrgSubscriptionWithPlan(context.Context, uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
	return db.GetOrgSubscriptionWithPlanRow{}, pgx.ErrNoRows
}
func (fakeSubStore) GetOrgSubscription(context.Context, uuid.UUID) (db.GetOrgSubscriptionRow, error) {
	return db.GetOrgSubscriptionRow{}, pgx.ErrNoRows
}
func (fakeSubStore) ListPlans(context.Context) ([]db.Plan, error) { return nil, nil }

// ---- payload + signature helpers ----

// signedPayload returns the exact bytes and the Stripe-Signature header for
// them, computed from Stripe's documented scheme rather than the SDK helper
// the production code uses.
func signedPayload(t *testing.T, payload []byte, secret string, at time.Time) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", at.Unix())
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

type eventOpts struct {
	id        string
	eventType string
	created   time.Time
	object    string // raw JSON of data.object
}

func eventPayload(t *testing.T, o eventOpts) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(
		`{"id":%q,"object":"event","api_version":%q,"created":%d,"type":%q,"data":{"object":%s}}`,
		o.id, StripeAPIVersion, o.created.Unix(), o.eventType, o.object))
}

// subscriptionObject renders a customer.subscription.* data.object.
// current_period_end and price live on the subscription ITEM in this API
// version, which is exactly the shape subscriptionObjectState reads.
func subscriptionObject(subID, customerID, status, priceID string, orgID, planID string, periodEnd int64, cancelAtPeriodEnd bool) string {
	metadata := "{}"
	if orgID != "" || planID != "" {
		metadata = fmt.Sprintf(`{"organization_id":%q,"plan_id":%q}`, orgID, planID)
	}
	return fmt.Sprintf(`{
		"id":%q,"object":"subscription","status":%q,"cancel_at_period_end":%t,
		"customer":%q,"metadata":%s,
		"items":{"object":"list","data":[
			{"id":"si_1","object":"subscription_item","current_period_end":%d,
			 "price":{"id":%q,"object":"price"}}]}
	}`, subID, status, cancelAtPeriodEnd, customerID, metadata, periodEnd, priceID)
}

func checkoutSessionObject(sessionID, customerID, subID, orgID, planID string) string {
	return fmt.Sprintf(`{
		"id":%q,"object":"checkout.session","mode":"subscription","status":"complete",
		"payment_status":"paid","client_reference_id":%q,"customer":%q,"subscription":%q,
		"metadata":{"organization_id":%q,"plan_id":%q}
	}`, sessionID, orgID, customerID, subID, orgID, planID)
}

func invoiceObject(invoiceID, customerID, subID, orgID, planID string) string {
	return fmt.Sprintf(`{
		"id":%q,"object":"invoice","customer":%q,
		"parent":{"type":"subscription_details","subscription_details":{
			"subscription":%q,"metadata":{"organization_id":%q,"plan_id":%q}}}
	}`, invoiceID, customerID, subID, orgID, planID)
}

// deliver signs payload and runs it through the service, as the handler
// would.
func deliver(t *testing.T, svc *Service, payload []byte) error {
	t.Helper()
	return svc.HandleWebhook(context.Background(), payload,
		signedPayload(t, payload, testWebhookSecret, time.Now()))
}

// seedPayingOrg puts an org on plan with a live Stripe subscription
// recorded, as a completed checkout would have.
func seedPayingOrg(f *fakeDB, orgID uuid.UUID, planID uuid.UUID, customerID, subID, status string, eventAt time.Time) {
	c, s, st := customerID, subID, status
	f.subs[orgID] = fakeSubRow{
		PlanID:               planID,
		StripeCustomerID:     &c,
		StripeSubscriptionID: &s,
		Status:               &st,
		StripeEventAt:        pgtype.Timestamp{Time: eventAt, Valid: true},
	}
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// ---- tests ----

// TestWebhook_BadSignatureIs400AndNothingIsWritten is the trap-1 test.
//
// Verification happens before the first database call, so a forged or
// mistyped signature cannot leave a stripe_events row, an org_subscriptions
// change, or anything else behind — "no state change whatsoever" is
// asserted here by the fake's own counters, not inferred.
func TestWebhook_BadSignatureIs400AndNothingIsWritten(t *testing.T) {
	f := newFakeDB()
	plan := f.addPlan("pro", "price_pro")
	orgID := uuid.New()
	seedPayingOrg(f, orgID, plan.ID, "cus_1", "sub_1", "active", time.Now().Add(-time.Hour))
	before := f.subs[orgID]

	svc := newWebhookService(f, testWebhookSecret)
	payload := eventPayload(t, eventOpts{
		id: "evt_forged", eventType: eventSubscriptionUpdated, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", statusPastDue, "price_pro", orgID.String(), plan.ID.String(), 0, false),
	})

	for name, signature := range map[string]string{
		"absent":              "",
		"garbage":             "t=1,v1=deadbeef",
		"malformed":           "not-a-signature-header",
		"signed with another": signedPayload(t, payload, "whsec_someone_elses_secret", time.Now()),
		"valid but stale":     signedPayload(t, payload, testWebhookSecret, time.Now().Add(-time.Hour)),
		"valid for a different body": signedPayload(t,
			eventPayload(t, eventOpts{id: "evt_other", eventType: eventSubscriptionUpdated, created: time.Now(), object: "{}"}),
			testWebhookSecret, time.Now()),
	} {
		t.Run(name, func(t *testing.T) {
			err := svc.HandleWebhook(context.Background(), payload, signature)
			if code := appErrorCode(t, err); code != apperror.WebhookSignatureInvalid {
				t.Fatalf("code = %s, want %s", code, apperror.WebhookSignatureInvalid)
			}
			if status, _ := apperror.Resolve(apperror.WebhookSignatureInvalid); status != http.StatusBadRequest {
				t.Fatalf("WEBHOOK_SIGNATURE_INVALID resolves to %d, want 400", status)
			}
			if f.claims != 0 || f.upserts != 0 || f.updates != 0 {
				t.Fatalf("a rejected delivery touched the database: claims=%d upserts=%d updates=%d",
					f.claims, f.upserts, f.updates)
			}
			if len(f.events) != 0 {
				t.Fatalf("a rejected delivery recorded %d stripe_events rows", len(f.events))
			}
			if got := f.subs[orgID]; !got.equal(before) {
				t.Fatalf("org_subscriptions changed: %+v, want %+v", got, before)
			}
		})
	}
}

// TestWebhook_OneByteBodyChangeFailsVerification is the concrete form of
// the Echo raw-body trap: the HMAC is over the EXACT bytes, so anything
// that re-serializes, re-orders, or re-supplies the body (c.Bind, a
// body-reading middleware, a helpful test harness) produces a payload whose
// signature no longer matches.
func TestWebhook_OneByteBodyChangeFailsVerification(t *testing.T) {
	f := newFakeDB()
	svc := newWebhookService(f, testWebhookSecret)

	payload := eventPayload(t, eventOpts{
		id: "evt_raw", eventType: eventSubscriptionUpdated, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", "active", "price_pro", uuid.NewString(), uuid.NewString(), 0, false),
	})
	signature := signedPayload(t, payload, testWebhookSecret, time.Now())

	// Re-marshalling the identical JSON is enough to break it: key order
	// and whitespace are part of the signed bytes.
	var round map[string]any
	if err := json.Unmarshal(payload, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reserialized, err := json.Marshal(round)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := svc.HandleWebhook(context.Background(), reserialized, signature); err == nil {
		t.Fatal("a re-serialized body verified; the signature is not being checked against the raw bytes")
	} else if code := appErrorCode(t, err); code != apperror.WebhookSignatureInvalid {
		t.Fatalf("code = %s, want %s", code, apperror.WebhookSignatureInvalid)
	}
}

// TestWebhook_UnknownEventTypeIsIgnored: 200, and not even recorded —
// stripe_events must not fill up with rows for events nothing reconciles.
func TestWebhook_UnknownEventTypeIsIgnored(t *testing.T) {
	f := newFakeDB()
	svc := newWebhookService(f, testWebhookSecret)

	for _, eventType := range []string{
		"payment_intent.succeeded", "customer.created", "charge.refunded", "invoice.finalized",
	} {
		payload := eventPayload(t, eventOpts{
			id: "evt_" + eventType, eventType: eventType, created: time.Now(), object: `{"id":"obj_1"}`,
		})
		if err := deliver(t, svc, payload); err != nil {
			t.Fatalf("%s: %v (an unhandled type must be a clean 200)", eventType, err)
		}
	}
	if len(f.events) != 0 {
		t.Fatalf("unhandled event types recorded %d stripe_events rows", len(f.events))
	}
	if f.upserts != 0 || f.updates != 0 {
		t.Fatalf("unhandled event types wrote state: upserts=%d updates=%d", f.upserts, f.updates)
	}
}

// TestWebhook_ReplayedEventIsANoOp. Stripe redelivers; the second arrival
// must change nothing, including not re-running the plan assignment.
func TestWebhook_ReplayedEventIsANoOp(t *testing.T) {
	f := newFakeDB()
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()

	payload := eventPayload(t, eventOpts{
		id: "evt_replay", eventType: eventSubscriptionCreated, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", "active", "price_pro", orgID.String(), pro.ID.String(), 1893456000, false),
	})

	svc := newWebhookService(f, testWebhookSecret)
	if err := deliver(t, svc, payload); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	first := f.subs[orgID]
	if first.PlanID != pro.ID {
		t.Fatalf("plan_id = %s, want %s", first.PlanID, pro.ID)
	}
	upsertsAfterFirst, updatesAfterFirst := f.upserts, f.updates

	for i := 0; i < 3; i++ {
		if err := deliver(t, svc, payload); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}

	if f.upserts != upsertsAfterFirst || f.updates != updatesAfterFirst {
		t.Fatalf("replays re-ran the reconciliation: upserts %d->%d, updates %d->%d",
			upsertsAfterFirst, f.upserts, updatesAfterFirst, f.updates)
	}
	if got := f.subs[orgID]; !got.equal(first) {
		t.Fatalf("row changed across replays: %+v, want %+v", got, first)
	}
}

// TestWebhook_OutOfOrderUpdateDoesNotRegressState is the trap-3 test.
//
// stripe_events alone does not cover this: a stale customer.subscription
// .updated is a DIFFERENT event id, so idempotency lets it straight through.
// The watermark (org_subscriptions.stripe_event_at, migration 00016) is what
// rejects it.
func TestWebhook_OutOfOrderUpdateDoesNotRegressState(t *testing.T) {
	f := newFakeDB()
	free := f.addPlan("free", "price_free")
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()
	seedPayingOrg(f, orgID, free.ID, "cus_1", "sub_1", "active", time.Time{})
	// Clear the seeded watermark so the first event establishes it.
	row := f.subs[orgID]
	row.StripeEventAt = pgtype.Timestamp{}
	f.subs[orgID] = row

	svc := newWebhookService(f, testWebhookSecret)

	older := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	newer := time.Now().Truncate(time.Second)

	// The NEWER event arrives first (which is exactly what "out of order"
	// means): the org upgrades to pro and goes active.
	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_new", eventType: eventSubscriptionUpdated, created: newer,
		object: subscriptionObject("sub_1", "cus_1", "active", "price_pro", orgID.String(), pro.ID.String(), 1893456000, false),
	})); err != nil {
		t.Fatalf("newer event: %v", err)
	}
	after := f.subs[orgID]
	if after.PlanID != pro.ID || str(after.Status) != "active" {
		t.Fatalf("after the newer event: plan=%s status=%s", after.PlanID, str(after.Status))
	}

	// Now the stale one turns up: it would put the org back on free and
	// past_due. It must be ignored.
	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_old", eventType: eventSubscriptionUpdated, created: older,
		object: subscriptionObject("sub_1", "cus_1", statusPastDue, "price_free", orgID.String(), free.ID.String(), 0, true),
	})); err != nil {
		t.Fatalf("stale event: %v", err)
	}

	got := f.subs[orgID]
	if got.PlanID != pro.ID {
		t.Fatalf("a stale event regressed plan_id to %s, want %s", got.PlanID, pro.ID)
	}
	if str(got.Status) != "active" {
		t.Fatalf("a stale event regressed status to %s, want active", str(got.Status))
	}
	if got.CancelAtPeriodEnd {
		t.Fatal("a stale event set cancel_at_period_end")
	}
	if !got.StripeEventAt.Valid || !got.StripeEventAt.Time.Equal(newer) {
		t.Fatalf("watermark = %v, want the newer event's %v", got.StripeEventAt.Time, newer)
	}

	t.Run("the stale event is still recorded, so its redelivery is cheap", func(t *testing.T) {
		if _, ok := f.events["evt_old"]; !ok {
			t.Fatal("a stale event was not recorded in stripe_events")
		}
	})

	t.Run("an event at the same second is applied, last writer winning", func(t *testing.T) {
		// Stripe event timestamps are whole seconds, so a tie is genuinely
		// ambiguous — there is no monotonic version to break it with. This
		// pins the documented choice rather than leaving it accidental.
		if err := deliver(t, svc, eventPayload(t, eventOpts{
			id: "evt_tie", eventType: eventSubscriptionUpdated, created: newer,
			object: subscriptionObject("sub_1", "cus_1", statusPastDue, "price_pro", orgID.String(), pro.ID.String(), 0, false),
		})); err != nil {
			t.Fatalf("tie event: %v", err)
		}
		if got := str(f.subs[orgID].Status); got != statusPastDue {
			t.Fatalf("status = %s, want %s (a same-second event must apply)", got, statusPastDue)
		}
	})
}

// TestWebhook_PlanChangeKeepsCustomLimits is plan invariant 2: custom_limits
// is the admin override, and a billing event must not clear it.
//
// The mechanism is that the plan move goes through
// subscription.Service.AssignPlan(Tx) — whose ON CONFLICT sets plan_id and
// updated_at only — and that billing's own UPDATE names the Stripe columns
// explicitly and never custom_limits.
func TestWebhook_PlanChangeKeepsCustomLimits(t *testing.T) {
	f := newFakeDB()
	free := f.addPlan("free", "price_free")
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()

	custom := json.RawMessage(`{"max_connectors":99,"max_tool_calls_per_month":500000}`)
	seedPayingOrg(f, orgID, free.ID, "cus_1", "sub_1", "active", time.Time{})
	row := f.subs[orgID]
	row.CustomLimits = custom
	row.StripeEventAt = pgtype.Timestamp{}
	f.subs[orgID] = row

	svc := newWebhookService(f, testWebhookSecret)
	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_upgrade", eventType: eventSubscriptionUpdated, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", "active", "price_pro", orgID.String(), pro.ID.String(), 1893456000, false),
	})); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	got := f.subs[orgID]
	if got.PlanID != pro.ID {
		t.Fatalf("plan_id = %s, want %s — the upgrade did not apply", got.PlanID, pro.ID)
	}
	if string(got.CustomLimits) != string(custom) {
		t.Fatalf("custom_limits = %s, want %s — a webhook-driven plan change cleared the admin override",
			got.CustomLimits, custom)
	}
}

// TestWebhook_CancellationClearsTheSubscriptionAndDowngrades is decision 4:
// stripe_subscription_id IS NULL is the canonical "not paying" signal, so a
// cancellation clears it rather than pointing at a dead Stripe object.
func TestWebhook_CancellationClearsTheSubscriptionAndDowngrades(t *testing.T) {
	f := newFakeDB()
	free := f.addPlan("free", "")
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()
	seedPayingOrg(f, orgID, pro.ID, "cus_1", "sub_1", "active", time.Now().Add(-time.Hour).Truncate(time.Second))

	svc := newWebhookService(f, testWebhookSecret)
	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_cancel", eventType: eventSubscriptionDeleted, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", statusCanceled, "price_pro", orgID.String(), pro.ID.String(), 0, true),
	})); err != nil {
		t.Fatalf("cancellation: %v", err)
	}

	got := f.subs[orgID]
	if got.StripeSubscriptionID != nil {
		t.Fatalf("stripe_subscription_id = %q, want NULL (decision 4)", *got.StripeSubscriptionID)
	}
	if got.PlanID != free.ID {
		t.Fatalf("plan_id = %s, want the free plan %s", got.PlanID, free.ID)
	}
	if str(got.Status) != statusCanceled {
		t.Fatalf("status = %s, want %s", str(got.Status), statusCanceled)
	}
	if got.CancelAtPeriodEnd {
		t.Fatal("cancel_at_period_end stayed true next to a NULL subscription")
	}
	if got.StripeCustomerID == nil || *got.StripeCustomerID != "cus_1" {
		t.Fatalf("the Stripe Customer was discarded: %v", got.StripeCustomerID)
	}
}

// TestWebhook_TenantIsolation covers the class SECURITY.md names first: one
// org's subscription must be unreachable through another org's identifiers.
//
// The property under test is the resolution order in resolveOrganization —
// the recorded, uniqueness-enforced customer/subscription linkage beats
// metadata, so an event carrying org A's Stripe ids cannot be steered onto
// org B by its metadata, and vice versa.
func TestWebhook_TenantIsolation(t *testing.T) {
	f := newFakeDB()
	free := f.addPlan("free", "price_free")
	pro := f.addPlan("pro", "price_pro")
	orgA, orgB := uuid.New(), uuid.New()
	seedPayingOrg(f, orgA, free.ID, "cus_a", "sub_a", "active", time.Time{})
	seedPayingOrg(f, orgB, free.ID, "cus_b", "sub_b", "active", time.Time{})
	for _, id := range []uuid.UUID{orgA, orgB} {
		row := f.subs[id]
		row.StripeEventAt = pgtype.Timestamp{}
		f.subs[id] = row
	}
	beforeB := f.subs[orgB]

	svc := newWebhookService(f, testWebhookSecret)

	// An event for org A's subscription, but with org B's id in metadata.
	// The recorded linkage must win: org A moves, org B does not.
	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_cross", eventType: eventSubscriptionUpdated, created: time.Now(),
		object: subscriptionObject("sub_a", "cus_a", "active", "price_pro", orgB.String(), pro.ID.String(), 1893456000, false),
	})); err != nil {
		t.Fatalf("cross-tenant event: %v", err)
	}

	if got := f.subs[orgA].PlanID; got != pro.ID {
		t.Fatalf("org A's plan = %s, want %s — the event did not land on the org that owns sub_a", got, pro.ID)
	}
	if got := f.subs[orgB]; !got.equal(beforeB) {
		t.Fatalf("org B's row changed: %+v, want %+v — metadata steered an event onto another tenant", got, beforeB)
	}

	t.Run("an event naming neither a known id nor a known org changes nothing", func(t *testing.T) {
		before := len(f.subs)
		upserts, updates := f.upserts, f.updates
		if err := deliver(t, svc, eventPayload(t, eventOpts{
			id: "evt_orphan", eventType: eventSubscriptionUpdated, created: time.Now(),
			object: subscriptionObject("sub_unknown", "cus_unknown", "active", "price_pro", "", "", 0, false),
		})); err != nil {
			t.Fatalf("orphan event: %v", err)
		}
		if len(f.subs) != before || f.upserts != upserts || f.updates != updates {
			t.Fatalf("an unmappable event wrote state: rows %d->%d upserts %d->%d updates %d->%d",
				before, len(f.subs), upserts, f.upserts, updates, f.updates)
		}
	})
}

// TestWebhook_MidProcessingFailureRollsBackTheClaim is the trap-2 test, and
// the reason the stripe_events claim lives inside the same transaction as
// the state change.
//
// Claim-then-process in separate transactions would leave the event
// permanently marked handled, so Stripe's retry would find a duplicate id
// and do nothing — the state change lost forever, silently. Here the
// failure rolls the claim back with it, and the retry works.
func TestWebhook_MidProcessingFailureRollsBackTheClaim(t *testing.T) {
	f := newFakeDB()
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()

	svc := newWebhookService(f, testWebhookSecret)
	payload := eventPayload(t, eventOpts{
		id: "evt_fail", eventType: eventSubscriptionCreated, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", "active", "price_pro", orgID.String(), pro.ID.String(), 1893456000, false),
	})

	f.upsertErr = errors.New("database is having a day")
	if err := deliver(t, svc, payload); err == nil {
		t.Fatal("a failed reconciliation returned nil; Stripe would never retry")
	}
	if _, ok := f.events["evt_fail"]; ok {
		t.Fatal("the event stayed claimed after a failure — Stripe's retry is now a silent no-op")
	}
	if _, ok := f.subs[orgID]; ok {
		t.Fatal("a rolled-back transaction left an org_subscriptions row behind")
	}

	// Stripe retries; this time it works, exactly as the design intends.
	f.upsertErr = nil
	if err := deliver(t, svc, payload); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if got := f.subs[orgID].PlanID; got != pro.ID {
		t.Fatalf("plan_id = %s after the retry, want %s", got, pro.ID)
	}
	if _, ok := f.events["evt_fail"]; !ok {
		t.Fatal("the successful retry did not record the event")
	}
}

// TestWebhook_CheckoutSessionCompletedCreatesTheRow. An org that has never
// been assigned a plan has NO org_subscriptions row (billing never creates
// one — see Service.ensureCustomer), so the first event of a new
// subscription is also the row's creation, and metadata is the only source
// that can map it.
func TestWebhook_CheckoutSessionCompletedCreatesTheRow(t *testing.T) {
	f := newFakeDB()
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()

	svc := newWebhookService(f, testWebhookSecret)
	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_checkout", eventType: eventCheckoutSessionCompleted, created: time.Now(),
		object: checkoutSessionObject("cs_1", "cus_1", "sub_1", orgID.String(), pro.ID.String()),
	})); err != nil {
		t.Fatalf("checkout.session.completed: %v", err)
	}

	got, ok := f.subs[orgID]
	if !ok {
		t.Fatal("no org_subscriptions row was created for a completed checkout")
	}
	if got.PlanID != pro.ID {
		t.Fatalf("plan_id = %s, want %s", got.PlanID, pro.ID)
	}
	if got.StripeCustomerID == nil || *got.StripeCustomerID != "cus_1" {
		t.Fatalf("stripe_customer_id = %v", got.StripeCustomerID)
	}
	if got.StripeSubscriptionID == nil || *got.StripeSubscriptionID != "sub_1" {
		t.Fatalf("stripe_subscription_id = %v", got.StripeSubscriptionID)
	}
	if got.Status != nil {
		t.Fatalf("status = %q — a Session says the customer paid, not what state the Subscription is in; "+
			"customer.subscription.created owns that column", *got.Status)
	}
}

// TestWebhook_InvoiceEvents: invoice.payment_failed moves the status
// immediately, invoice.paid refreshes without claiming "active", and
// NEITHER may move the plan.
//
// That last part matters: an invoice's metadata is a snapshot frozen at
// finalization, so trusting it for a plan would walk a Customer Portal plan
// change backwards on the next renewal.
func TestWebhook_InvoiceEvents(t *testing.T) {
	f := newFakeDB()
	free := f.addPlan("free", "price_free")
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()
	seedPayingOrg(f, orgID, pro.ID, "cus_1", "sub_1", "active", time.Time{})
	row := f.subs[orgID]
	row.StripeEventAt = pgtype.Timestamp{}
	f.subs[orgID] = row

	svc := newWebhookService(f, testWebhookSecret)

	// A stale metadata snapshot naming the FREE plan, of the kind a
	// Portal-driven upgrade leaves behind on later invoices.
	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_inv_failed", eventType: eventInvoicePaymentFailed, created: time.Now().Truncate(time.Second),
		object: invoiceObject("in_1", "cus_1", "sub_1", orgID.String(), free.ID.String()),
	})); err != nil {
		t.Fatalf("invoice.payment_failed: %v", err)
	}
	got := f.subs[orgID]
	if str(got.Status) != statusPastDue {
		t.Fatalf("status = %s, want %s", str(got.Status), statusPastDue)
	}
	if got.PlanID != pro.ID {
		t.Fatalf("plan_id = %s, want %s — an invoice moved the plan", got.PlanID, pro.ID)
	}

	if err := deliver(t, svc, eventPayload(t, eventOpts{
		id: "evt_inv_paid", eventType: eventInvoicePaid, created: time.Now().Add(time.Second).Truncate(time.Second),
		object: invoiceObject("in_2", "cus_1", "sub_1", orgID.String(), free.ID.String()),
	})); err != nil {
		t.Fatalf("invoice.paid: %v", err)
	}
	got = f.subs[orgID]
	if got.PlanID != pro.ID {
		t.Fatalf("plan_id = %s, want %s — invoice.paid moved the plan", got.PlanID, pro.ID)
	}
	if str(got.Status) != statusPastDue {
		t.Fatalf("status = %s — invoice.paid must not assert 'active'; the customer.subscription.updated "+
			"that follows owns that column", str(got.Status))
	}
}

// TestWebhook_NotConfiguredIs501 mirrors the two guarded routes: a
// deployment with no STRIPE_WEBHOOK_SECRET mounts the route and answers
// BILLING_NOT_CONFIGURED rather than pretending to verify anything. An
// empty secret must never mean "accept everything".
func TestWebhook_NotConfiguredIs501(t *testing.T) {
	f := newFakeDB()
	svc := newWebhookService(f, "")

	payload := eventPayload(t, eventOpts{
		id: "evt_x", eventType: eventSubscriptionUpdated, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", "active", "price_pro", uuid.NewString(), uuid.NewString(), 0, false),
	})
	err := svc.HandleWebhook(context.Background(), payload, signedPayload(t, payload, testWebhookSecret, time.Now()))
	if code := appErrorCode(t, err); code != apperror.BillingNotConfigured {
		t.Fatalf("code = %s, want %s", code, apperror.BillingNotConfigured)
	}
	if len(f.events) != 0 || f.upserts != 0 {
		t.Fatal("an unconfigured webhook wrote state")
	}
}

// ---- handler level ----

// TestWebhookHandler_MountedWithoutAnyAuthGuard checks the two properties
// server.go's wiring has to preserve: the route answers without a JWT or an
// x-organization-id (Stripe sends neither), and the body reaching the
// service is the verbatim request body.
func TestWebhookHandler_MountedWithoutAnyAuthGuard(t *testing.T) {
	f := newFakeDB()
	pro := f.addPlan("pro", "price_pro")
	orgID := uuid.New()
	svc := newWebhookService(f, testWebhookSecret)

	e := newTestEcho(svc, newTestGuards(uuid.New(), "nobody@example.com", nil))
	NewHandler(svc).RegisterWebhook(e)

	payload := eventPayload(t, eventOpts{
		id: "evt_http", eventType: eventSubscriptionCreated, created: time.Now(),
		object: subscriptionObject("sub_1", "cus_1", "active", "price_pro", orgID.String(), pro.ID.String(), 1893456000, false),
	})

	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signedPayload(t, payload, testWebhookSecret, time.Now()))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var body WebhookResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !body.Received {
		t.Fatalf("body = %+v, want received=true", body)
	}
	if f.subs[orgID].PlanID != pro.ID {
		t.Fatal("the handler did not reach the reconciler with the raw body")
	}

	t.Run("a bad signature is a 400 through the handler too", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(payload)))
		req.Header.Set("Stripe-Signature", "t=1,v1=00")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an oversized body is rejected before verification", func(t *testing.T) {
		huge := strings.Repeat("a", maxWebhookBodyBytes+1)
		req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(huge))
		req.Header.Set("Stripe-Signature", "t=1,v1=00")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})
}
