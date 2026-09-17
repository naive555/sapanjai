package billing

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// ---- hand-mocked billingStore ----

type mockBillingStore struct {
	getPlanByID            func(ctx context.Context, id uuid.UUID) (db.Plan, error)
	getActivePlanPrice     func(ctx context.Context, arg db.GetActivePlanPriceParams) (db.PlanPrice, error)
	getOrgBillingRef       func(ctx context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error)
	claimOrgStripeCustomer func(ctx context.Context, arg db.ClaimOrgStripeCustomerParams) (*string, error)
	getOrganizationByID    func(ctx context.Context, id uuid.UUID) (db.Organization, error)

	// withTx backs the webhook's one transaction (webhook_test.go). Left
	// nil by the Checkout/Portal tests, which never reach it — a nil field
	// panics loudly rather than passing quietly if that ever changes.
	withTx func(ctx context.Context, fn func(q db.Querier) error) error

	// countUsageEventsForOrgSince/listUsageRollupsForOrgPeriod back
	// GET /billing/usage (usage.go, usage_test.go). Left nil by every test
	// in this file, none of which call Usage — the usage tests build their
	// own store literal instead of going through this shared mock, since
	// they need neither the Checkout/Portal fields above nor withTx.
	countUsageEventsForOrgSince  func(ctx context.Context, arg db.CountUsageEventsForOrgSinceParams) (int64, error)
	listUsageRollupsForOrgPeriod func(ctx context.Context, arg db.ListUsageRollupsForOrgPeriodParams) ([]db.ListUsageRollupsForOrgPeriodRow, error)
}

func (m *mockBillingStore) GetPlanByID(ctx context.Context, id uuid.UUID) (db.Plan, error) {
	return m.getPlanByID(ctx, id)
}
func (m *mockBillingStore) GetActivePlanPrice(ctx context.Context, arg db.GetActivePlanPriceParams) (db.PlanPrice, error) {
	return m.getActivePlanPrice(ctx, arg)
}
func (m *mockBillingStore) GetOrgBillingRef(ctx context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error) {
	return m.getOrgBillingRef(ctx, organizationID)
}
func (m *mockBillingStore) ClaimOrgStripeCustomer(ctx context.Context, arg db.ClaimOrgStripeCustomerParams) (*string, error) {
	return m.claimOrgStripeCustomer(ctx, arg)
}
func (m *mockBillingStore) GetOrganizationByID(ctx context.Context, id uuid.UUID) (db.Organization, error) {
	return m.getOrganizationByID(ctx, id)
}
func (m *mockBillingStore) WithTx(ctx context.Context, fn func(q db.Querier) error) error {
	return m.withTx(ctx, fn)
}
func (m *mockBillingStore) CountUsageEventsForOrgSince(ctx context.Context, arg db.CountUsageEventsForOrgSinceParams) (int64, error) {
	return m.countUsageEventsForOrgSince(ctx, arg)
}
func (m *mockBillingStore) ListUsageRollupsForOrgPeriod(ctx context.Context, arg db.ListUsageRollupsForOrgPeriodParams) ([]db.ListUsageRollupsForOrgPeriodRow, error) {
	return m.listUsageRollupsForOrgPeriod(ctx, arg)
}

var _ billingStore = (*mockBillingStore)(nil)

// ---- hand-mocked stripeAPI ----
//
// No test in this package may reach the network. Every Stripe call goes
// through this mock; the real adapter (stripeClient in stripe.go) is
// constructed only by NewStripeClient, which nothing here calls.
type mockStripe struct {
	mu sync.Mutex

	customerCalls []customerInput
	checkoutCalls []checkoutInput
	portalCalls   []portalInput

	// nextCustomerID, when set, overrides the default "cus_<n>" so a test
	// can assert a specific id flows through. createErr/checkoutErr/
	// portalErr force the upstream-failure path.
	nextCustomerID string
	createErr      error
	checkoutErr    error
	portalErr      error
}

