package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/billing"
)

// The /billing integration tests run against the real server, the real RBAC
// engine, and real Postgres — but never against Stripe. setupTestServer
// builds a config with no StripeSecretKey, so billing.NewStripeClient
// returns nil and every route that gets past its guard answers 501
// BILLING_NOT_CONFIGURED. That is exactly the property these tests need:
// the guard and tenant-isolation behaviour is entirely upstream of the
// Stripe call, so it can be exercised end-to-end with no network and no
// credentials, and the Stripe-side behaviour is covered by the hand-mocked
// unit tests in internal/module/billing.
//
// It also means these tests double as the proof that a developer (and CI)
// with no Stripe account can boot the API and run the whole suite.

const (
	billingCheckoutPath = "/billing/checkout"
	billingPortalPath   = "/billing/portal"
)

// grantRole creates a role carrying permissions and assigns it to userID,
// using org's owner as the caller — the same two /rbac routes the dashboard
// uses, rather than writing role rows directly.
func grantRole(t *testing.T, client *http.Client, baseURL string, org createdOrg, userID string, permissions []string) {
	t.Helper()

	ownerHeaders := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/rbac/roles",
		map[string]any{"name": uniqueSlug("billing-role"), "permissions": permissions}, ownerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create role: status = %d, body = %v", resp.StatusCode, body)
	}
	roleID, _ := body["id"].(string)
	if roleID == "" {
		t.Fatalf("create role: missing id: %v", body)
	}

	resp, body = doJSON(t, client, baseURL, http.MethodPost, "/rbac/assign",
		map[string]any{"userId": userID, "roleId": roleID}, ownerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assign role: status = %d, body = %v", resp.StatusCode, body)
	}
}

// TestIntegration_Billing_GuardsRejectAPlainMember is the regression test for
// the deleted POST /subscription/assign (plan invariant 4).
//
// That route sat on RequireOrg, which is membership-only, so any `member`
// could move their own org onto any plan and lift max_members / max_roles /
// max_connectors on themselves. Both /billing routes take
// RequirePermission("billing:write") instead — the Portal included, since it
// can cancel the subscription and surfaces invoices carrying a billing
// address, and so must never get the weaker guard.
//
// A 403 here (rather than a 501) is also the proof that the guard runs
// BEFORE the billing-not-configured check: an unauthorized caller learns
// nothing about whether this deployment has Stripe wired up.
func TestIntegration_Billing_GuardsRejectAPlainMember(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "billing-guard")
	member := registerUser(t, client, ts.URL, "billing-guard-member")
	inviteMember(t, client, ts.URL, org, member.Email, "member")

	memberHeaders := map[string]string{
		"Authorization":     "Bearer " + member.AccessToken,
		"x-organization-id": org.ID,
	}

	for _, path := range []string{billingCheckoutPath, billingPortalPath} {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, path,
			map[string]any{"planId": uuid.NewString()}, memberHeaders)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as a plain member: status = %d, want 403; body = %v", path, resp.StatusCode, body)
		}
		if body["message"] != "Missing permission: billing:write" {
			t.Fatalf("%s: message = %v, want %q", path, body["message"], "Missing permission: billing:write")
		}
	}

	t.Run("a role granting only billing:read is still denied", func(t *testing.T) {
		reader := registerUser(t, client, ts.URL, "billing-guard-reader")
		inviteMember(t, client, ts.URL, org, reader.Email, "member")
		grantRole(t, client, ts.URL, org, reader.UserID, []string{"billing:read"})

		readerHeaders := map[string]string{
			"Authorization":     "Bearer " + reader.AccessToken,
			"x-organization-id": org.ID,
		}
		for _, path := range []string{billingCheckoutPath, billingPortalPath} {
			resp, body := doJSON(t, client, ts.URL, http.MethodPost, path,
				map[string]any{"planId": uuid.NewString()}, readerHeaders)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s with billing:read only: status = %d, want 403; body = %v", path, resp.StatusCode, body)
			}
		}
	})

	t.Run("billing:write granted to a non-owner passes the guard", func(t *testing.T) {
		// The point of decision 2's "not owner-only": a customer must be
		// able to delegate billing to a finance person without making them
		// an org owner. Past the guard, the request reaches the service and
		// stops at BILLING_NOT_CONFIGURED (no Stripe key in this suite) —
		// which is precisely the "the guard let it through" signal.
		finance := registerUser(t, client, ts.URL, "billing-guard-finance")
		inviteMember(t, client, ts.URL, org, finance.Email, "member")
		grantRole(t, client, ts.URL, org, finance.UserID, []string{"billing:write"})

		financeHeaders := map[string]string{
			"Authorization":     "Bearer " + finance.AccessToken,
			"x-organization-id": org.ID,
		}
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, billingPortalPath, nil, financeHeaders)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("portal with billing:write: status = %d, want 501 (past the guard); body = %v", resp.StatusCode, body)
		}
	})

	t.Run("the owner bypasses RBAC and needs no explicit grant", func(t *testing.T) {
		ownerHeaders := map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		}
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, billingPortalPath, nil, ownerHeaders)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("portal as owner: status = %d, want 501 (past the guard); body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Billing is not configured" {
			t.Fatalf("message = %v, want %q", body["message"], "Billing is not configured")
		}
	})
}

