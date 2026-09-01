package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
)

// demotePlatformRole clears userID's platform_role entirely (SQL NULL),
// mirroring what ChangePlatformRole's role=nil path writes. Unlike
// promoteToPlatformRole (admin_integration_test.go), which always takes a
// non-empty role string, this exists because the CHECK constraint from
// migration 00011 rejects "" outright — NULL is the only valid "no role"
// value, and Go's *string can't express that through a bare string param.
func demotePlatformRole(t *testing.T, store *database.Store, userID uuid.UUID) {
	t.Helper()
	if err := store.SetUserPlatformRole(context.Background(), db.SetUserPlatformRoleParams{ID: userID, PlatformRole: nil}); err != nil {
		t.Fatalf("demotePlatformRole(%s): %v", userID, err)
	}
}

// Task 6.3/6.4: TOTP step-up end to end against the real wired server —
// real envelope encryption (CONNECTOR_MASTER_KEY, see setupTestServer),
// real Postgres row, real Redis admin:2fa:<userId>/admin:2fa:attempts:
// keys. The unit tests in internal/module/admin/totp_test.go already cover
// the business-logic edge cases with a fake sealer; this file's job is
// proving the whole stack — migration 00012, sqlc queries, envelope
// encryption, Redis, and the RequirePlatformRole/RequirePlatformRoleNo2FA
// wiring in server.go — actually holds together.

// totpSecretFromOtpauthURI parses the otpauth:// URI POST /admin/2fa/enroll
// returns and pulls out the base32 secret, so the test can act as the
// authenticator app would.
func totpSecretFromOtpauthURI(t *testing.T, uri string) string {
	t.Helper()
	key, err := otp.NewKeyFromURL(uri)
	if err != nil {
		t.Fatalf("otp.NewKeyFromURL(%q): %v", uri, err)
	}
	return key.Secret()
}

func TestIntegration_Admin_TOTP_EnrollConfirmVerify(t *testing.T) {
	ts, _, store := setupTestServer(t, func(cfg *config.Config) {
		cfg.AdminRequire2FA = true
	})
	client := ts.Client()

	staff := registerUser(t, client, ts.URL, "totp-happy-path")
	promoteToPlatformRole(t, store, uuid.MustParse(staff.UserID), "superadmin")
	headers := map[string]string{"Authorization": "Bearer " + staff.AccessToken}

	// Before any step-up: ADMIN_REQUIRE_2FA=true and no admin:2fa:<userId>
	// key yet, so a route OTHER than enroll/confirm/verify must refuse.
	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, headers)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("pre-verify /admin/me: status = %d, want 403; body = %v", resp.StatusCode, body)
	}
	if msg, _ := body["message"].(string); msg != "Two-factor authentication required" {
		t.Errorf("pre-verify /admin/me: message = %q", msg)
	}

	// Enroll must itself be reachable despite ADMIN_REQUIRE_2FA=true and no
	// step-up yet — the chicken-and-egg RequirePlatformRoleNo2FA exists for.
	enrollResp, enrollBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/enroll", nil, headers)
	if enrollResp.StatusCode != http.StatusOK {
		t.Fatalf("enroll: status = %d, want 200; body = %v", enrollResp.StatusCode, enrollBody)
	}
	uri, _ := enrollBody["otpauthUri"].(string)
	if uri == "" {
		t.Fatalf("enroll: missing otpauthUri: %v", enrollBody)
	}
	secret := totpSecretFromOtpauthURI(t, uri)

	confirmCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	confirmResp, confirmBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/confirm",
		map[string]any{"code": confirmCode}, headers)
	if confirmResp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: status = %d, want 200; body = %v", confirmResp.StatusCode, confirmBody)
	}
	recoveryCodesRaw, _ := confirmBody["recoveryCodes"].([]any)
	if len(recoveryCodesRaw) != 10 {
		t.Fatalf("confirm: got %d recovery codes, want 10: %v", len(recoveryCodesRaw), confirmBody)
	}

	// Still gated: confirming does not itself satisfy the step-up, only
	// verify does.
	resp, body = doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, headers)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post-confirm, pre-verify /admin/me: status = %d, want 403; body = %v", resp.StatusCode, body)
	}

	verifyCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	verifyResp, verifyBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/verify",
		map[string]any{"code": verifyCode}, headers)
	if verifyResp.StatusCode != http.StatusOK {
		t.Fatalf("verify: status = %d, want 200; body = %v", verifyResp.StatusCode, verifyBody)
	}

	// Now every other /admin route works for the rest of the 12h window.
	resp, body = doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-verify /admin/me: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
}

// TestIntegration_Admin_TOTP_StaleCodeRejected: Task 6.4's explicit "rejects
// a stale window" requirement, against the real server.
func TestIntegration_Admin_TOTP_StaleCodeRejected(t *testing.T) {
	ts, _, store := setupTestServer(t, func(cfg *config.Config) {
		cfg.AdminRequire2FA = true
	})
	client := ts.Client()

	staff := registerUser(t, client, ts.URL, "totp-stale")
	promoteToPlatformRole(t, store, uuid.MustParse(staff.UserID), "superadmin")
	headers := map[string]string{"Authorization": "Bearer " + staff.AccessToken}

	_, enrollBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/enroll", nil, headers)
	secret := totpSecretFromOtpauthURI(t, enrollBody["otpauthUri"].(string))
	confirmCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	if resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/confirm",
		map[string]any{"code": confirmCode}, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	staleCode, err := totp.GenerateCode(secret, time.Now().Add(-150*time.Second))
	if err != nil {
		t.Fatalf("totp.GenerateCode (stale): %v", err)
	}
	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/verify",
		map[string]any{"code": staleCode}, headers)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("verify with stale code: status = %d, want 401; body = %v", resp.StatusCode, body)
	}

	// The stale attempt must not have satisfied the step-up.
	meResp, meBody := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, headers)
	if meResp.StatusCode != http.StatusForbidden {
		t.Fatalf("/admin/me after a stale verify attempt: status = %d, want 403; body = %v", meResp.StatusCode, meBody)
	}
}