func (m *mockStripe) CreateCustomer(_ context.Context, in customerInput) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customerCalls = append(m.customerCalls, in)
	if m.createErr != nil {
		return "", m.createErr
	}
	if m.nextCustomerID != "" {
		return m.nextCustomerID, nil
	}
	return "cus_created", nil
}

func (m *mockStripe) CreateCheckoutSession(_ context.Context, in checkoutInput) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkoutCalls = append(m.checkoutCalls, in)
	if m.checkoutErr != nil {
		return "", m.checkoutErr
	}
	return "https://checkout.stripe.com/c/pay/cs_test_123", nil
}

func (m *mockStripe) CreatePortalSession(_ context.Context, in portalInput) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.portalCalls = append(m.portalCalls, in)
	if m.portalErr != nil {
		return "", m.portalErr
	}
	return "https://billing.stripe.com/p/session/bps_test_123", nil
}

var _ stripeAPI = (*mockStripe)(nil)

// ---- helpers ----

func newTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newTestAudit builds an auditlog.Service over a store whose CreateAuditLog
// always fails, proving along the way that a failed audit write never fails
// a billing request (CLAUDE.md: audit writes are best-effort).
func newTestAudit() *auditlog.Service {
	return auditlog.NewService(failingAuditQuerier{}, newTestLog())
}

type failingAuditQuerier struct{ db.Querier }

func (failingAuditQuerier) CreateAuditLog(context.Context, db.CreateAuditLogParams) error {
	return errors.New("audit store is down")
}

func appErrorCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperror.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

const testPublicURL = "https://app.example.com"

// planFixture is a public plan with one active THB monthly price.
func planFixture() (db.Plan, db.PlanPrice) {
	planID := uuid.New()
	return db.Plan{ID: planID, Name: "pro", IsPublic: true},
		db.PlanPrice{
			ID: uuid.New(), PlanID: planID, StripePriceID: "price_thb_pro_monthly",
			UnitAmount: 99000, Currency: defaultCurrency, Interval: intervalMonth, Active: true,
		}
}

// newCatalogueStore is a store that serves plan/price lookups and an
// organization, with subscription-row behaviour supplied per test.
func newCatalogueStore(plan db.Plan, price db.PlanPrice) *mockBillingStore {
	return &mockBillingStore{
		getPlanByID: func(_ context.Context, id uuid.UUID) (db.Plan, error) {
			if id != plan.ID {
				return db.Plan{}, pgx.ErrNoRows
			}
			return plan, nil
		},
		getActivePlanPrice: func(_ context.Context, arg db.GetActivePlanPriceParams) (db.PlanPrice, error) {
			if arg.PlanID != price.PlanID || arg.Currency != price.Currency || arg.BillingInterval != price.Interval {
				return db.PlanPrice{}, pgx.ErrNoRows
			}
			return price, nil
		},
		getOrganizationByID: func(_ context.Context, id uuid.UUID) (db.Organization, error) {
			return db.Organization{ID: id, Name: "Acme Co", Slug: "acme"}, nil
		},
	}
}

// withExistingCustomer makes the store report an org_subscriptions row that
// already records customerID.
func withExistingCustomer(store *mockBillingStore, customerID string) *mockBillingStore {
	store.getOrgBillingRef = func(_ context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error) {
		id := customerID
		return db.GetOrgBillingRefRow{OrganizationID: organizationID, StripeCustomerID: &id}, nil
	}
	return store
}

// withClaimableRow makes the store report an org_subscriptions row with no
// customer recorded yet, and accept the claim.
func withClaimableRow(store *mockBillingStore, claimed *[]db.ClaimOrgStripeCustomerParams) *mockBillingStore {
	store.getOrgBillingRef = func(_ context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error) {
		return db.GetOrgBillingRefRow{OrganizationID: organizationID}, nil
	}
	store.claimOrgStripeCustomer = func(_ context.Context, arg db.ClaimOrgStripeCustomerParams) (*string, error) {
		if claimed != nil {
			*claimed = append(*claimed, arg)
		}
		id := arg.StripeCustomerID
		return &id, nil
	}
	return store
}