// TestIntegration_Billing_TenantIsolation covers the class SECURITY.md names
// first: one org's billing surface must be unreachable from another org's
// session.
//
// The mechanism under test is that the organization is taken from the
// guard-verified x-organization-id header, never from the request body —
// there is simply no input through which a caller can name an org they are
// not a member of, so a cross-tenant checkout or portal link is
// unrepresentable rather than merely rejected.
func TestIntegration_Billing_TenantIsolation(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	orgA := createOrgWithOwner(t, client, ts.URL, "billing-iso-a")
	orgB := createOrgWithOwner(t, client, ts.URL, "billing-iso-b")

	t.Run("org B's owner cannot act on org A", func(t *testing.T) {
		crossHeaders := map[string]string{
			"Authorization":     "Bearer " + orgB.Owner.AccessToken,
			"x-organization-id": orgA.ID, // another tenant's org
		}
		for _, path := range []string{billingCheckoutPath, billingPortalPath} {
			resp, body := doJSON(t, client, ts.URL, http.MethodPost, path,
				map[string]any{"planId": uuid.NewString()}, crossHeaders)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s across tenants: status = %d, want 403; body = %v", path, resp.StatusCode, body)
			}
			if body["message"] != "Missing permission: billing:write" {
				t.Fatalf("%s: message = %v", path, body["message"])
			}
		}
	})

	t.Run("org A's recorded Stripe customer stays with org A", func(t *testing.T) {
		// A customer id recorded against org A must not become reachable by
		// org B. ClaimOrgStripeCustomer is scoped by organization_id and the
		// partial unique index on stripe_customer_id (migration 00013) makes
		// the id itself unique across orgs, so a second org can never end up
		// pointing at the same Stripe Customer.
		ctx := context.Background()
		planID := createPlan(t, store, map[string]int{"max_members": 5})
		orgAID := uuid.MustParse(orgA.ID)
		orgBID := uuid.MustParse(orgB.ID)
		assignPlanDirect(t, store, orgAID, planID)
		assignPlanDirect(t, store, orgBID, planID)

		customerA := "cus_iso_" + uuid.NewString()
		claimed, err := store.ClaimOrgStripeCustomer(ctx, db.ClaimOrgStripeCustomerParams{
			OrganizationID: orgAID, StripeCustomerID: customerA,
		})
		if err != nil {
			t.Fatalf("claim for org A: %v", err)
		}
		if claimed == nil || *claimed != customerA {
			t.Fatalf("claim returned %v, want %q", claimed, customerA)
		}

		refB, err := store.GetOrgBillingRef(ctx, orgBID)
		if err != nil {
			t.Fatalf("GetOrgBillingRef(orgB): %v", err)
		}
		if refB.StripeCustomerID != nil {
			t.Fatalf("org B's row reports customer %q; org A's claim leaked across tenants", *refB.StripeCustomerID)
		}

		// The same id cannot be claimed by a second org: the partial unique
		// index rejects it, so two tenants can never share a Customer.
		if _, err := store.ClaimOrgStripeCustomer(ctx, db.ClaimOrgStripeCustomerParams{
			OrganizationID: orgBID, StripeCustomerID: customerA,
		}); err == nil {
			t.Fatal("org B successfully claimed org A's Stripe customer id; the unique index is not doing its job")
		}
	})
}

