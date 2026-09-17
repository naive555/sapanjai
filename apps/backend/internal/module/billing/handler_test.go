package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/authtoken"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// ---- test harness: a real echo + real appmw.Guards over fake infra,
// mirroring internal/module/mcpkey/handler_test.go. ----

type testValidator struct{ v *validator.Validate }

func newTestValidator() *testValidator {
	return &testValidator{v: validator.New(validator.WithRequiredStructEnabled())}
}

func (tv *testValidator) Validate(i any) error { return tv.v.Struct(i) }

type fakeTokenVerifier struct {
	userID uuid.UUID
	email  string
}

func (f *fakeTokenVerifier) VerifyAccessToken(string) (authtoken.AccessToken, error) {
	return authtoken.AccessToken{UserID: f.userID, Email: f.email}, nil
}

type fakeBlacklistChecker struct{}

func (f *fakeBlacklistChecker) IsBlacklisted(context.Context, string) (bool, error) {
	return false, nil
}
func (f *fakeBlacklistChecker) IsBanned(context.Context, uuid.UUID) (bool, error) { return false, nil }
func (f *fakeBlacklistChecker) IsTwoFactorVerified(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}

type fakeMembershipStore struct{}

func (f *fakeMembershipStore) GetMembership(_ context.Context, arg db.GetMembershipParams) (db.Membership, error) {
	return db.Membership{ID: uuid.New(), UserID: arg.UserID, OrganizationID: arg.OrganizationID, Role: "member"}, nil
}

func (f *fakeMembershipStore) GetUserByID(_ context.Context, id uuid.UUID) (db.User, error) {
	return db.User{ID: id}, nil
}

type fakePermissionChecker struct{ granted map[string]bool }

func (f *fakePermissionChecker) HasPermission(_ context.Context, _, _ uuid.UUID, action string) (bool, error) {
	return f.granted[action], nil
}

func newTestGuards(userID uuid.UUID, email string, granted map[string]bool) *appmw.Guards {
	return appmw.NewGuards(
		&fakeTokenVerifier{userID: userID, email: email},
		&fakeBlacklistChecker{},
		&fakeMembershipStore{},
		&fakePermissionChecker{granted: granted},
	)
}

// newTestEcho mounts the real routes behind the real guards. The error
// handler mirrors internal/server's (which can't be imported here without a
// cycle) in the one respect these tests care about: an *apperror.Error
// resolves through apperror.Resolve, so a service code shows up as its real
// status rather than echo's default 500.
func newTestEcho(svc *Service, guards *appmw.Guards) *echo.Echo {
	e := echo.New()
	e.Validator = newTestValidator()
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		status, message := http.StatusInternalServerError, "Internal server error"
		var appErr *apperror.Error
		var httpErr *echo.HTTPError
		if errors.As(err, &appErr) {
			status, message = apperror.Resolve(appErr.Code)
		} else if errors.As(err, &httpErr) {
			status = httpErr.Code
			if msg, ok := httpErr.Message.(string); ok {
				message = msg
			}
		}
		_ = c.JSON(status, httpx.ErrorResponse{Message: message})
	}
	NewHandler(svc).Register(e.Group("/billing"), guards)
	return e
}

func doBillingRequest(t *testing.T, e *echo.Echo, method, path string, body any, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	if s, ok := body.(string); ok {
		buf.WriteString(s)
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf.Write(b)
	}

	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, decoded
}

func authHeaders(orgID uuid.UUID) map[string]string {
	return map[string]string{"Authorization": "Bearer x", "x-organization-id": orgID.String()}
}

// ---- permission enforcement (plan invariant 4) ----