// newService builds a Service for the Checkout/Portal tests: no webhook
// verifier, no plan assigner, and no limit resolver, because none of the
// three routes exercised through this helper touch any of them. The
// webhook tests build their own (newWebhookService, webhook_test.go); the
// usage tests build their own too (usage_test.go), with a real
// limitResolver.
func newService(store billingStore, sc stripeAPI) *Service {
	return NewService(store, sc, nil, nil, nil, newTestAudit(), testPublicURL, newTestLog())
}

// ---- Checkout ----

func TestCheckout_HappyPath(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
	sc := &mockStripe{}
	svc := newService(store, sc)

	orgID, actorID := uuid.New(), uuid.New()
	url, err := svc.Checkout(context.Background(), orgID, actorID, "finance@acme.example", plan.ID.String(), "")
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if url != "https://checkout.stripe.com/c/pay/cs_test_123" {
		t.Fatalf("url = %q, want the hosted checkout URL", url)
	}
	if len(sc.checkoutCalls) != 1 {
		t.Fatalf("CreateCheckoutSession called %d times, want 1", len(sc.checkoutCalls))
	}

	got := sc.checkoutCalls[0]
	if got.PriceID != price.StripePriceID {
		t.Errorf("PriceID = %q, want %q", got.PriceID, price.StripePriceID)
	}
	if got.CustomerID != "cus_existing" {
		t.Errorf("CustomerID = %q, want the org's recorded customer", got.CustomerID)
	}
	if got.ClientReferenceID != orgID.String() {
		t.Errorf("ClientReferenceID = %q, want the org id %q", got.ClientReferenceID, orgID)
	}
	if got.Metadata[metadataOrganizationID] != orgID.String() {
		t.Errorf("session metadata %s = %q, want %q", metadataOrganizationID, got.Metadata[metadataOrganizationID], orgID)
	}
	if got.Metadata[metadataPlanID] != plan.ID.String() {
		t.Errorf("session metadata %s = %q, want %q", metadataPlanID, got.Metadata[metadataPlanID], plan.ID)
	}
	// The Subscription outlives the Session, and every renewal/cancellation
	// event months later references only the Subscription — so the org id
	// has to be on that object too or step 7 cannot reconcile them.
	if got.SubscriptionMetadata[metadataOrganizationID] != orgID.String() {
		t.Errorf("subscription metadata %s = %q, want %q",
			metadataOrganizationID, got.SubscriptionMetadata[metadataOrganizationID], orgID)
	}
	if got.SubscriptionMetadata[metadataPlanID] != plan.ID.String() {
		t.Errorf("subscription metadata %s = %q, want %q",
			metadataPlanID, got.SubscriptionMetadata[metadataPlanID], plan.ID)
	}

	// Redirects must land on the FRONTEND origin (config.AppPublicURL), not
	// this API — the API's address is routinely unreachable from a browser.
	if !strings.HasPrefix(got.SuccessURL, testPublicURL+"/") {
		t.Errorf("SuccessURL = %q, want it on the frontend origin %q", got.SuccessURL, testPublicURL)
	}
	if !strings.HasPrefix(got.CancelURL, testPublicURL+"/") {
		t.Errorf("CancelURL = %q, want it on the frontend origin %q", got.CancelURL, testPublicURL)
	}
	if got.SuccessURL == got.CancelURL {
		t.Errorf("SuccessURL and CancelURL are identical (%q); a cancelled checkout would look successful", got.SuccessURL)
	}
}

