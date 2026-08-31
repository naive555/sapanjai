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
	"github.com/sapanjai/backend/internal/shared/authtoken"
)

// ---- hand-mocked dependencies ----

// mockTokenVerifier keeps the pre-impersonation (userID, email, error)
// triple as its common-case knob so every existing test reads unchanged,
// and adds verifyClaims for the handful that need to express the imp/act
// claims a triple cannot carry. verifyClaims wins when both are set.
type mockTokenVerifier struct {
	verify       func(token string) (uuid.UUID, string, error)
	verifyClaims func(token string) (authtoken.AccessToken, error)
}

func (m *mockTokenVerifier) VerifyAccessToken(token string) (authtoken.AccessToken, error) {
	if m.verifyClaims != nil {
		return m.verifyClaims(token)
	}
	userID, email, err := m.verify(token)
	return authtoken.AccessToken{UserID: userID, Email: email}, err
}

type mockBlacklist struct {
	isBlacklisted       func(ctx context.Context, token string) (bool, error)
	isBanned            func(ctx context.Context, userID uuid.UUID) (bool, error)
	isTwoFactorVerified func(ctx context.Context, userID uuid.UUID) (bool, error)
}

func (m *mockBlacklist) IsBlacklisted(ctx context.Context, token string) (bool, error) {
	return m.isBlacklisted(ctx, token)
}

func (m *mockBlacklist) IsBanned(ctx context.Context, userID uuid.UUID) (bool, error) {
	if m.isBanned == nil {
		return false, nil
	}
	return m.isBanned(ctx, userID)
}

// IsTwoFactorVerified defaults to true (verified) rather than false: most
// existing tests never call SetAdminRequire2FA(true), so the 2FA branch in
// requirePlatformRole is never reached and this default is inert for them.
// The handful of tests that DO exercise ADMIN_REQUIRE_2FA set this field
// explicitly.
func (m *mockBlacklist) IsTwoFactorVerified(ctx context.Context, userID uuid.UUID) (bool, error) {
	if m.isTwoFactorVerified == nil {
		return true, nil
	}
	return m.isTwoFactorVerified(ctx, userID)
}

type mockMembershipStore struct {
	getMembership func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
	getUserByID   func(ctx context.Context, id uuid.UUID) (db.User, error)
}

func (m *mockMembershipStore) GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
	return m.getMembership(ctx, arg)
}

func (m *mockMembershipStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	if m.getUserByID == nil {
		return db.User{}, nil
	}
	return m.getUserByID(ctx, id)
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

func TestRequireAuth_BannedUser(t *testing.T) {
	userID := uuid.New()
	g := NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return userID, "user@example.com", nil
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
			isBanned: func(ctx context.Context, gotUserID uuid.UUID) (bool, error) {
				if gotUserID != userID {
					t.Errorf("IsBanned called with %v, want %v", gotUserID, userID)
				}
				return true, nil
			},
		},
		&mockMembershipStore{},
		&mockPermissionChecker{},
	)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequireAuth()(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Account suspended")
}

func TestRequireAuth_BanCheckedAfterSignatureVerification(t *testing.T) {
	// A banned check must never run for a token whose signature fails to
	// verify — there is no trustworthy user id to check yet.
	g := NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return uuid.Nil, "", errors.New("bad signature")
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
			isBanned: func(ctx context.Context, userID uuid.UUID) (bool, error) {
				t.Fatal("IsBanned should not be called when signature verification fails")
				return false, nil
			},
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

func TestRequireAuth_IsBannedErrorPropagates(t *testing.T) {
	banErr := errors.New("redis down")
	g := NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return uuid.New(), "user@example.com", nil
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
			isBanned: func(ctx context.Context, userID uuid.UUID) (bool, error) {
				return false, banErr
			},
		},
		&mockMembershipStore{},
		&mockPermissionChecker{},
	)
	c, _ := newTestContext(http.MethodGet, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequireAuth()(okNext)(c)
	if !errors.Is(err, banErr) {
		t.Fatalf("expected the raw error to propagate, got %v", err)
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

// ---- Impersonation (execution plan Phase 4, docs/11-admin-panel.md §5) ----

// impersonationGuards builds a Guards whose verifier returns an
// impersonation token for the given target/actor, with nothing blacklisted
// or banned.
func impersonationGuards(targetID, actorID uuid.UUID, store userStore) *Guards {
	return NewGuards(
		&mockTokenVerifier{
			verifyClaims: func(token string) (authtoken.AccessToken, error) {
				return authtoken.AccessToken{
					UserID:       targetID,
					Email:        "target@example.com",
					ActorID:      actorID,
					Impersonated: true,
				}, nil
			},
		},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
		},
		store,
		&mockPermissionChecker{},
	)
}

// The read-only rule is enforced by method, at the guard, so that a route
// added later is covered without anyone remembering to add it to a list.
func TestRequireAuth_ImpersonationRejectsUnsafeMethods(t *testing.T) {
	g := impersonationGuards(uuid.New(), uuid.New(), &mockMembershipStore{})

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		t.Run(method, func(t *testing.T) {
			c, _ := newTestContext(method, "/", map[string]string{
				"Authorization": "Bearer impersonation.jwt.token",
			})
			err := g.RequireAuth()(okNext)(c)
			assertHTTPError(t, err, http.StatusForbidden, "Impersonated sessions are read-only")
		})
	}
}

func TestRequireAuth_ImpersonationAllowsSafeMethods(t *testing.T) {
	targetID, actorID := uuid.New(), uuid.New()
	g := impersonationGuards(targetID, actorID, &mockMembershipStore{})

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			c, _ := newTestContext(method, "/", map[string]string{
				"Authorization": "Bearer impersonation.jwt.token",
			})

			var gotUser, gotActor uuid.UUID
			var gotImpersonated bool
			next := func(c echo.Context) error {
				gotUser, gotActor, gotImpersonated = UserID(c), ActorID(c), IsImpersonated(c)
				return c.String(http.StatusOK, "ok")
			}

			if err := g.RequireAuth()(next)(c); err != nil {
				t.Fatalf("RequireAuth: unexpected error %v", err)
			}
			// The request authenticates AS the target, with the staff member
			// recorded alongside rather than in place of them.
			if gotUser != targetID {
				t.Errorf("UserID(c) = %v, want the target %v", gotUser, targetID)
			}
			if gotActor != actorID {
				t.Errorf("ActorID(c) = %v, want the actor %v", gotActor, actorID)
			}
			if !gotImpersonated {
				t.Error("IsImpersonated(c) = false, want true")
			}
		})
	}
}

// An ordinary token must leave both impersonation values at their zero
// state, so nothing downstream can mistake a normal request for an
// impersonated one.
func TestRequireAuth_OrdinaryTokenIsNotImpersonated(t *testing.T) {
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
	// A POST, to prove the read-only rule does not fire for a normal token.
	c, _ := newTestContext(http.MethodPost, "/", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	var gotActor uuid.UUID
	var gotImpersonated bool
	next := func(c echo.Context) error {
		gotActor, gotImpersonated = ActorID(c), IsImpersonated(c)
		return c.String(http.StatusOK, "ok")
	}

	if err := g.RequireAuth()(next)(c); err != nil {
		t.Fatalf("RequireAuth: unexpected error %v", err)
	}
	if gotImpersonated {
		t.Error("IsImpersonated(c) = true for an ordinary token")
	}
	if gotActor != uuid.Nil {
		t.Errorf("ActorID(c) = %v, want uuid.Nil for an ordinary token", gotActor)
	}
}