// TestIntegration_Billing_LazyCustomerClaimIsIdempotent exercises the
// Postgres half of the no-duplicate-Customer guarantee directly against the
// database, since the interleaving it protects against cannot be produced
// through the HTTP surface with no Stripe key configured.
//
// The claim is a conditional UPDATE ("WHERE stripe_customer_id IS NULL"), so
// of two writers exactly one matches a row; the loser gets pgx.ErrNoRows and
// re-reads the winner's id rather than overwriting it.
func TestIntegration_Billing_LazyCustomerClaimIsIdempotent(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()
	ctx := context.Background()

	org := createOrgWithOwner(t, client, ts.URL, "billing-claim")
	orgID := uuid.MustParse(org.ID)
	planID := createPlan(t, store, map[string]int{"max_members": 5})
	assignPlanDirect(t, store, orgID, planID)

	ref, err := store.GetOrgBillingRef(ctx, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingRef: %v", err)
	}
	if ref.StripeCustomerID != nil {
		t.Fatalf("a freshly assigned subscription already has a customer: %q", *ref.StripeCustomerID)
	}
	// Assigning a plan must not invent a Stripe Subscription either —
	// "stripe_subscription_id IS NULL" is the canonical "not paying" signal
	// (decision 4).
	if ref.StripeSubscriptionID != nil {
		t.Fatalf("stripe_subscription_id = %q on a non-paying org", *ref.StripeSubscriptionID)
	}

	winner := "cus_first_" + uuid.NewString()
	claimed, err := store.ClaimOrgStripeCustomer(ctx, db.ClaimOrgStripeCustomerParams{
		OrganizationID: orgID, StripeCustomerID: winner,
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if claimed == nil || *claimed != winner {
		t.Fatalf("first claim returned %v, want %q", claimed, winner)
	}

	// A second claim — the racing request — matches no row and must not
	// overwrite the recorded id.
	loser := "cus_second_" + uuid.NewString()
	if _, err := store.ClaimOrgStripeCustomer(ctx, db.ClaimOrgStripeCustomerParams{
		OrganizationID: orgID, StripeCustomerID: loser,
	}); err == nil {
		t.Fatal("a second claim succeeded; the conditional UPDATE is not conditional")
	}

	after, err := store.GetOrgBillingRef(ctx, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingRef after: %v", err)
	}
	if after.StripeCustomerID == nil || *after.StripeCustomerID != winner {
		t.Fatalf("recorded customer = %v, want the first claim %q", after.StripeCustomerID, winner)
	}

	t.Run("the claim leaves entitlements untouched", func(t *testing.T) {
		// Invariant 2 and invariant 5 together: billing's one write touches
		// stripe_customer_id and nothing else. The plan the org is entitled
		// to must be exactly what it was before.
		sub, err := store.GetOrgSubscription(ctx, orgID)
		if err != nil {
			t.Fatalf("GetOrgSubscription: %v", err)
		}
		if sub.PlanID != planID {
			t.Fatalf("plan_id = %s, want %s — the customer claim moved the org's plan", sub.PlanID, planID)
		}
	})
}

// TestIntegration_Billing_NoSubscriptionRowIsNotCreatedByBilling pins the
// deliberate choice behind ensureCustomer: an org that has never been
// assigned a plan has no org_subscriptions row at all, and resolves to
// "unlimited" through subscription.Service.EffectiveLimits. Billing must not
// insert a row for it — doing so would quietly tighten that org's limits as
// a side effect of a billing click. Creating the row is AssignPlan's job, at
// the moment a subscription actually starts (invariant 5, step 7).
func TestIntegration_Billing_NoSubscriptionRowIsNotCreatedByBilling(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()
	ctx := context.Background()

	org := createOrgWithOwner(t, client, ts.URL, "billing-norow")
	orgID := uuid.MustParse(org.ID)

	if _, err := store.GetOrgBillingRef(ctx, orgID); err == nil {
		t.Fatal("a brand-new org already has an org_subscriptions row; this test's premise is stale")
	}

	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}
	resp, body := doJSON(t, client, ts.URL, http.MethodPost, billingPortalPath, nil, headers)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("portal: status = %d, want 501; body = %v", resp.StatusCode, body)
	}

	if _, err := store.GetOrgBillingRef(ctx, orgID); err == nil {
		t.Fatal("a /billing request created an org_subscriptions row; entitlements must only move through AssignPlan")
	}
}