func TestCheckout_DefaultsToMonthlyTHB(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")

	var asked db.GetActivePlanPriceParams
	inner := store.getActivePlanPrice
	store.getActivePlanPrice = func(ctx context.Context, arg db.GetActivePlanPriceParams) (db.PlanPrice, error) {
		asked = arg
		return inner(ctx, arg)
	}

	svc := newService(store, &mockStripe{})
	if _, err := svc.Checkout(context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), ""); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	if asked.Currency != "thb" {
		t.Errorf("currency = %q, want %q (decision 3: THB only)", asked.Currency, "thb")
	}
	if asked.BillingInterval != intervalMonth {
		t.Errorf("interval = %q, want %q when the request omits one", asked.BillingInterval, intervalMonth)
	}
}

func TestCheckout_NotConfiguredWhenNoStripeKey(t *testing.T) {
	// Every store func is nil: a request that got past the "is billing
	// configured" check would panic rather than silently pass.
	svc := newService(&mockBillingStore{}, nil)

	_, err := svc.Checkout(context.Background(), uuid.New(), uuid.New(), "a@b.example", uuid.NewString(), "")
	if code := appErrorCode(t, err); code != apperror.BillingNotConfigured {
		t.Fatalf("code = %q, want %q", code, apperror.BillingNotConfigured)
	}
	if status, _ := apperror.Resolve(apperror.BillingNotConfigured); status != 501 {
		t.Fatalf("BILLING_NOT_CONFIGURED resolves to %d, want 501", status)
	}
}

func TestCheckout_UnknownMalformedAndPrivatePlansAreIndistinguishable(t *testing.T) {
	plan, price := planFixture()
	privatePlan := db.Plan{ID: uuid.New(), Name: "bespoke-enterprise", IsPublic: false}

	store := newCatalogueStore(plan, price)
	inner := store.getPlanByID
	store.getPlanByID = func(ctx context.Context, id uuid.UUID) (db.Plan, error) {
		if id == privatePlan.ID {
			return privatePlan, nil
		}
		return inner(ctx, id)
	}
	withExistingCustomer(store, "cus_existing")

	sc := &mockStripe{}
	svc := newService(store, sc)

	for name, planID := range map[string]string{
		"nonexistent plan": uuid.NewString(),
		"malformed id":     "not-a-uuid",
		"non-public plan":  privatePlan.ID.String(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Checkout(context.Background(), uuid.New(), uuid.New(), "a@b.example", planID, "")
			// All three answer NOT_FOUND so this route can't be used as an
			// oracle for which private plans exist in the catalogue.
			if code := appErrorCode(t, err); code != apperror.NotFound {
				t.Fatalf("code = %q, want %q", code, apperror.NotFound)
			}
		})
	}

	if len(sc.checkoutCalls) != 0 || len(sc.customerCalls) != 0 {
		t.Fatalf("a rejected plan still reached Stripe: %d customer, %d checkout calls",
			len(sc.customerCalls), len(sc.checkoutCalls))
	}
}

func TestCheckout_PlanWithNoActivePriceIsNotPurchasable(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
	store.getActivePlanPrice = func(context.Context, db.GetActivePlanPriceParams) (db.PlanPrice, error) {
		return db.PlanPrice{}, pgx.ErrNoRows
	}
	sc := &mockStripe{}

	_, err := newService(store, sc).Checkout(
		context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), "")
	if code := appErrorCode(t, err); code != apperror.PlanNotPurchasable {
		t.Fatalf("code = %q, want %q", code, apperror.PlanNotPurchasable)
	}
	if len(sc.checkoutCalls) != 0 {
		t.Fatalf("a plan with no price still created a Checkout Session")
	}
}

func TestCheckout_UnsupportedIntervalNeverQueriesPrices(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
	store.getActivePlanPrice = func(context.Context, db.GetActivePlanPriceParams) (db.PlanPrice, error) {
		t.Fatal("GetActivePlanPrice must not run for an interval plan_prices can never hold")
		return db.PlanPrice{}, nil
	}

	_, err := newService(store, &mockStripe{}).Checkout(
		context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), "fortnight")
	if code := appErrorCode(t, err); code != apperror.PlanNotPurchasable {
		t.Fatalf("code = %q, want %q", code, apperror.PlanNotPurchasable)
	}
}

