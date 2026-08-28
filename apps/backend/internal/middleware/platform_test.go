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