// TestIntegration_Billing_CheckoutValidation covers the request contract at
// the HTTP layer: the routes bind and validate before anything else the
// caller can observe.
func TestIntegration_Billing_CheckoutValidation(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "billing-validate")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	cases := map[string]struct {
		body       any
		wantStatus int
		wantMsg    string
	}{
		"missing planId":       {map[string]any{}, http.StatusUnprocessableEntity, "Validation failed"},
		"malformed planId":     {map[string]any{"planId": "nope"}, http.StatusUnprocessableEntity, "Validation failed"},
		"unsupported interval": {map[string]any{"planId": uuid.NewString(), "interval": "weekly"}, http.StatusUnprocessableEntity, "Validation failed"},
		"malformed JSON":       {"{", http.StatusBadRequest, "Invalid request body"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp, body := doJSON(t, client, ts.URL, http.MethodPost, billingCheckoutPath, tc.body, headers)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %v", resp.StatusCode, tc.wantStatus, body)
			}
			if body["message"] != tc.wantMsg {
				t.Fatalf("message = %v, want %q", body["message"], tc.wantMsg)
			}
		})
	}

	t.Run("a well-formed request reaches the service", func(t *testing.T) {
		// 501 rather than 422/400: validation passed and the service was
		// entered. With a Stripe key configured this is where the plan
		// lookup would take over.
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, billingCheckoutPath,
			map[string]any{"planId": uuid.NewString(), "interval": "year"}, headers)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501; body = %v", resp.StatusCode, body)
		}
	})
}

// TestIntegration_Billing_PlanPricesAreQueryableEndToEnd exercises the two
// catalogue queries this module added against real Postgres, since the unit
// tests mock them. A price for a plan must resolve only for the exact
// currency/interval pair asked for.
func TestIntegration_Billing_PlanPricesAreQueryableEndToEnd(t *testing.T) {
	_, _, store := setupTestServer(t)
	ctx := context.Background()

	planID := createPlan(t, store, map[string]int{"max_members": 5})
	priceID := "price_" + uuid.NewString()
	insertPlanPrice(t, store, planID, priceID, 99000, "thb", "month", true)

	got, err := store.GetActivePlanPrice(ctx, db.GetActivePlanPriceParams{
		PlanID: planID, Currency: "thb", BillingInterval: "month",
	})
	if err != nil {
		t.Fatalf("GetActivePlanPrice: %v", err)
	}
	if got.StripePriceID != priceID {
		t.Fatalf("stripe_price_id = %q, want %q", got.StripePriceID, priceID)
	}

	for name, arg := range map[string]db.GetActivePlanPriceParams{
		"another currency": {PlanID: planID, Currency: "usd", BillingInterval: "month"},
		"another interval": {PlanID: planID, Currency: "thb", BillingInterval: "year"},
		"another plan":     {PlanID: uuid.New(), Currency: "thb", BillingInterval: "month"},
	} {
		t.Run(name+" resolves to no row", func(t *testing.T) {
			if _, err := store.GetActivePlanPrice(ctx, arg); err == nil {
				t.Fatal("expected no rows")
			}
		})
	}

	t.Run("a deactivated price stops being selectable", func(t *testing.T) {
		deactivated := "price_" + uuid.NewString()
		otherPlan := createPlan(t, store, map[string]int{"max_members": 5})
		insertPlanPrice(t, store, otherPlan, deactivated, 50000, "thb", "month", false)
		if _, err := store.GetActivePlanPrice(ctx, db.GetActivePlanPriceParams{
			PlanID: otherPlan, Currency: "thb", BillingInterval: "month",
		}); err == nil {
			t.Fatal("an inactive price was selected for checkout")
		}
	})

	t.Run("GetPlanByID round-trips the step 1 columns", func(t *testing.T) {
		plan, err := store.GetPlanByID(ctx, planID)
		if err != nil {
			t.Fatalf("GetPlanByID: %v", err)
		}
		if !plan.IsPublic {
			t.Fatal("is_public defaulted to false; a seeded plan would be unbuyable")
		}
		if _, err := store.GetPlanByID(ctx, uuid.New()); err == nil {
			t.Fatal("GetPlanByID returned a row for a nonexistent id")
		}
	})
}

