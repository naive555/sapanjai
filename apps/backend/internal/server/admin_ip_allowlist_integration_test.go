package server_test

import (
	"net"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/config"
)

// Task 6.2/6.4: ADMIN_IP_ALLOWLIST end to end against the real wired
// server. httptest.NewServer listens on 127.0.0.1, and no test client here
// ever sets X-Forwarded-For/X-Real-IP (mirroring apps/frontend's route.ts,
// which now strips both unconditionally — see its own comment and
// server.go's e.IPExtractor comment), so c.RealIP() resolves to the raw
// loopback peer address for every request in this file. That is exactly
// the "no trustworthy per-client signal once headers are refused"
// conclusion those two files document — these tests exercise the allowlist
// itself, not a spoofed-header scenario (internal/middleware/
// ipallowlist_test.go already covers "does the allowlist actually gate" as
// a pure unit test; this file's job is proving the group wiring in
// server.go puts it before RequireAuth in the real request path).

func mustParseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("net.ParseCIDR(%q): %v", s, err)
	}
	return cidr
}

func TestIntegration_Admin_IPAllowlist_OffCIDR_Rejects404BeforeAuth(t *testing.T) {
	ts, _, _ := setupTestServer(t, func(cfg *config.Config) {
		// 10.0.0.0/8 never matches the loopback peer httptest listens on.
		cfg.AdminIPAllowlist = []*net.IPNet{mustParseCIDR(t, "10.0.0.0/8")}
	})
	client := ts.Client()

	// No Authorization header at all: if the allowlist ran after
	// RequireAuth this would be indistinguishable from an ordinary 401, so
	// the assertion below (exactly 404, with the contract's normalized
	// message) is what proves the ordering.
	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
	}
	if msg, _ := body["message"].(string); msg != "Route not found" {
		t.Errorf("message = %q, want %q — an off-network caller must not learn /admin exists", msg, "Route not found")
	}
}

func TestIntegration_Admin_IPAllowlist_Empty_DisablesCheck(t *testing.T) {
	// setupTestServer's cfg starts with AdminIPAllowlist unset (nil) unless
	// a configure func sets it — this is Task 6.2's required default.
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	// No Authorization header, no allowlist: this must reach RequireAuth
	// and fail there (401), never be shortcut to 404.
	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (the allowlist must be disabled, not gate this request); body = %v", resp.StatusCode, body)
	}
}

func TestIntegration_Admin_IPAllowlist_OnCIDR_ReachesAuth(t *testing.T) {
	ts, _, store := setupTestServer(t, func(cfg *config.Config) {
		cfg.AdminIPAllowlist = []*net.IPNet{mustParseCIDR(t, "127.0.0.0/8")}
	})
	client := ts.Client()

	superUser := registerUser(t, client, ts.URL, "ip-allowlist-onlist")
	promoteToPlatformRole(t, store, uuid.MustParse(superUser.UserID), "superadmin")

	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil,
		map[string]string{"Authorization": "Bearer " + superUser.AccessToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an on-allowlist caller with a valid superadmin token); body = %v", resp.StatusCode, body)
	}
}
