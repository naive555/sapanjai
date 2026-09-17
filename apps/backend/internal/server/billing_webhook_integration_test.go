package server_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/module/billing"
	"github.com/sapanjai/backend/internal/module/subscription"
)

// POST /billing/webhook end to end: the real wired server, the real Echo
// middleware stack, real Postgres, and the real Stripe signature verifier —
// but no network. The payloads are built here and signed here with Stripe's
// documented v1 scheme (HMAC-SHA256 over "<timestamp>.<body>"), which is
// local crypto, so no test in this file needs a Stripe account, a key, or
// an outbound connection.
//
// What makes it worth running against the real server rather than only as a
// unit test: the route is mounted OUTSIDE the guarded /billing group with no
// auth middleware at all (Stripe presents no JWT and no x-organization-id),
// the signature is verified against the raw request body after the global
// Recover/RequestID/requestLogger stack has run, and the reconciliation
// commits the stripe_events claim and the org_subscriptions change in one
// real transaction. None of those three is exercised by a mock.

const (
	webhookPath       = "/billing/webhook"
	testWebhookSecret = "whsec_integration_test_secret_value"
)

// withWebhookSecret is the configure func that turns the webhook on.
func withWebhookSecret(cfg *config.Config) {
	cfg.StripeWebhookSecret = testWebhookSecret
}

// signStripePayload builds a Stripe-Signature header for payload, deriving
// the HMAC here rather than through the SDK helper the production code
// calls — so the two derivations have to agree independently.
func signStripePayload(t *testing.T, payload []byte, secret string, at time.Time) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", at.Unix())
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// subscriptionEventPayload renders a customer.subscription.* event.
// billing.StripeAPIVersion is the version the SDK pins, and the SDK rejects
// an event from another release train, so it is the value a real Stripe
// webhook endpoint must be configured with too.
func subscriptionEventPayload(eventID, eventType string, created time.Time,
	subID, customerID, status, priceID string, orgID, planID uuid.UUID, cancelAtPeriodEnd bool,
) []byte {
	object := fmt.Sprintf(`{
		"id":%q,"object":"subscription","status":%q,"cancel_at_period_end":%t,
		"customer":%q,"metadata":{"organization_id":%q,"plan_id":%q},
		"items":{"object":"list","data":[
			{"id":"si_1","object":"subscription_item","current_period_end":1893456000,
			 "price":{"id":%q,"object":"price"}}]}
	}`, subID, status, cancelAtPeriodEnd, customerID, orgID, planID, priceID)

	return []byte(fmt.Sprintf(
		`{"id":%q,"object":"event","api_version":%q,"created":%d,"type":%q,"data":{"object":%s}}`,
		eventID, billing.StripeAPIVersion, created.Unix(), eventType, object))
}

// postWebhook delivers payload with signature and returns the status and
// decoded body.
func postWebhook(t *testing.T, client *http.Client, baseURL string, payload []byte, signature string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL+webhookPath, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("Stripe-Signature", signature)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup

	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// deliverSigned is the happy path: sign now, post, require 200.
func deliverSigned(t *testing.T, client *http.Client, baseURL string, payload []byte) {
	t.Helper()
	status, body := postWebhook(t, client, baseURL, payload, signStripePayload(t, payload, testWebhookSecret, time.Now()))
	if status != http.StatusOK {
		t.Fatalf("webhook: status = %d, want 200; body = %v", status, body)
	}
}

// billingRow is the subset of org_subscriptions these tests assert on. Read
// with raw SQL because there is no tenant-facing route that exposes the
// Stripe columns (the subscription page shows plan and limits only).
type billingRow struct {
	PlanID               uuid.UUID
	CustomLimits         *string
	StripeCustomerID     *string
	StripeSubscriptionID *string
	Status               *string
	CancelAtPeriodEnd    bool
	StripeEventAt        *time.Time
}

func readBillingRow(t *testing.T, store *database.Store, orgID uuid.UUID) billingRow {
	t.Helper()

	var row billingRow
	err := store.Pool.QueryRow(context.Background(),
		`SELECT plan_id, custom_limits::text, stripe_customer_id, stripe_subscription_id,
		        status, cancel_at_period_end, stripe_event_at
		 FROM org_subscriptions WHERE organization_id = $1`, orgID).
		Scan(&row.PlanID, &row.CustomLimits, &row.StripeCustomerID, &row.StripeSubscriptionID,
			&row.Status, &row.CancelAtPeriodEnd, &row.StripeEventAt)
	if err != nil {
		t.Fatalf("read org_subscriptions for %s: %v", orgID, err)
	}
	return row
}

func setCustomLimits(t *testing.T, store *database.Store, orgID uuid.UUID, limits string) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(),
		`UPDATE org_subscriptions SET custom_limits = $2::jsonb WHERE organization_id = $1`,
		orgID, limits); err != nil {
		t.Fatalf("set custom_limits: %v", err)
	}
}