// insertPlanPrice writes a plan_prices row directly. There is no API route
// that creates one — admin plan-price CRUD is step 8 of the billing plan —
// so this is raw SQL by necessity rather than by preference.
func insertPlanPrice(t *testing.T, store *database.Store, planID uuid.UUID, stripePriceID string, unitAmount int64, currency, interval string, active bool) {
	t.Helper()

	_, err := store.Pool.Exec(context.Background(),
		`INSERT INTO plan_prices (plan_id, stripe_price_id, unit_amount, currency, "interval", active)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		planID, stripePriceID, unitAmount, currency, interval, active)
	if err != nil {
		t.Fatalf("insert plan_price: %v", err)
	}
}

// TestIntegration_Billing_RouteSurface pins the exact route surface step 10
// has to write into docs/02-api-contract.md, so a renamed or added route
// breaks a test rather than silently drifting from the contract.
func TestIntegration_Billing_RouteSurface(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "billing-routes")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	// Only POST is mounted on either path; GET must 404/405, never succeed.
	for _, path := range []string{billingCheckoutPath, billingPortalPath} {
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("GET %s returned 200; only POST should be mounted", path)
		}
	}

	// POST /billing/webhook exists (step 7) but is deliberately NOT on this
	// guarded group: Stripe presents no JWT and no x-organization-id, so a
	// webhook behind RequirePermission could never be reached by Stripe at
	// all. Sending it a fully authenticated request proves the negative —
	// it is answered on its own terms (501 here, since this suite
	// configures no STRIPE_WEBHOOK_SECRET), never 403 for a missing
	// billing:write and never 400 for a missing x-organization-id.
	// billing_webhook_integration_test.go covers the route itself.
	resp, body := doJSON(t, client, ts.URL, http.MethodPost, billing.WebhookPath, map[string]any{}, headers)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("POST /billing/webhook: status = %d, want 501; body = %v", resp.StatusCode, body)
	}

	// And it is reachable with no credentials at all, which is the whole
	// point of mounting it outside the group.
	resp, body = doJSON(t, client, ts.URL, http.MethodPost, billing.WebhookPath, map[string]any{}, nil)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("POST /billing/webhook unauthenticated: status = %d — an auth guard is in front of it; body = %v",
			resp.StatusCode, body)
	}
}

// ---- GET /billing/usage (billing plan step 9) ----

// TestIntegration_Billing_Usage_ReadPermissionSucceeds is the mirror of
// TestIntegration_Billing_GuardsRejectAPlainMember's "a role granting only
// billing:read is still denied" sub-test: billing:read is exactly the
// permission this ONE route needs, so a caller holding only it must
// succeed here, not just fail elsewhere. Unlike checkout/portal, this
// route needs no Stripe key at all (plan invariant 1 — it answers from
// Postgres alone), so 200 is the actual happy path, not a "past the guard"
// 501 stand-in.
func TestIntegration_Billing_Usage_ReadPermissionSucceeds(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "billing-usage-read")
	planID := createPlan(t, store, map[string]int{"max_tool_calls_per_month": 100})
	assignPlanDirect(t, store, uuid.MustParse(org.ID), planID)

	reader := registerUser(t, client, ts.URL, "billing-usage-reader")
	inviteMember(t, client, ts.URL, org, reader.Email, "member")
	grantRole(t, client, ts.URL, org, reader.UserID, []string{billing.PermissionRead})

	readerHeaders := map[string]string{
		"Authorization":     "Bearer " + reader.AccessToken,
		"x-organization-id": org.ID,
	}

	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/billing/usage", nil, readerHeaders)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	if body["callCount"] != 0.0 {
		t.Errorf("callCount = %v, want 0 (no usage recorded yet)", body["callCount"])
	}
	if body["limit"] != 100.0 {
		t.Errorf("limit = %v, want 100 (from the assigned plan)", body["limit"])
	}
	if _, ok := body["periodStart"]; !ok {
		t.Errorf("missing periodStart: %v", body)
	}
	if _, ok := body["periodEnd"]; !ok {
		t.Errorf("missing periodEnd: %v", body)
	}
	if byTool, ok := body["byTool"].([]any); !ok || len(byTool) != 0 {
		t.Errorf("byTool = %v, want an empty array", body["byTool"])
	}
}

// TestIntegration_Billing_Usage_GuardsRejectEveryoneElse is
// TestIntegration_Billing_GuardsRejectAPlainMember's shape, applied to the
// read route: a plain member is denied, billing:write alone does not imply
// billing:read (the two guard different routes for different reasons — see
// PermissionRead's doc comment), and the owner bypasses RBAC as always.
func TestIntegration_Billing_Usage_GuardsRejectEveryoneElse(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "billing-usage-denied")
	member := registerUser(t, client, ts.URL, "billing-usage-member")
	inviteMember(t, client, ts.URL, org, member.Email, "member")

	memberHeaders := map[string]string{
		"Authorization":     "Bearer " + member.AccessToken,
		"x-organization-id": org.ID,
	}
	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/billing/usage", nil, memberHeaders)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("plain member: status = %d, want 403; body = %v", resp.StatusCode, body)
	}
	if body["message"] != "Missing permission: "+billing.PermissionRead {
		t.Fatalf("message = %v, want %q", body["message"], "Missing permission: "+billing.PermissionRead)
	}

	t.Run("billing:write alone does not imply billing:read", func(t *testing.T) {
		writer := registerUser(t, client, ts.URL, "billing-usage-writer")
		inviteMember(t, client, ts.URL, org, writer.Email, "member")
		grantRole(t, client, ts.URL, org, writer.UserID, []string{billing.PermissionWrite})

		writerHeaders := map[string]string{
			"Authorization":     "Bearer " + writer.AccessToken,
			"x-organization-id": org.ID,
		}
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/billing/usage", nil, writerHeaders)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("billing:write only: status = %d, want 403; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("the owner bypasses RBAC and needs no explicit grant", func(t *testing.T) {
		ownerHeaders := map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		}
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/billing/usage", nil, ownerHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("owner: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
	})
}

// TestIntegration_Billing_Usage_TenantIsolation is the class SECURITY.md
// names first, restated for the usage ledger: org A's usage_events must
// never be counted into org B's GET /billing/usage response, even though
// both are read from the exact same table with organization_id as the only
// boundary between them.
func TestIntegration_Billing_Usage_TenantIsolation(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()
	ctx := context.Background()

	orgA := createOrgWithOwner(t, client, ts.URL, "billing-usage-iso-a")
	orgB := createOrgWithOwner(t, client, ts.URL, "billing-usage-iso-b")
	orgAID := uuid.MustParse(orgA.ID)

	// Three usage events for org A only.
	for i := 0; i < 3; i++ {
		if err := store.CreateUsageEvent(ctx, db.CreateUsageEventParams{
			OrganizationID: orgAID,
			Tool:           "sheets_query_rows",
		}); err != nil {
			t.Fatalf("CreateUsageEvent: %v", err)
		}
	}

	headersFor := func(org createdOrg) map[string]string {
		return map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		}
	}

	respA, bodyA := doJSON(t, client, ts.URL, http.MethodGet, "/billing/usage", nil, headersFor(orgA))
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("org A: status = %d, want 200; body = %v", respA.StatusCode, bodyA)
	}
	if bodyA["callCount"] != 3.0 {
		t.Fatalf("org A callCount = %v, want 3", bodyA["callCount"])
	}

	respB, bodyB := doJSON(t, client, ts.URL, http.MethodGet, "/billing/usage", nil, headersFor(orgB))
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("org B: status = %d, want 200; body = %v", respB.StatusCode, bodyB)
	}
	if bodyB["callCount"] != 0.0 {
		t.Fatalf("org B callCount = %v, want 0 — org A's usage events leaked across tenants", bodyB["callCount"])
	}

	// And org B's owner cannot read org A's usage by naming org A's id
	// with its own (org-B-only) credentials — mirrors
	// TestIntegration_Billing_TenantIsolation's cross-tenant sub-test.
	crossHeaders := map[string]string{
		"Authorization":     "Bearer " + orgB.Owner.AccessToken,
		"x-organization-id": orgA.ID,
	}
	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/billing/usage", nil, crossHeaders)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant usage read: status = %d, want 403; body = %v", resp.StatusCode, body)
	}
}