func TestCheckout_YearlyIntervalIsAccepted(t *testing.T) {
	plan, _ := planFixture()
	yearly := db.PlanPrice{
		PlanID: plan.ID, StripePriceID: "price_thb_pro_yearly",
		Currency: defaultCurrency, Interval: intervalYear, Active: true,
	}
	store := withExistingCustomer(newCatalogueStore(plan, yearly), "cus_existing")
	sc := &mockStripe{}

	if _, err := newService(store, sc).Checkout(
		context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), intervalYear); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if sc.checkoutCalls[0].PriceID != "price_thb_pro_yearly" {
		t.Fatalf("PriceID = %q, want the yearly price", sc.checkoutCalls[0].PriceID)
	}
}

func TestCheckout_StripeFailureIsA502AndNeverLeaksUpstreamDetail(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
	// A realistic hostile error: Stripe errors quote request parameters
	// back, and an integration that forwards them verbatim is how
	// credential-adjacent detail escapes.
	sc := &mockStripe{checkoutErr: errors.New("request failed with api_key rk_live_supersecret")}

	_, err := newService(store, sc).Checkout(
		context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), "")
	if code := appErrorCode(t, err); code != apperror.BillingProviderError {
		t.Fatalf("code = %q, want %q", code, apperror.BillingProviderError)
	}
	if strings.Contains(err.Error(), "rk_live") {
		t.Fatalf("the error returned to the caller quotes the upstream string: %v", err)
	}
	if status, _ := apperror.Resolve(apperror.BillingProviderError); status != 502 {
		t.Fatalf("BILLING_PROVIDER_ERROR resolves to %d, want 502", status)
	}
}

func TestCheckout_FailedAuditWriteStillSucceeds(t *testing.T) {
	// newTestAudit's querier always fails. Per CLAUDE.md, an audit write is
	// best-effort and must never fail the caller's request.
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")

	if _, err := newService(store, &mockStripe{}).Checkout(
		context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), ""); err != nil {
		t.Fatalf("Checkout failed because the audit write did: %v", err)
	}
}

// ---- lazy Stripe Customer (plan decision 4) ----

func TestEnsureCustomer_CreatedOnFirstInteractionReusedOnSecond(t *testing.T) {
	plan, price := planFixture()

	var recorded *string
	store := newCatalogueStore(plan, price)
	store.getOrgBillingRef = func(_ context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error) {
		return db.GetOrgBillingRefRow{OrganizationID: organizationID, StripeCustomerID: recorded}, nil
	}
	store.claimOrgStripeCustomer = func(_ context.Context, arg db.ClaimOrgStripeCustomerParams) (*string, error) {
		if recorded != nil {
			return nil, pgx.ErrNoRows // already claimed: the conditional UPDATE matches nothing
		}
		id := arg.StripeCustomerID
		recorded = &id
		return &id, nil
	}

	sc := &mockStripe{nextCustomerID: "cus_lazy"}
	svc := newService(store, sc)
	orgID := uuid.New()

	for i, call := range []func() (string, error){
		func() (string, error) {
			return svc.Checkout(context.Background(), orgID, uuid.New(), "a@b.example", plan.ID.String(), "")
		},
		func() (string, error) { return svc.Portal(context.Background(), orgID, uuid.New(), "a@b.example") },
	} {
		if _, err := call(); err != nil {
			t.Fatalf("billing call %d: %v", i, err)
		}
	}

	if len(sc.customerCalls) != 1 {
		t.Fatalf("CreateCustomer called %d times across two billing interactions, want exactly 1", len(sc.customerCalls))
	}
	if recorded == nil || *recorded != "cus_lazy" {
		t.Fatalf("recorded customer = %v, want cus_lazy persisted on the first interaction", recorded)
	}
	if sc.checkoutCalls[0].CustomerID != "cus_lazy" || sc.portalCalls[0].CustomerID != "cus_lazy" {
		t.Fatalf("both interactions must use the same Customer: checkout=%q portal=%q",
			sc.checkoutCalls[0].CustomerID, sc.portalCalls[0].CustomerID)
	}
}

