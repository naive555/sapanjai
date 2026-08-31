package middleware

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

func newTestGuardsForPlatformRole(t *testing.T, userID uuid.UUID, getUserByID func(ctx context.Context, id uuid.UUID) (db.User, error)) *Guards {
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
		&mockMembershipStore{getUserByID: getUserByID},
		&mockPermissionChecker{},
	)
}

// newTestGuardsForTwoFactor is newTestGuardsForPlatformRole plus control
// over IsTwoFactorVerified, for the Task 6.3/6.4 step-up tests below.
func newTestGuardsForTwoFactor(
	t *testing.T,
	userID uuid.UUID,
	getUserByID func(ctx context.Context, id uuid.UUID) (db.User, error),
	isTwoFactorVerified func(ctx context.Context, userID uuid.UUID) (bool, error),
) *Guards {
	t.Helper()
	g := NewGuards(
		&mockTokenVerifier{
			verify: func(token string) (uuid.UUID, string, error) {
				return userID, "user@example.com", nil
			},
		},
		&mockBlacklist{
			isBlacklisted:       func(ctx context.Context, token string) (bool, error) { return false, nil },
			isTwoFactorVerified: isTwoFactorVerified,
		},
		&mockMembershipStore{getUserByID: getUserByID},
		&mockPermissionChecker{},
	)
	g.SetAdminRequire2FA(true)
	return g
}

func strPtr(s string) *string { return &s }

