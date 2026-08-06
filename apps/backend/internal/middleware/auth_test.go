package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// ---- hand-mocked dependencies ----

type mockTokenVerifier struct {
	verify func(token string) (uuid.UUID, string, error)
}

func (m *mockTokenVerifier) VerifyAccessToken(token string) (uuid.UUID, string, error) {
	return m.verify(token)
}

type mockBlacklist struct {
	isBlacklisted func(ctx context.Context, token string) (bool, error)
}

func (m *mockBlacklist) IsBlacklisted(ctx context.Context, token string) (bool, error) {
	return m.isBlacklisted(ctx, token)
}

type mockMembershipStore struct {
	getMembership func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
}

func (m *mockMembershipStore) GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
	return m.getMembership(ctx, arg)
}

type mockPermissionChecker struct {
	hasPermission func(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error)
}

func (m *mockPermissionChecker) HasPermission(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
	return m.hasPermission(ctx, userID, organizationID, action)
}

// ---- helpers ----

func newTestContext(method, target string, headers map[string]string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func okNext(c echo.Context) error {
	return c.String(http.StatusOK, "ok")
}

func assertHTTPError(t *testing.T, err error, wantStatus int, wantMessage string) {
	t.Helper()
	var he *echo.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *echo.HTTPError, got %T (%v)", err, err)
	}
	if he.Code != wantStatus {
		t.Errorf("status = %d, want %d", he.Code, wantStatus)
	}
	if msg, ok := he.Message.(string); !ok || msg != wantMessage {
		t.Errorf("message = %v, want %q", he.Message, wantMessage)
	}
}

// ---- RequireAuth ----

func TestRequireAuth_MissingToken(t *testing.T) {
	g := NewGuards(
		&mockTokenVerifier{},
		&mockBlacklist{},
		&mockMembershipStore{},
		&mockPermissionChecker{},
	)
	c, _ := newTestContext(http.MethodGet, "/", nil)

	err := g.RequireAuth()(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
}

func TestRequireAuth_BlacklistedTokenCheckedBeforeSignature(t *testing.T) {
	// A blacklisted-but-otherwise-garbage token must still yield "Token
	// revoked", proving the blacklist check runs before signature
	// verification, per plugin.ts verifyToken.
	g := NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				t.Fatal("VerifyAccessToken should not be called once blacklisted")
				return uuid.Nil, "", nil
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return true, nil },
		},
		&mockMembershipStore{},
		&mockPermissionChecker{},
	)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer not-even-a-jwt",
	})

	err := g.RequireAuth()(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Token revoked")
}

func TestRequireAuth_InvalidSignature(t *testing.T) {
	g := NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return uuid.Nil, "", errors.New("bad signature")
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
		},
		&mockMembershipStore{},
		&mockPermissionChecker{},
	)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer some.jwt.token",
	})

	err := g.RequireAuth()(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
}

func TestRequireAuth_Success(t *testing.T) {
	userID := uuid.New()
	g := NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return userID, "user@example.com", nil
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
		},
		&mockMembershipStore{},
		&mockPermissionChecker{},
	)
	c, rec := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	var gotID uuid.UUID
	var gotEmail string
	next := func(c echo.Context) error {
		gotID = UserID(c)
		gotEmail = UserEmail(c)
		return c.String(http.StatusOK, "ok")
	}

	if err := g.RequireAuth()(next)(c); err != nil {
		t.Fatalf("RequireAuth: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotID != userID {
		t.Errorf("UserID(c) = %v, want %v", gotID, userID)
	}
	if gotEmail != "user@example.com" {
		t.Errorf("UserEmail(c) = %q, want %q", gotEmail, "user@example.com")
	}
}

// ---- RequireOrg ----

func validAuthGuardsForOrg(t *testing.T, userID uuid.UUID, membershipFn func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)) *Guards {
	t.Helper()
	return NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return userID, "user@example.com", nil
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
		},
		&mockMembershipStore{getMembership: membershipFn},
		&mockPermissionChecker{},
	)
}

func TestRequireOrg_MissingToken(t *testing.T) {
	g := NewGuards(&mockTokenVerifier{}, &mockBlacklist{}, &mockMembershipStore{}, &mockPermissionChecker{})
	c, _ := newTestContext(http.MethodGet, "/", nil)

	err := g.RequireOrg()(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
}

func TestRequireOrg_MissingHeader(t *testing.T) {
	g := validAuthGuardsForOrg(t, uuid.New(), nil)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequireOrg()(okNext)(c)
	assertHTTPError(t, err, http.StatusBadRequest, "Missing x-organization-id header")
}

func TestRequireOrg_MalformedOrgID(t *testing.T) {
	g := validAuthGuardsForOrg(t, uuid.New(), func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
		t.Fatal("GetMembership should not be called for a malformed org id")
		return db.Membership{}, nil
	})
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       "not-a-uuid",
	})

	err := g.RequireOrg()(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Not a member of this organization")
}

func TestRequireOrg_NotAMember(t *testing.T) {
	orgID := uuid.New()
	g := validAuthGuardsForOrg(t, uuid.New(), func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
		return db.Membership{}, pgx.ErrNoRows
	})
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	err := g.RequireOrg()(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Not a member of this organization")
}

func TestRequireOrg_DatabaseErrorPropagates(t *testing.T) {
	orgID := uuid.New()
	dbErr := errors.New("connection reset")
	g := validAuthGuardsForOrg(t, uuid.New(), func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
		return db.Membership{}, dbErr
	})
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	err := g.RequireOrg()(okNext)(c)
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