// uniquePriceID keeps plan_prices.stripe_price_id unique across the tests
// in this package, which share one database.
func uniquePriceID() string { return "price_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

func derefString(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestIntegration_BillingWebhook_ReconcilesASubscription is the main path:
// an unauthenticated, signature-verified delivery moves an org's plan and
// records the Stripe linkage, in one transaction against real Postgres.
func TestIntegration_BillingWebhook_ReconcilesASubscription(t *testing.T) {
	ts, _, store := setupTestServer(t, withWebhookSecret)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "webhook-reconcile")
	orgID := uuid.MustParse(org.ID)
	freePlan := createPlan(t, store, map[string]int{"max_members": 5})
	proPlan := createPlan(t, store, map[string]int{"max_members": 50})
	proPrice := uniquePriceID()
	insertPlanPrice(t, store, proPlan, proPrice, 99000, "thb", "month", true)
	assignPlanDirect(t, store, orgID, freePlan)

	customerID := "cus_" + uuid.NewString()
	subID := "sub_" + uuid.NewString()

	// No Authorization header and no x-organization-id anywhere: Stripe
	// sends neither, so a route that needed them could never be reached.
	deliverSigned(t, client, ts.URL, subscriptionEventPayload(
		"evt_"+uuid.NewString(), "customer.subscription.created", time.Now(),
		subID, customerID, "active", proPrice, orgID, proPlan, false))

	row := readBillingRow(t, store, orgID)
	if row.PlanID != proPlan {
		t.Fatalf("plan_id = %s, want %s", row.PlanID, proPlan)
	}
	if derefString(row.StripeCustomerID) != customerID {
		t.Fatalf("stripe_customer_id = %s, want %s", derefString(row.StripeCustomerID), customerID)
	}
	if derefString(row.StripeSubscriptionID) != subID {
		t.Fatalf("stripe_subscription_id = %s, want %s", derefString(row.StripeSubscriptionID), subID)
	}
	if derefString(row.Status) != "active" {
		t.Fatalf("status = %s, want active", derefString(row.Status))
	}
	if row.StripeEventAt == nil {
		t.Fatal("stripe_event_at was not set; the out-of-order watermark is not being written")
	}

	t.Run("the entitlement the gateway reads followed the plan", func(t *testing.T) {
		// The point of the whole exercise: org_subscriptions.plan_id is the
		// entitlement record (plan invariant 1), and EffectiveLimits is the
		// single answer to "what may this org do".
		limits, err := subscription.NewService(store).EffectiveLimits(context.Background(), orgID)
		if err != nil {
			t.Fatalf("EffectiveLimits: %v", err)
		}
		if limits["max_members"] != 50 {
			t.Fatalf("max_members = %v, want 50 (the pro plan)", limits["max_members"])
		}
	})

	t.Run("cancelling clears stripe_subscription_id and downgrades to free", func(t *testing.T) {
		// Decision 4: NULL is the canonical "not paying" signal, so the
		// column is cleared rather than left pointing at a dead object.
		// cmd/seed's "free" plan is the downgrade target, so this subtest
		// needs the real seeded plan rather than createPlan's unique name.
		seedFreePlan(t, store)

		deliverSigned(t, client, ts.URL, subscriptionEventPayload(
			"evt_"+uuid.NewString(), "customer.subscription.deleted", time.Now().Add(time.Second),
			subID, customerID, "canceled", proPrice, orgID, proPlan, true))

		row := readBillingRow(t, store, orgID)
		if row.StripeSubscriptionID != nil {
			t.Fatalf("stripe_subscription_id = %q, want NULL", *row.StripeSubscriptionID)
		}
		if derefString(row.Status) != "canceled" {
			t.Fatalf("status = %s, want canceled", derefString(row.Status))
		}
		if derefString(row.StripeCustomerID) != customerID {
			t.Fatal("the Stripe Customer was discarded; decision 4 keeps it for the next upgrade")
		}
		if row.CancelAtPeriodEnd {
			t.Fatal("cancel_at_period_end stayed true next to a NULL subscription")
		}
	})
}

// seedFreePlan ensures a plan literally named "free" exists — the name the
// webhook downgrades a cancelled subscription to, and the one cmd/seed
// creates. Idempotent, because UpsertPlan is ON CONFLICT DO NOTHING and the
// tests in this package share a database.
func seedFreePlan(t *testing.T, store *database.Store) uuid.UUID {
	t.Helper()

	if _, err := store.Pool.Exec(context.Background(),
		`INSERT INTO plans (name, limits) VALUES ('free', '{"max_members":5,"max_roles":3,"max_connectors":2}'::jsonb)
		 ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("seed free plan: %v", err)
	}
	plan, err := store.GetPlanByName(context.Background(), "free")
	if err != nil {
		t.Fatalf("GetPlanByName(free): %v", err)
	}
	return plan.ID
}

// TestIntegration_BillingWebhook_ReplayIsANoOp: Stripe redelivers, and the
// second arrival must be a clean 200 that changes nothing. The mechanism is
// the stripe_events primary key, claimed inside the reconciling transaction.
func TestIntegration_BillingWebhook_ReplayIsANoOp(t *testing.T) {
	ts, _, store := setupTestServer(t, withWebhookSecret)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "webhook-replay")
	orgID := uuid.MustParse(org.ID)
	proPlan := createPlan(t, store, map[string]int{"max_members": 50})
	otherPlan := createPlan(t, store, map[string]int{"max_members": 7})
	proPrice := uniquePriceID()
	insertPlanPrice(t, store, proPlan, proPrice, 99000, "thb", "month", true)

	eventID := "evt_" + uuid.NewString()
	payload := subscriptionEventPayload(eventID, "customer.subscription.created", time.Now(),
		"sub_"+uuid.NewString(), "cus_"+uuid.NewString(), "active", proPrice, orgID, proPlan, false)

	deliverSigned(t, client, ts.URL, payload)
	first := readBillingRow(t, store, orgID)

	// Move the row out from under the replay. If the replay were processed
	// it would put the org back on pro; a no-op leaves this alone.
	assignPlanDirect(t, store, orgID, otherPlan)

	for i := 0; i < 3; i++ {
		deliverSigned(t, client, ts.URL, payload)
	}

	after := readBillingRow(t, store, orgID)
	if after.PlanID != otherPlan {
		t.Fatalf("plan_id = %s after replays, want %s — the replay was reprocessed", after.PlanID, otherPlan)
	}
	if after.StripeEventAt == nil || !after.StripeEventAt.Equal(*first.StripeEventAt) {
		t.Fatalf("the watermark moved on a replay: %v -> %v", first.StripeEventAt, after.StripeEventAt)
	}

	var rows int
	if err := store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM stripe_events WHERE id = $1`, eventID).Scan(&rows); err != nil {
		t.Fatalf("count stripe_events: %v", err)
	}
	if rows != 1 {
		t.Fatalf("stripe_events has %d rows for %s, want exactly 1", rows, eventID)
	}
}

// TestIntegration_BillingWebhook_OutOfOrderDoesNotRegress. stripe_events
// only stops an exact replay; a STALE customer.subscription.updated is a
// different event id and needs the watermark (org_subscriptions
// .stripe_event_at, migration 00016) to be rejected.
func TestIntegration_BillingWebhook_OutOfOrderDoesNotRegress(t *testing.T) {
	ts, _, store := setupTestServer(t, withWebhookSecret)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "webhook-order")
	orgID := uuid.MustParse(org.ID)
	smallPlan := createPlan(t, store, map[string]int{"max_members": 5})
	bigPlan := createPlan(t, store, map[string]int{"max_members": 50})
	smallPrice, bigPrice := uniquePriceID(), uniquePriceID()
	insertPlanPrice(t, store, smallPlan, smallPrice, 10000, "thb", "month", true)
	insertPlanPrice(t, store, bigPlan, bigPrice, 99000, "thb", "month", true)

	subID, customerID := "sub_"+uuid.NewString(), "cus_"+uuid.NewString()
	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()

	// The newer event arrives first — which is what out-of-order means.
	deliverSigned(t, client, ts.URL, subscriptionEventPayload(
		"evt_"+uuid.NewString(), "customer.subscription.updated", newer,
		subID, customerID, "active", bigPrice, orgID, bigPlan, false))

	// Then the straggler, which would put the org back on the small plan
	// and past_due.
	deliverSigned(t, client, ts.URL, subscriptionEventPayload(
		"evt_"+uuid.NewString(), "customer.subscription.updated", older,
		subID, customerID, "past_due", smallPrice, orgID, smallPlan, true))

	row := readBillingRow(t, store, orgID)
	if row.PlanID != bigPlan {
		t.Fatalf("plan_id = %s, want %s — a stale event regressed the plan", row.PlanID, bigPlan)
	}
	if derefString(row.Status) != "active" {
		t.Fatalf("status = %s, want active — a stale event regressed the status", derefString(row.Status))
	}
	if row.CancelAtPeriodEnd {
		t.Fatal("a stale event set cancel_at_period_end")
	}
}