// TestHandler_WithoutBillingWriteEveryRouteIsDenied is the regression test
// for the deleted POST /subscription/assign: that route sat on RequireOrg,
// which is membership-only, so any `member` could move their own org onto
// any plan and lift max_members/max_roles/max_connectors on themselves.
// Both routes here take an RBAC action instead — including the Portal,
// which can cancel the subscription and expose invoices carrying a billing
// address, and so never gets the weaker guard.
//
// The store and Stripe mocks are bare: every func field is nil, so a denied
// request that somehow reached the service would panic and fail loudly
// rather than pass quietly.
func TestHandler_WithoutBillingWriteEveryRouteIsDenied(t *testing.T) {
	orgID, userID := uuid.New(), uuid.New()
	svc := newService(&mockBillingStore{}, &mockStripe{})

	for name, granted := range map[string]map[string]bool{
		"no permissions at all":      {},
		"a read-ish permission only": {"billing:read": true},
		"permissions for other resources": {
			"connector:write": true, "mcpkey:write": true, "organization:write": true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEcho(svc, newTestGuards(userID, "member@acme.example", granted))

			for _, route := range []struct{ method, path string }{
				{http.MethodPost, "/billing/checkout"},
				{http.MethodPost, "/billing/portal"},
			} {
				rec, body := doBillingRequest(t, e, route.method, route.path,
					map[string]any{"planId": uuid.NewString()}, authHeaders(orgID))
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s %s: status = %d, want 403; body = %v", route.method, route.path, rec.Code, body)
				}
				if body["message"] != "Missing permission: "+PermissionWrite {
					t.Errorf("%s %s: message = %v, want %q", route.method, route.path, body["message"],
						"Missing permission: "+PermissionWrite)
				}
			}
		})
	}
}

func TestHandler_MissingOrgHeaderIs400(t *testing.T) {
	e := newTestEcho(newService(&mockBillingStore{}, &mockStripe{}),
		newTestGuards(uuid.New(), "a@b.example", map[string]bool{PermissionWrite: true}))

	for _, path := range []string{"/billing/checkout", "/billing/portal"} {
		rec, body := doBillingRequest(t, e, http.MethodPost, path,
			map[string]any{"planId": uuid.NewString()}, map[string]string{"Authorization": "Bearer x"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body = %v", path, rec.Code, body)
		}
		if body["message"] != "Missing x-organization-id header" {
			t.Errorf("%s: message = %v", path, body["message"])
		}
	}
}

// ---- request binding and validation ----

func TestHandler_Checkout_ValidationErrors(t *testing.T) {
	orgID := uuid.New()
	e := newTestEcho(newService(&mockBillingStore{}, &mockStripe{}),
		newTestGuards(uuid.New(), "a@b.example", map[string]bool{PermissionWrite: true}))

	cases := map[string]struct {
		body       any
		wantStatus int
		wantMsg    string
	}{
		"missing planId": {map[string]any{}, http.StatusUnprocessableEntity, "Validation failed"},
		"malformed planId": {map[string]any{"planId": "not-a-uuid"},
			http.StatusUnprocessableEntity, "Validation failed"},
		"unsupported interval": {map[string]any{"planId": uuid.NewString(), "interval": "fortnight"},
			http.StatusUnprocessableEntity, "Validation failed"},
		"malformed JSON": {"{", http.StatusBadRequest, "Invalid request body"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec, body := doBillingRequest(t, e, http.MethodPost, "/billing/checkout", tc.body, authHeaders(orgID))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %v", rec.Code, tc.wantStatus, body)
			}
			if body["message"] != tc.wantMsg {
				t.Fatalf("message = %v, want %q", body["message"], tc.wantMsg)
			}
		})
	}
}

// ---- happy path through the full handler stack ----