func TestRequireOrg_Success(t *testing.T) {
	userID := uuid.New()
	orgID := uuid.New()
	membership := db.Membership{ID: uuid.New(), UserID: userID, OrganizationID: orgID, Role: "admin"}

	g := validAuthGuardsForOrg(t, userID, func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
		if arg.UserID != userID || arg.OrganizationID != orgID {
			t.Fatalf("GetMembership called with %+v, want user=%v org=%v", arg, userID, orgID)
		}
		return membership, nil
	})
	c, rec := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	var gotOrgID uuid.UUID
	var gotMembership db.Membership
	next := func(c echo.Context) error {
		gotOrgID = OrgID(c)
		gotMembership = MembershipFromContext(c)
		return c.String(http.StatusOK, "ok")
	}

	if err := g.RequireOrg()(next)(c); err != nil {
		t.Fatalf("RequireOrg: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotOrgID != orgID {
		t.Errorf("OrgID(c) = %v, want %v", gotOrgID, orgID)
	}
	if gotMembership.Role != "admin" {
		t.Errorf("MembershipFromContext(c).Role = %q, want %q", gotMembership.Role, "admin")
	}
}

// ---- RequirePermission ----

func validAuthGuardsForPermission(
	t *testing.T,
	userID uuid.UUID,
	membershipFn func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error),
	hasPermissionFn func(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error),
) *Guards {
	t.Helper()
	return NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return userID, "user@example.com", nil
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
		},
		&mockMembershipStore{getMembership: membershipFn},
		&mockPermissionChecker{hasPermission: hasPermissionFn},
	)
}

func TestRequirePermission_MissingHeader(t *testing.T) {
	g := validAuthGuardsForPermission(t, uuid.New(), nil, nil)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequirePermission("project:create")(okNext)(c)
	assertHTTPError(t, err, http.StatusBadRequest, "Missing x-organization-id header")
}

func TestRequirePermission_MalformedOrgID(t *testing.T) {
	g := validAuthGuardsForPermission(t, uuid.New(), nil, func(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
		t.Fatal("HasPermission should not be called for a malformed org id")
		return false, nil
	})
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       "not-a-uuid",
	})

	err := g.RequirePermission("project:create")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Missing permission: project:create")
}

func TestRequirePermission_Denied(t *testing.T) {
	orgID := uuid.New()
	g := validAuthGuardsForPermission(t, uuid.New(), nil, func(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
		return false, nil
	})
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	err := g.RequirePermission("project:create")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Missing permission: project:create")
}

func TestRequirePermission_NonMemberGetsMissingPermissionNotNotAMember(t *testing.T) {
	// The permission check runs BEFORE membership resolution, per plugin.ts
	// requirePermission — a caller with no membership at all (HasPermission
	// returns false for them, same as a real non-member) must see "Missing
	// permission", never "Not a member of this organization".
	orgID := uuid.New()
	g := validAuthGuardsForPermission(t, uuid.New(),
		func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			t.Fatal("GetMembership should not be called before the permission check fails")
			return db.Membership{}, nil
		},
		func(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
			return false, nil
		},
	)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	err := g.RequirePermission("project:create")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Missing permission: project:create")
}

func TestRequirePermission_CheckerErrorPropagates(t *testing.T) {
	orgID := uuid.New()
	checkErr := errors.New("rbac lookup failed")
	g := validAuthGuardsForPermission(t, uuid.New(), nil, func(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
		return false, checkErr
	})
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	err := g.RequirePermission("project:create")(okNext)(c)
	if !errors.Is(err, checkErr) {
		t.Fatalf("expected the raw error to propagate, got %v", err)
	}
}

func TestRequirePermission_AllowedButMembershipMissingPropagatesNotAMember(t *testing.T) {
	orgID := uuid.New()
	g := validAuthGuardsForPermission(t, uuid.New(),
		func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, pgx.ErrNoRows
		},
		func(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
			return true, nil
		},
	)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	err := g.RequirePermission("project:create")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Not a member of this organization")
}

func TestRequirePermission_Success(t *testing.T) {
	userID := uuid.New()
	orgID := uuid.New()
	membership := db.Membership{ID: uuid.New(), UserID: userID, OrganizationID: orgID, Role: "member"}

	g := validAuthGuardsForPermission(t, userID,
		func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			if arg.UserID != userID || arg.OrganizationID != orgID {
				t.Fatalf("GetMembership called with %+v, want user=%v org=%v", arg, userID, orgID)
			}
			return membership, nil
		},
		func(ctx context.Context, gotUserID, gotOrgID uuid.UUID, action string) (bool, error) {
			if gotUserID != userID || gotOrgID != orgID || action != "project:create" {
				t.Fatalf("HasPermission called with user=%v org=%v action=%q, want user=%v org=%v action=%q",
					gotUserID, gotOrgID, action, userID, orgID, "project:create")
			}
			return true, nil
		},
	)
	c, rec := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
		OrgHeader:       orgID.String(),
	})

	var gotOrgID uuid.UUID
	var gotMembership db.Membership
	next := func(c echo.Context) error {
		gotOrgID = OrgID(c)
		gotMembership = MembershipFromContext(c)
		return c.String(http.StatusOK, "ok")
	}

	if err := g.RequirePermission("project:create")(next)(c); err != nil {
		t.Fatalf("RequirePermission: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotOrgID != orgID {
		t.Errorf("OrgID(c) = %v, want %v", gotOrgID, orgID)
	}
	if gotMembership.Role != "member" {
		t.Errorf("MembershipFromContext(c).Role = %q, want %q", gotMembership.Role, "member")
	}
}