// TestIntegration_BillingWebhook_BadSignatureIs400AndChangesNothing is the
// trap-1 requirement. Verification runs before the first database call, so
// there is no stripe_events row and no org_subscriptions change to find
// afterwards.
func TestIntegration_BillingWebhook_BadSignatureIs400AndChangesNothing(t *testing.T) {
	ts, _, store := setupTestServer(t, withWebhookSecret)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "webhook-badsig")
	orgID := uuid.MustParse(org.ID)
	basePlan := createPlan(t, store, map[string]int{"max_members": 5})
	otherPlan := createPlan(t, store, map[string]int{"max_members": 50})
	otherPrice := uniquePriceID()
	insertPlanPrice(t, store, otherPlan, otherPrice, 99000, "thb", "month", true)
	assignPlanDirect(t, store, orgID, basePlan)

	eventID := "evt_" + uuid.NewString()
	payload := subscriptionEventPayload(eventID, "customer.subscription.updated", time.Now(),
		"sub_"+uuid.NewString(), "cus_"+uuid.NewString(), "active", otherPrice, orgID, otherPlan, false)

	for name, signature := range map[string]string{
		"absent":                                 "",
		"garbage":                                "t=1,v1=deadbeef",
		"malformed header":                       "nonsense",
		"signed with the wrong secret":           signStripePayload(t, payload, "whsec_not_the_configured_one", time.Now()),
		"valid but outside the tolerance window": signStripePayload(t, payload, testWebhookSecret, time.Now().Add(-time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			status, body := postWebhook(t, client, ts.URL, payload, signature)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %v", status, body)
			}
			if body["message"] != "Invalid webhook signature" {
				t.Fatalf("message = %v, want %q", body["message"], "Invalid webhook signature")
			}

			row := readBillingRow(t, store, orgID)
			if row.PlanID != basePlan {
				t.Fatalf("plan_id = %s, want %s — a rejected delivery changed state", row.PlanID, basePlan)
			}
			if row.StripeSubscriptionID != nil || row.StripeEventAt != nil {
				t.Fatalf("a rejected delivery wrote Stripe columns: sub=%v at=%v",
					row.StripeSubscriptionID, row.StripeEventAt)
			}
			var rows int
			if err := store.Pool.QueryRow(context.Background(),
				`SELECT count(*) FROM stripe_events WHERE id = $1`, eventID).Scan(&rows); err != nil {
				t.Fatalf("count stripe_events: %v", err)
			}
			if rows != 0 {
				t.Fatalf("a rejected delivery recorded %d stripe_events rows", rows)
			}
		})
	}
}