func TestRequirePlatformRole_MissingToken(t *testing.T) {
	g := NewGuards(&mockTokenVerifier{}, &mockBlacklist{}, &mockMembershipStore{}, &mockPermissionChecker{})
	c, _ := newTestContext(http.MethodGet, "/admin/me", nil)

	err := g.RequirePlatformRole("superadmin", "support")(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
}

func TestRequirePlatformRole_UserRowMissing(t *testing.T) {
	// A user id with no row (deleted mid-session) is 401, not 500.
	g := newTestGuardsForPlatformRole(t, uuid.New(), func(ctx context.Context, id uuid.UUID) (db.User, error) {
		return db.User{}, pgx.ErrNoRows
	})
	c, _ := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequirePlatformRole("superadmin", "support")(okNext)(c)
	assertHTTPError(t, err, http.StatusUnauthorized, "Unauthorized")
}

func TestRequirePlatformRole_LookupErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	g := newTestGuardsForPlatformRole(t, uuid.New(), func(ctx context.Context, id uuid.UUID) (db.User, error) {
		return db.User{}, dbErr
	})
	c, _ := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequirePlatformRole("superadmin", "support")(okNext)(c)
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

func TestRequirePlatformRole_NoPlatformRoleDenied(t *testing.T) {
	// An ordinary tenant user (platform_role NULL) is denied, and the
	// message must be indistinguishable from a wrong-role denial so the
	// console cannot be probed for which platform roles exist.
	userID := uuid.New()
	g := newTestGuardsForPlatformRole(t, userID, func(ctx context.Context, id uuid.UUID) (db.User, error) {
		return db.User{ID: id, PlatformRole: nil}, nil
	})
	c, _ := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequirePlatformRole("superadmin", "support")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Insufficient permissions")
}

func TestRequirePlatformRole_WrongRoleDenied(t *testing.T) {
	userID := uuid.New()
	g := newTestGuardsForPlatformRole(t, userID, func(ctx context.Context, id uuid.UUID) (db.User, error) {
		return db.User{ID: id, PlatformRole: strPtr("support")}, nil
	})
	c, _ := newTestContext(http.MethodDelete, "/admin/organizations/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	// A route restricted to superadmin-only must deny a support-role caller.
	err := g.RequirePlatformRole("superadmin")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Insufficient permissions")
}

func TestRequirePlatformRole_SupportAllowed(t *testing.T) {
	userID := uuid.New()
	g := newTestGuardsForPlatformRole(t, userID, func(ctx context.Context, id uuid.UUID) (db.User, error) {
		return db.User{ID: id, PlatformRole: strPtr("support")}, nil
	})
	c, rec := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	var gotRole string
	next := func(c echo.Context) error {
		gotRole = PlatformRole(c)
		return c.String(http.StatusOK, "ok")
	}

	if err := g.RequirePlatformRole("superadmin", "support")(next)(c); err != nil {
		t.Fatalf("RequirePlatformRole: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotRole != "support" {
		t.Errorf("PlatformRole(c) = %q, want %q", gotRole, "support")
	}
}

func TestRequirePlatformRole_SuperadminAllowed(t *testing.T) {
	userID := uuid.New()
	g := newTestGuardsForPlatformRole(t, userID, func(ctx context.Context, id uuid.UUID) (db.User, error) {
		return db.User{ID: id, PlatformRole: strPtr("superadmin")}, nil
	})
	c, rec := newTestContext(http.MethodDelete, "/admin/organizations/"+uuid.NewString(), map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	var gotRole string
	next := func(c echo.Context) error {
		gotRole = PlatformRole(c)
		return c.String(http.StatusOK, "ok")
	}

	if err := g.RequirePlatformRole("superadmin")(next)(c); err != nil {
		t.Fatalf("RequirePlatformRole: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotRole != "superadmin" {
		t.Errorf("PlatformRole(c) = %q, want %q", gotRole, "superadmin")
	}
}

// ---- Impersonation cannot reach the admin console (Phase 4, Task 4.3) ----

// The escalation this closes: impersonation refuses a target who holds a
// platform_role AT ISSUANCE, but platform_role is a mutable row. Promote
// the impersonated user during the token's 10-minute life and a check that
// only consulted the database would now say "yes, superadmin" — handing the
// impersonator a staff session. RequirePlatformRole therefore refuses on
// the token's own immutable imp claim, before the row is ever read.
func TestRequirePlatformRole_RejectsImpersonationTokenEvenWhenTargetIsStaff(t *testing.T) {
	targetID, actorID := uuid.New(), uuid.New()
	g := impersonationGuards(targetID, actorID, &mockMembershipStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			t.Fatal("GetUserByID must not be reached: the imp claim decides this before any row is read")
			return db.User{}, nil
		},
	})
	// GET, so the read-only rule in verify() is NOT what rejects this —
	// otherwise the test would pass for the wrong reason.
	c, _ := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer impersonation.jwt.token",
	})

	err := g.RequirePlatformRole("superadmin", "support")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Insufficient permissions")
}

// ---- end impersonation tests ----

// ---- TOTP step-up (Phase 6, Task 6.3/6.4) ----

// TestRequirePlatformRole_TwoFactorRequired_NoLiveKey confirms
// ADMIN_REQUIRE_2FA=true (SetAdminRequire2FA(true)) rejects a caller with a
// valid role but no live admin:2fa:<userId> Redis key.
func TestRequirePlatformRole_TwoFactorRequired_NoLiveKey(t *testing.T) {
	userID := uuid.New()
	g := newTestGuardsForTwoFactor(t, userID,
		func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, PlatformRole: strPtr("superadmin")}, nil
		},
		func(ctx context.Context, gotUserID uuid.UUID) (bool, error) {
			if gotUserID != userID {
				t.Errorf("IsTwoFactorVerified called with %v, want %v", gotUserID, userID)
			}
			return false, nil
		},
	)
	c, _ := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequirePlatformRole("superadmin", "support")(okNext)(c)
	assertHTTPError(t, err, http.StatusForbidden, "Two-factor authentication required")
}