// TestIntegration_Admin_TOTP_RecoveryCodeAcceptedOnce: Task 6.4's other
// explicit requirement, against the real server.
func TestIntegration_Admin_TOTP_RecoveryCodeAcceptedOnce(t *testing.T) {
	ts, _, store := setupTestServer(t, func(cfg *config.Config) {
		cfg.AdminRequire2FA = true
	})
	client := ts.Client()

	staff := registerUser(t, client, ts.URL, "totp-recovery")
	promoteToPlatformRole(t, store, uuid.MustParse(staff.UserID), "superadmin")
	headers := map[string]string{"Authorization": "Bearer " + staff.AccessToken}

	_, enrollBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/enroll", nil, headers)
	secret := totpSecretFromOtpauthURI(t, enrollBody["otpauthUri"].(string))
	confirmCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	_, confirmBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/confirm",
		map[string]any{"code": confirmCode}, headers)
	recoveryCodesRaw, _ := confirmBody["recoveryCodes"].([]any)
	if len(recoveryCodesRaw) == 0 {
		t.Fatalf("confirm: no recovery codes returned: %v", confirmBody)
	}
	recoveryCode, _ := recoveryCodesRaw[0].(string)

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/verify",
		map[string]any{"code": recoveryCode}, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify with a fresh recovery code: status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/verify",
		map[string]any{"code": recoveryCode}, headers)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("verify with an already-used recovery code: status = %d, want 401; body = %v", resp.StatusCode, body)
	}
}

// TestIntegration_Admin_RequireTwoFA_False_BypassesCleanly: Task 6.4's
// "ADMIN_REQUIRE_2FA=false bypasses cleanly" requirement.
func TestIntegration_Admin_RequireTwoFA_False_BypassesCleanly(t *testing.T) {
	ts, _, store := setupTestServer(t, func(cfg *config.Config) {
		cfg.AdminRequire2FA = false
	})
	client := ts.Client()

	staff := registerUser(t, client, ts.URL, "totp-disabled")
	promoteToPlatformRole(t, store, uuid.MustParse(staff.UserID), "superadmin")
	headers := map[string]string{"Authorization": "Bearer " + staff.AccessToken}

	// No enroll/confirm/verify at all — every other /admin route must work
	// immediately.
	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/me with ADMIN_REQUIRE_2FA=false: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
}

// TestIntegration_Admin_TOTP_DemotionLeavesStaleKeyUnreachable is the test
// the execution plan's Task 6.3 explicitly demands: the 12h
// admin:2fa:<userId> Redis key is not revoked on demotion, and the plan's
// argument for why that is safe is that demotion revokes sessions, killing
// the access token, so the stale key is unreachable without a fresh login —
// at which point the role check fails first. This proves that chain against
// the real server: the SAME still-live access token that was used to
// satisfy step-up must be refused the instant platform_role is gone,
// without needing to wait for the token to expire or the 2FA key to.
func TestIntegration_Admin_TOTP_DemotionLeavesStaleKeyUnreachable(t *testing.T) {
	ts, _, store := setupTestServer(t, func(cfg *config.Config) {
		cfg.AdminRequire2FA = true
	})
	client := ts.Client()

	staff := registerUser(t, client, ts.URL, "totp-demoted")
	promoteToPlatformRole(t, store, uuid.MustParse(staff.UserID), "superadmin")
	headers := map[string]string{"Authorization": "Bearer " + staff.AccessToken}

	_, enrollBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/enroll", nil, headers)
	secret := totpSecretFromOtpauthURI(t, enrollBody["otpauthUri"].(string))
	confirmCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	if resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/confirm",
		map[string]any{"code": confirmCode}, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	verifyCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	if resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/2fa/verify",
		map[string]any{"code": verifyCode}, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("verify: status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	// Confirm step-up actually took: this same token now reaches /admin/me.
	if resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-demotion /admin/me: status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	// Demote directly through the store (mirroring `make admin-grant
	// ROLE=none`) rather than through
	// PATCH /admin/users/:userId/platform-role, which needs a second
	// superadmin and a password — this test is only about what
	// RequirePlatformRole does once platform_role is gone, not about
	// ChangePlatformRole's own mutation path (covered by
	// admin_mutations_integration_test.go).
	demotePlatformRole(t, store, uuid.MustParse(staff.UserID))

	// The exact same access token, still unexpired, with the 2FA Redis key
	// still live (nothing here ever cleared it) must now be refused. If
	// this returns 200 or the 2FA-required 403 instead of a role-check
	// 403, the "role check fails first" argument this test exists to pin
	// does not hold.
	resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/me", nil, headers)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post-demotion /admin/me: status = %d, want 403; body = %v", resp.StatusCode, body)
	}
	if msg, _ := body["message"].(string); msg != "Insufficient permissions" {
		t.Errorf("post-demotion /admin/me: message = %q, want %q (must be the role-check's message, not the 2FA one — proves the role check ran and rejected FIRST, never reaching the still-live 2FA key)", msg, "Insufficient permissions")
	}
}