func TestEnsureCustomer_LosingARaceAdoptsTheWinnersCustomer(t *testing.T) {
	// Simulates the interleaving the conditional claim exists for: this
	// request creates a Customer, but by the time it tries to claim, a
	// concurrent request has already recorded a different one. The database
	// stays the single answer — this request must adopt the winner's id,
	// not overwrite it and not return its own.
	plan, price := planFixture()
	store := newCatalogueStore(plan, price)

	claimAttempted := false
	store.getOrgBillingRef = func(_ context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error) {
		if !claimAttempted {
			return db.GetOrgBillingRefRow{OrganizationID: organizationID}, nil
		}
		winner := "cus_winner"
		return db.GetOrgBillingRefRow{OrganizationID: organizationID, StripeCustomerID: &winner}, nil
	}
	store.claimOrgStripeCustomer = func(context.Context, db.ClaimOrgStripeCustomerParams) (*string, error) {
		claimAttempted = true
		return nil, pgx.ErrNoRows
	}

	sc := &mockStripe{nextCustomerID: "cus_loser"}
	if _, err := newService(store, sc).Checkout(
		context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), ""); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	if sc.checkoutCalls[0].CustomerID != "cus_winner" {
		t.Fatalf("CustomerID = %q, want the id already recorded in Postgres (cus_winner)", sc.checkoutCalls[0].CustomerID)
	}
}

func TestEnsureCustomer_NoSubscriptionRowStillWorksAndCreatesNoRow(t *testing.T) {
	// An org that has never been assigned a plan has no org_subscriptions
	// row. Billing must still work — and must NOT create that row: it is
	// the entitlement record, and an org without one resolves to
	// "unlimited" through EffectiveLimits, so inserting a free-plan row
	// here would quietly tighten that org's limits as a side effect of a
	// billing click. Creating it is AssignPlan's job (invariant 5).
	plan, price := planFixture()
	store := newCatalogueStore(plan, price)
	store.getOrgBillingRef = func(context.Context, uuid.UUID) (db.GetOrgBillingRefRow, error) {
		return db.GetOrgBillingRefRow{}, pgx.ErrNoRows
	}
	store.claimOrgStripeCustomer = func(context.Context, db.ClaimOrgStripeCustomerParams) (*string, error) {
		return nil, pgx.ErrNoRows // conditional UPDATE matched no row
	}

	sc := &mockStripe{nextCustomerID: "cus_rowless"}
	if _, err := newService(store, sc).Checkout(
		context.Background(), uuid.New(), uuid.New(), "a@b.example", plan.ID.String(), ""); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if sc.checkoutCalls[0].CustomerID != "cus_rowless" {
		t.Fatalf("CustomerID = %q, want the freshly created customer", sc.checkoutCalls[0].CustomerID)
	}
}

func TestEnsureCustomer_IdempotencyKeyIsDeterministicPerOrg(t *testing.T) {
	// The Stripe half of the no-duplicate-Customer guarantee: two concurrent
	// creates carrying the same Idempotency-Key return the same Customer
	// rather than minting two. A per-attempt random key (the SDK's
	// NewIdempotencyKey) would not survive this race, which is why the key
	// is derived from the org id.
	orgA, orgB := uuid.New(), uuid.New()

	if got, want := customerIdempotencyKey(orgA), customerIdempotencyKey(orgA); got != want {
		t.Fatalf("key is not stable for one org: %q vs %q", got, want)
	}
	if customerIdempotencyKey(orgA) == customerIdempotencyKey(orgB) {
		t.Fatalf("two different orgs share an idempotency key; they would share a Stripe Customer")
	}
	if !strings.Contains(customerIdempotencyKey(orgA), orgA.String()) {
		t.Fatalf("key %q does not derive from the org id", customerIdempotencyKey(orgA))
	}
}