// TestIntegration_BillingWebhook_UnknownEventTypeIsIgnored: 200 so Stripe
// stops retrying (a 4xx here eventually gets the endpoint disabled, taking
// the events that DO matter with it), and no stripe_events row, so a noisy
// account cannot fill that table with rows nothing will ever reconcile.
func TestIntegration_BillingWebhook_UnknownEventTypeIsIgnored(t *testing.T) {
	ts, _, store := setupTestServer(t, withWebhookSecret)
	client := ts.Client()

	for _, eventType := range []string{"payment_intent.succeeded", "customer.created", "charge.refunded"} {
		eventID := "evt_" + uuid.NewString()
		payload := []byte(fmt.Sprintf(
			`{"id":%q,"object":"event","api_version":%q,"created":%d,"type":%q,"data":{"object":{"id":"obj_1"}}}`,
			eventID, billing.StripeAPIVersion, time.Now().Unix(), eventType))

		status, body := postWebhook(t, client, ts.URL, payload,
			signStripePayload(t, payload, testWebhookSecret, time.Now()))
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body = %v", eventType, status, body)
		}
		if received, _ := body["received"].(bool); !received {
			t.Fatalf("%s: body = %v, want received=true", eventType, body)
		}

		var rows int
		if err := store.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM stripe_events WHERE id = $1`, eventID).Scan(&rows); err != nil {
			t.Fatalf("count stripe_events: %v", err)
		}
		if rows != 0 {
			t.Fatalf("%s: recorded %d stripe_events rows for an unhandled type", eventType, rows)
		}
	}
}

// TestIntegration_BillingWebhook_PlanChangeKeepsCustomLimits is plan
// invariant 2 end to end. custom_limits is the admin override; it is
// applied over plan limits by EffectiveLimits, and a webhook-driven plan
// change must not clear it.
func TestIntegration_BillingWebhook_PlanChangeKeepsCustomLimits(t *testing.T) {
	ts, _, store := setupTestServer(t, withWebhookSecret)
	client := ts.Client()
	ctx := context.Background()

	org := createOrgWithOwner(t, client, ts.URL, "webhook-customlimits")
	orgID := uuid.MustParse(org.ID)
	smallPlan := createPlan(t, store, map[string]int{"max_members": 5, "max_connectors": 2})
	bigPlan := createPlan(t, store, map[string]int{"max_members": 50, "max_connectors": 10})
	bigPrice := uniquePriceID()
	insertPlanPrice(t, store, bigPlan, bigPrice, 99000, "thb", "month", true)

	assignPlanDirect(t, store, orgID, smallPlan)
	setCustomLimits(t, store, orgID, `{"max_connectors": 99}`)

	deliverSigned(t, client, ts.URL, subscriptionEventPayload(
		"evt_"+uuid.NewString(), "customer.subscription.updated", time.Now(),
		"sub_"+uuid.NewString(), "cus_"+uuid.NewString(), "active", bigPrice, orgID, bigPlan, false))

	row := readBillingRow(t, store, orgID)
	if row.PlanID != bigPlan {
		t.Fatalf("plan_id = %s, want %s — the plan change did not apply", row.PlanID, bigPlan)
	}
	if row.CustomLimits == nil || !strings.Contains(*row.CustomLimits, "99") {
		t.Fatalf("custom_limits = %v — a webhook-driven plan change cleared the admin override",
			row.CustomLimits)
	}

	limits, err := subscription.NewService(store).EffectiveLimits(ctx, orgID)
	if err != nil {
		t.Fatalf("EffectiveLimits: %v", err)
	}
	if limits["max_connectors"] != 99 {
		t.Fatalf("max_connectors = %v, want the custom override 99", limits["max_connectors"])
	}
	if limits["max_members"] != 50 {
		t.Fatalf("max_members = %v, want the new plan's 50", limits["max_members"])
	}
}

// TestIntegration_BillingWebhook_EffectiveLimitsResolveWithStripeUnreachable
// is plan invariant 3 in its strongest form: this server has no Stripe
// credentials at all, so every /billing route is 501 and no webhook can be
// verified — and entitlement resolution, the thing the MCP gateway is on
// the latency path of, must be completely unaffected.
func TestIntegration_BillingWebhook_EffectiveLimitsResolveWithStripeUnreachable(t *testing.T) {
	// No configure func: no STRIPE_SECRET_KEY, no STRIPE_WEBHOOK_SECRET.
	ts, cfg, store := setupTestServer(t)
	client := ts.Client()
	ctx := context.Background()

	if cfg.BillingEnabled() || cfg.WebhookEnabled() {
		t.Fatal("this test's premise is that Stripe is entirely unconfigured")
	}

	org := createOrgWithOwner(t, client, ts.URL, "webhook-invariant3")
	orgID := uuid.MustParse(org.ID)
	plan := createPlan(t, store, map[string]int{"max_members": 5, "max_tool_calls_per_month": 1000})
	assignPlanDirect(t, store, orgID, plan)
	setCustomLimits(t, store, orgID, `{"max_tool_calls_per_month": 50000}`)

	subSvc := subscription.NewService(store)

	limits, err := subSvc.EffectiveLimits(ctx, orgID)
	if err != nil {
		t.Fatalf("EffectiveLimits with Stripe unreachable: %v", err)
	}
	if limits["max_members"] != 5 {
		t.Fatalf("max_members = %v, want 5", limits["max_members"])
	}
	if limits["max_tool_calls_per_month"] != 50000 {
		t.Fatalf("max_tool_calls_per_month = %v, want the custom override 50000", limits["max_tool_calls_per_month"])
	}

	// And the enforcement path the gateway actually calls.
	if err := subSvc.EnforceLimit(ctx, orgID, "max_tool_calls_per_month", 10); err != nil {
		t.Fatalf("EnforceLimit under quota: %v", err)
	}
	if err := subSvc.EnforceLimit(ctx, orgID, "max_tool_calls_per_month", 50000); err == nil {
		t.Fatal("EnforceLimit at quota returned nil")
	}

	// Meanwhile the webhook itself degrades rather than failing open: an
	// unverifiable delivery is 501, never a silently accepted one.
	payload := subscriptionEventPayload("evt_"+uuid.NewString(), "customer.subscription.updated", time.Now(),
		"sub_x", "cus_x", "active", "price_x", orgID, plan, false)
	status, body := postWebhook(t, client, ts.URL, payload, signStripePayload(t, payload, testWebhookSecret, time.Now()))
	if status != http.StatusNotImplemented {
		t.Fatalf("webhook with no secret: status = %d, want 501; body = %v", status, body)
	}
	if body["message"] != "Billing is not configured" {
		t.Fatalf("message = %v", body["message"])
	}

	if after := readBillingRow(t, store, orgID); after.PlanID != plan || after.StripeEventAt != nil {
		t.Fatal("an unconfigured webhook changed state")
	}
}

// TestIntegration_BillingWebhook_TenantIsolation: one org's subscription
// must be unreachable through another org's identifiers.
//
// The resolution order is the mechanism — the recorded, uniqueness-enforced
// stripe_subscription_id / stripe_customer_id linkage (partial unique
// indexes, migration 00013) beats the organization_id in Stripe metadata —
// so an event carrying org A's Stripe ids lands on org A no matter what its
// metadata claims.
func TestIntegration_BillingWebhook_TenantIsolation(t *testing.T) {
	ts, _, store := setupTestServer(t, withWebhookSecret)
	client := ts.Client()

	orgA := createOrgWithOwner(t, client, ts.URL, "webhook-iso-a")
	orgB := createOrgWithOwner(t, client, ts.URL, "webhook-iso-b")
	orgAID, orgBID := uuid.MustParse(orgA.ID), uuid.MustParse(orgB.ID)

	basePlan := createPlan(t, store, map[string]int{"max_members": 5})
	richPlan := createPlan(t, store, map[string]int{"max_members": 500})
	richPrice := uniquePriceID()
	insertPlanPrice(t, store, richPlan, richPrice, 99000, "thb", "month", true)

	assignPlanDirect(t, store, orgAID, basePlan)
	assignPlanDirect(t, store, orgBID, basePlan)

	// Give org A a live subscription.
	subA, customerA := "sub_"+uuid.NewString(), "cus_"+uuid.NewString()
	deliverSigned(t, client, ts.URL, subscriptionEventPayload(
		"evt_"+uuid.NewString(), "customer.subscription.created", time.Now().Add(-time.Minute),
		subA, customerA, "active", richPrice, orgAID, richPlan, false))

	beforeB := readBillingRow(t, store, orgBID)

	// Now an event for org A's subscription whose metadata names org B.
	deliverSigned(t, client, ts.URL, subscriptionEventPayload(
		"evt_"+uuid.NewString(), "customer.subscription.updated", time.Now(),
		subA, customerA, "past_due", richPrice, orgBID, richPlan, false))

	afterA := readBillingRow(t, store, orgAID)
	if derefString(afterA.Status) != "past_due" {
		t.Fatalf("org A's status = %s, want past_due — the event did not land on the org that owns %s",
			derefString(afterA.Status), subA)
	}

	afterB := readBillingRow(t, store, orgBID)
	if afterB.PlanID != beforeB.PlanID || afterB.Status != nil || afterB.StripeSubscriptionID != nil {
		t.Fatalf("org B's row changed: %+v — metadata steered an event onto another tenant", afterB)
	}

	t.Run("org B cannot claim org A's Stripe identifiers", func(t *testing.T) {
		// The partial unique indexes from migration 00013 are what make
		// resolution single-valued in the first place.
		if _, err := store.Pool.Exec(context.Background(),
			`UPDATE org_subscriptions SET stripe_subscription_id = $2 WHERE organization_id = $1`,
			orgBID, subA); err == nil {
			t.Fatal("two organizations can point at one Stripe subscription; the unique index is not doing its job")
		}
	})
}

// TestIntegration_BillingWebhook_IPAllowlist covers the defence-in-depth
// layer the plan asks for ("also allowlist Stripe's published IPs on the
// webhook route — ADMIN_IP_ALLOWLIST is the existing pattern to copy").
//
// A 404, not a 403, for the same reason /admin gives one: a scanner learns
// nothing. For this route it has a second benefit — a non-2xx means a
// genuinely misdirected Stripe delivery is retried rather than dropped.
func TestIntegration_BillingWebhook_IPAllowlist(t *testing.T) {
	payloadFor := func(orgID, planID uuid.UUID) []byte {
		return subscriptionEventPayload("evt_"+uuid.NewString(), "customer.subscription.updated", time.Now(),
			"sub_"+uuid.NewString(), "cus_"+uuid.NewString(), "active", "price_x", orgID, planID, false)
	}

	t.Run("off-list callers get a bare 404 before verification", func(t *testing.T) {
		ts, _, _ := setupTestServer(t, withWebhookSecret, func(cfg *config.Config) {
			// 10.0.0.0/8 never matches httptest's loopback peer.
			cfg.StripeWebhookIPAllowlist = []*net.IPNet{mustParseCIDR(t, "10.0.0.0/8")}
		})
		payload := payloadFor(uuid.New(), uuid.New())
		// A VALID signature, so a 404 can only be the allowlist.
		status, body := postWebhook(t, ts.Client(), ts.URL, payload,
			signStripePayload(t, payload, testWebhookSecret, time.Now()))
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", status, body)
		}
		if body["message"] != "Route not found" {
			t.Fatalf("message = %v, want %q", body["message"], "Route not found")
		}
	})

	t.Run("on-list callers reach verification", func(t *testing.T) {
		ts, _, _ := setupTestServer(t, withWebhookSecret, func(cfg *config.Config) {
			cfg.StripeWebhookIPAllowlist = []*net.IPNet{mustParseCIDR(t, "127.0.0.0/8")}
		})
		payload := payloadFor(uuid.New(), uuid.New())
		// Deliberately unsigned: 400 (not 404) proves the request got past
		// the allowlist and into the signature check.
		status, body := postWebhook(t, ts.Client(), ts.URL, payload, "")
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", status, body)
		}
	})

	t.Run("unset disables the check", func(t *testing.T) {
		ts, _, _ := setupTestServer(t, withWebhookSecret)
		payload := payloadFor(uuid.New(), uuid.New())
		status, _ := postWebhook(t, ts.Client(), ts.URL, payload, "")
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (no allowlist means every caller reaches verification)", status)
		}
	})
}

// TestIntegration_BillingWebhook_RouteSurface pins what step 10 writes into
// docs/02-api-contract.md, and the two structural properties of the mount:
// only POST exists, and it carries no auth guard.
func TestIntegration_BillingWebhook_RouteSurface(t *testing.T) {
	ts, _, _ := setupTestServer(t, withWebhookSecret)
	client := ts.Client()

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, ts.URL+webhookPath, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s %s returned 200; only POST should be mounted", method, webhookPath)
		}
	}

	// No Authorization header, no x-organization-id: a 400 (bad signature)
	// rather than a 401/403 is the proof that no auth guard sits in front.
	// Stripe presents neither, so a guarded webhook could never be reached.
	status, body := postWebhook(t, client, ts.URL, []byte(`{}`), "t=1,v1=00")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — an auth guard is in front of the webhook; body = %v", status, body)
	}
}