func TestHandler_Checkout_ReturnsRedirectURLAndPassesCallerIdentity(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
	sc := &mockStripe{}
	orgID, userID := uuid.New(), uuid.New()

	e := newTestEcho(newService(store, sc),
		newTestGuards(userID, "finance@acme.example", map[string]bool{PermissionWrite: true}))

	rec, body := doBillingRequest(t, e, http.MethodPost, "/billing/checkout",
		map[string]any{"planId": plan.ID.String()}, authHeaders(orgID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", rec.Code, body)
	}
	if body["url"] != "https://checkout.stripe.com/c/pay/cs_test_123" {
		t.Fatalf("url = %v", body["url"])
	}
	// The response carries the redirect URL and nothing else — no session
	// id, no customer id, no price.
	if len(body) != 1 {
		t.Fatalf("response has %d keys (%v), want exactly {url}", len(body), body)
	}
	// The organization comes from the guard-resolved header, never a body
	// field: there is no input through which a caller could name another
	// tenant's organization.
	if sc.checkoutCalls[0].ClientReferenceID != orgID.String() {
		t.Fatalf("session references %q, want the header org %q", sc.checkoutCalls[0].ClientReferenceID, orgID)
	}
}

func TestHandler_Portal_ReturnsRedirectURL(t *testing.T) {
	plan, price := planFixture()
	store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
	e := newTestEcho(newService(store, &mockStripe{}),
		newTestGuards(uuid.New(), "a@b.example", map[string]bool{PermissionWrite: true}))

	rec, body := doBillingRequest(t, e, http.MethodPost, "/billing/portal", nil, authHeaders(uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", rec.Code, body)
	}
	if body["url"] != "https://billing.stripe.com/p/session/bps_test_123" {
		t.Fatalf("url = %v", body["url"])
	}
}

// ---- service error codes reach the wire with the right status ----

func TestHandler_ServiceErrorsMapToStatuses(t *testing.T) {
	plan, price := planFixture()
	orgID := uuid.New()
	guards := newTestGuards(uuid.New(), "a@b.example", map[string]bool{PermissionWrite: true})

	t.Run("unknown plan is 404", func(t *testing.T) {
		store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
		e := newTestEcho(newService(store, &mockStripe{}), guards)
		rec, body := doBillingRequest(t, e, http.MethodPost, "/billing/checkout",
			map[string]any{"planId": uuid.NewString()}, authHeaders(orgID))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", rec.Code, body)
		}
	})

	t.Run("plan with no active price is 409", func(t *testing.T) {
		store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
		store.getActivePlanPrice = func(context.Context, db.GetActivePlanPriceParams) (db.PlanPrice, error) {
			return db.PlanPrice{}, pgx.ErrNoRows
		}
		e := newTestEcho(newService(store, &mockStripe{}), guards)
		rec, body := doBillingRequest(t, e, http.MethodPost, "/billing/checkout",
			map[string]any{"planId": plan.ID.String()}, authHeaders(orgID))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", rec.Code, body)
		}
		if body["message"] != "Plan is not available for purchase" {
			t.Fatalf("message = %v", body["message"])
		}
	})

	// Both routes stay mounted and stay guarded with no Stripe key
	// configured, so a permission regression cannot hide behind a route
	// that simply isn't registered on a Stripe-less deployment.
	t.Run("no stripe key is 501 on both routes", func(t *testing.T) {
		e := newTestEcho(newService(&mockBillingStore{}, nil), guards)
		for _, path := range []string{"/billing/checkout", "/billing/portal"} {
			rec, body := doBillingRequest(t, e, http.MethodPost, path,
				map[string]any{"planId": plan.ID.String()}, authHeaders(orgID))
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("%s: status = %d, want 501; body = %v", path, rec.Code, body)
			}
			if body["message"] != "Billing is not configured" {
				t.Fatalf("%s: message = %v", path, body["message"])
			}
		}
	})

	t.Run("stripe failure is 502", func(t *testing.T) {
		store := withExistingCustomer(newCatalogueStore(plan, price), "cus_existing")
		e := newTestEcho(newService(store, &mockStripe{portalErr: errAPIDown}), guards)
		rec, body := doBillingRequest(t, e, http.MethodPost, "/billing/portal", nil, authHeaders(orgID))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body = %v", rec.Code, body)
		}
	})
}