func TestEnsureCustomer_TenantsNeverShareACustomer(t *testing.T) {
	// Tenant isolation, the class SECURITY.md names first: each org's
	// billing objects are built from ITS OWN row, keyed by the
	// organization id the guard resolved. Org B's request must never reach
	// org A's Stripe Customer.
	plan, price := planFixture()
	orgA, orgB := uuid.New(), uuid.New()
	customers := map[uuid.UUID]string{orgA: "cus_org_a", orgB: "cus_org_b"}

	store := newCatalogueStore(plan, price)
	store.getOrgBillingRef = func(_ context.Context, organizationID uuid.UUID) (db.GetOrgBillingRefRow, error) {
		id := customers[organizationID]
		return db.GetOrgBillingRefRow{OrganizationID: organizationID, StripeCustomerID: &id}, nil
	}

	sc := &mockStripe{}
	svc := newService(store, sc)

	if _, err := svc.Checkout(context.Background(), orgA, uuid.New(), "a@a.example", plan.ID.String(), ""); err != nil {
		t.Fatalf("org A checkout: %v", err)
	}
	if _, err := svc.Portal(context.Background(), orgB, uuid.New(), "b@b.example"); err != nil {
		t.Fatalf("org B portal: %v", err)
	}

	if sc.checkoutCalls[0].CustomerID != "cus_org_a" {
		t.Errorf("org A got customer %q", sc.checkoutCalls[0].CustomerID)
	}
	if sc.checkoutCalls[0].ClientReferenceID != orgA.String() {
		t.Errorf("org A session references %q, want %q", sc.checkoutCalls[0].ClientReferenceID, orgA)
	}
	if sc.portalCalls[0].CustomerID != "cus_org_b" {
		t.Errorf("org B got customer %q, want cus_org_b — org A's portal must be unreachable from org B", sc.portalCalls[0].CustomerID)
	}
}

func TestEnsureCustomer_CustomerCarriesOrgIdentity(t *testing.T) {
	plan, price := planFixture()
	var claimed []db.ClaimOrgStripeCustomerParams
	store := withClaimableRow(newCatalogueStore(plan, price), &claimed)
	sc := &mockStripe{}
	orgID := uuid.New()

	if _, err := newService(store, sc).Checkout(
		context.Background(), orgID, uuid.New(), "finance@acme.example", plan.ID.String(), ""); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	created := sc.customerCalls[0]
	if created.OrganizationID != orgID.String() {
		t.Errorf("customer metadata org = %q, want %q", created.OrganizationID, orgID)
	}
	if created.Email != "finance@acme.example" {
		t.Errorf("customer email = %q, want the caller's", created.Email)
	}
	if created.Name != "Acme Co" {
		t.Errorf("customer name = %q, want the ORGANIZATION's name, not the person's", created.Name)
	}
	if len(claimed) != 1 || claimed[0].OrganizationID != orgID {
		t.Fatalf("claim = %+v, want exactly one claim for %s", claimed, orgID)
	}
}

func TestEnsureCustomer_StripeFailureIsA502(t *testing.T) {
	plan, price := planFixture()
	store := withClaimableRow(newCatalogueStore(plan, price), nil)
	sc := &mockStripe{createErr: errors.New("stripe is down")}

	_, err := newService(store, sc).Portal(context.Background(), uuid.New(), uuid.New(), "a@b.example")
	if code := appErrorCode(t, err); code != apperror.BillingProviderError {
		t.Fatalf("code = %q, want %q", code, apperror.BillingProviderError)
	}
}

// ---- Portal ----