// TestRequirePlatformRole_TwoFactorSatisfied_Allowed is the same setup with
// a live key: the request must proceed.
func TestRequirePlatformRole_TwoFactorSatisfied_Allowed(t *testing.T) {
	userID := uuid.New()
	g := newTestGuardsForTwoFactor(t, userID,
		func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, PlatformRole: strPtr("superadmin")}, nil
		},
		func(ctx context.Context, _ uuid.UUID) (bool, error) { return true, nil },
	)
	c, rec := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	if err := g.RequirePlatformRole("superadmin", "support")(okNext)(c); err != nil {
		t.Fatalf("RequirePlatformRole: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestRequirePlatformRole_TwoFactorNotEnforced_WhenConfigDisabled confirms
// SetAdminRequire2FA(false) — ADMIN_REQUIRE_2FA=false, the local-dev
// setting — bypasses the 2FA check cleanly even when IsTwoFactorVerified
// would say no. IsTwoFactorVerified must not even be called: local dev
// shouldn't need a working Redis 2FA cache to boot the console.
func TestRequirePlatformRole_TwoFactorNotEnforced_WhenConfigDisabled(t *testing.T) {
	userID := uuid.New()
	g := NewGuards(
		&mockTokenVerifier{verify: func(token string) (uuid.UUID, string, error) { return userID, "user@example.com", nil }},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
			isTwoFactorVerified: func(ctx context.Context, _ uuid.UUID) (bool, error) {
				t.Fatal("IsTwoFactorVerified must not be called when ADMIN_REQUIRE_2FA is false")
				return false, nil
			},
		},
		&mockMembershipStore{getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, PlatformRole: strPtr("superadmin")}, nil
		}},
		&mockPermissionChecker{},
	)
	// SetAdminRequire2FA deliberately NOT called: the zero value (false) is
	// what every deployment gets unless server.go opts in from
	// ADMIN_REQUIRE_2FA, so this test also stands in for "never configured."
	c, rec := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	if err := g.RequirePlatformRole("superadmin", "support")(okNext)(c); err != nil {
		t.Fatalf("RequirePlatformRole: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestRequirePlatformRoleNo2FA_BypassesStepUp exercises the guard the three
// /admin/2fa/{enroll,confirm,verify} routes use: it must succeed with no
// live 2FA key at all, and must never even ask — that's the escape from the
// chicken-and-egg the guard's doc comment names.
func TestRequirePlatformRoleNo2FA_BypassesStepUp(t *testing.T) {
	userID := uuid.New()
	g := NewGuards(
		&mockTokenVerifier{verify: func(token string) (uuid.UUID, string, error) { return userID, "user@example.com", nil }},
		&mockBlacklist{
			isBlacklisted: func(ctx context.Context, token string) (bool, error) { return false, nil },
			isTwoFactorVerified: func(ctx context.Context, _ uuid.UUID) (bool, error) {
				t.Fatal("IsTwoFactorVerified must not be called by RequirePlatformRoleNo2FA")
				return false, nil
			},
		},
		&mockMembershipStore{getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, PlatformRole: strPtr("superadmin")}, nil
		}},
		&mockPermissionChecker{},
	)
	g.SetAdminRequire2FA(true)
	c, rec := newTestContext(http.MethodPost, "/admin/2fa/enroll", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	if err := g.RequirePlatformRoleNo2FA("superadmin", "support")(okNext)(c); err != nil {
		t.Fatalf("RequirePlatformRoleNo2FA: unexpected error %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestRequirePlatformRole_DemotedStaffRejectedEvenWithLive2FAKey is the
// test the execution plan's Task 6.3 explicitly asks for: the 12h
// admin:2fa:<userId> Redis key is NOT revoked when ChangePlatformRole
// demotes a staff member (internal/module/admin/service.go's SetBan/
// ChangePlatformRole doc comments state this is deliberate). This proves
// the documented mitigation actually holds at the one place that matters —
// IsTwoFactorVerified returning true for a stale key must NOT be enough to
// reach an admin route once platform_role is gone: the role check runs
// first and rejects before the 2FA check is ever consulted.
//
// If this test ever fails, the "role check fails first" argument in
// docs/11-admin-panel.md and the execution plan is wrong and the stale key
// needs active revocation, not just a comment explaining why it doesn't.
func TestRequirePlatformRole_DemotedStaffRejectedEvenWithLive2FAKey(t *testing.T) {
	userID := uuid.New()
	g := newTestGuardsForTwoFactor(t, userID,
		// Demoted: platform_role is now nil, exactly what ChangePlatformRole
		// writes on revoke.
		func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, PlatformRole: nil}, nil
		},
		// The stale key from before the demotion is still live in Redis —
		// ChangePlatformRole never touched it.
		func(ctx context.Context, _ uuid.UUID) (bool, error) { return true, nil },
	)
	c, _ := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer valid.jwt.token",
	})

	err := g.RequirePlatformRole("superadmin", "support")(okNext)(c)
	// Must be the role-check's "Insufficient permissions", never the
	// 2FA-check's "Two-factor authentication required" — the latter would
	// mean the guard reached the 2FA branch at all, which is exactly what
	// must not happen for a demoted user.
	assertHTTPError(t, err, http.StatusForbidden, "Insufficient permissions")
}
