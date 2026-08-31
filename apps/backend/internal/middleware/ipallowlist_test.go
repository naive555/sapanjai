package middleware

import (
	"net"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("net.ParseCIDR(%q): %v", s, err)
	}
	return n
}

// AdminIPAllowlist is applied to the /admin GROUP in server.go, meaning it
// runs before any RequireAuth/RequirePlatformRole middleware in the chain —
// these tests assert that ordering has teeth by proving `next` (which
// stands in for the rest of the chain, auth included) is never invoked for
// a rejected request.
func TestAdminIPAllowlist_EmptyDisablesCheck(t *testing.T) {
	called := false
	next := func(c echo.Context) error { called = true; return c.String(http.StatusOK, "ok") }

	c, rec := newTestContext(http.MethodGet, "/admin/me", nil)
	err := AdminIPAllowlist(nil)(next)(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Error("next was not called; an empty allowlist must disable the check")
	}
}

func TestAdminIPAllowlist_OffCIDR_Rejects404BeforeNext(t *testing.T) {
	// httptest.NewRequest defaults RemoteAddr to 192.0.2.1:1234 (TEST-NET-1,
	// RFC 5737) — deliberately outside the allowlist below so this exercises
	// the real c.RealIP() resolution, not a stubbed header.
	next := func(c echo.Context) error {
		t.Fatal("next must not be called for an off-allowlist IP")
		return nil
	}

	c, _ := newTestContext(http.MethodGet, "/admin/me", map[string]string{
		"Authorization": "Bearer some-token-that-must-never-be-inspected",
	})
	err := AdminIPAllowlist([]*net.IPNet{mustCIDR(t, "10.0.0.0/8")})(next)(c)

	// The middleware's own raw error carries echo's generic 404 message
	// ("Not Found", from echo.NewHTTPError's default); server.go's global
	// HTTPErrorHandler is what normalizes any 404 to the contract's
	// "Route not found" wire message. That normalization is exercised
	// end-to-end in internal/server/admin_ip_allowlist_integration_test.go,
	// which is where "an off-network scanner can't tell /admin exists" is
	// actually proven against the real response body — what matters here is
	// the status code and that `next` (standing in for the entire rest of
	// the chain, RequireAuth included) never runs.
	assertHTTPError(t, err, http.StatusNotFound, "Not Found")
}

func TestAdminIPAllowlist_OnCIDR_CallsNext(t *testing.T) {
	called := false
	next := func(c echo.Context) error { called = true; return c.String(http.StatusOK, "ok") }

	c, rec := newTestContext(http.MethodGet, "/admin/me", nil)
	err := AdminIPAllowlist([]*net.IPNet{mustCIDR(t, "192.0.2.0/24")})(next)(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Error("next was not called for an in-allowlist IP")
	}
}