func TestPortal_HappyPath(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
	sc := &mockStripe{}

	url, err := newService(store, sc).Portal(context.Background(), uuid.New(), uuid.New(), "a@b.example")
	if err != nil {
		t.Fatalf("Portal: %v", err)
	}
	if url != "https://billing.stripe.com/p/session/bps_test_123" {
		t.Fatalf("url = %q, want the hosted portal URL", url)
	}
	if len(sc.portalCalls) != 1 {
		t.Fatalf("CreatePortalSession called %d times, want 1", len(sc.portalCalls))
	}
	if sc.portalCalls[0].ReturnURL != testPublicURL+"/subscription" {
		t.Fatalf("ReturnURL = %q, want it on the frontend origin", sc.portalCalls[0].ReturnURL)
	}
}

func TestPortal_NotConfiguredWhenNoStripeKey(t *testing.T) {
	svc := newService(&mockBillingStore{}, nil)
	_, err := svc.Portal(context.Background(), uuid.New(), uuid.New(), "a@b.example")
	if code := appErrorCode(t, err); code != apperror.BillingNotConfigured {
		t.Fatalf("code = %q, want %q", code, apperror.BillingNotConfigured)
	}
}

// ---- the Stripe traps, asserted structurally ----

// TestCheckoutInput_HasNoPaymentMethodTypesOrAutomaticTax is a regression
// guard on the two Stripe traps that silently produce a working-looking
// integration:
//
//   - payment_method_types: passing ["card"] disables dynamic payment
//     methods and removes PromptPay, which for Thai customers is not a
//     rounding error. It must stay unset, so there must be no field
//     through which a caller could set it.
//   - automatic_tax: Stripe Tax calculates nothing AND errors nothing
//     without an active registration, so switching it on without one looks
//     handled and collects zero (decision 3: off at launch, Thai VAT rides
//     the entity's own registration).
//
// A field-name assertion rather than a request assertion, because the point
// is that the capability is absent from the type, not merely unused today.
func TestCheckoutInput_HasNoPaymentMethodTypesOrAutomaticTax(t *testing.T) {
	forbidden := []string{"paymentmethodtypes", "automatictax", "tax", "coupon", "discounts", "trialperioddays"}
	typ := reflect.TypeOf(checkoutInput{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		for _, bad := range forbidden {
			if name == bad {
				t.Errorf("checkoutInput has a %q field; see this test's doc comment for why it must not", typ.Field(i).Name)
			}
		}
	}
}

// TestIntegrationIdentifier_IsStableWithAnEightLetterSuffix pins the
// identifier's shape. It exists so flows created by this integration are
// comparable to each other in the Stripe Dashboard over time, which a
// per-request random value would defeat entirely — so this asserts the
// constant is a constant, and that its suffix is the 8 random letters the
// Stripe guidance asks for.
func TestIntegrationIdentifier_IsStableWithAnEightLetterSuffix(t *testing.T) {
	if !regexp.MustCompile(`^[a-z0-9_]+_[a-z]{8}$`).MatchString(integrationIdentifier) {
		t.Fatalf("integrationIdentifier = %q, want a name plus an 8-lowercase-letter suffix", integrationIdentifier)
	}
}

// TestNewStripeClient_NilWithoutAKey is what makes a local dev box and CI
// able to boot and run the whole suite with no Stripe credentials: server.go
// mounts the routes either way, and a nil client is the BILLING_NOT_CONFIGURED
// path rather than a startup failure.
func TestNewStripeClient_NilWithoutAKey(t *testing.T) {
	if got := NewStripeClient(""); got != nil {
		t.Fatalf("NewStripeClient(\"\") = %v, want nil", got)
	}
	if got := NewStripeClient("rk_test_pretend"); got == nil {
		t.Fatal("NewStripeClient with a key returned nil")
	}
}

// errAPIDown is a stand-in upstream failure shared by the handler tests.
var errAPIDown = errors.New("stripe api is down")
