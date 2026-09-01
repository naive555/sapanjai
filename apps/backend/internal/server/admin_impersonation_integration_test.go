package server_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Impersonation end-to-end (execution plan Phase 4 Task 4.5,
// docs/11-admin-panel.md §5). The containment story is only real if each
// of these holds against the actual wired server, not just the unit mocks.

// startImpersonation mints a token as staffToken for targetUserID and
// returns the token string.
func startImpersonation(t *testing.T, client *http.Client, baseURL, staffToken, targetUserID, reason string) string {
	t.Helper()

	resp, body := doJSON(t, client, baseURL, http.MethodPost,
		"/admin/users/"+targetUserID+"/impersonate",
		map[string]any{"reason": reason},
		map[string]string{"Authorization": "Bearer " + staffToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("impersonate: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	token, _ := body["accessToken"].(string)
	if token == "" {
		t.Fatalf("impersonate: missing accessToken: %v", body)
	}
	return token
}

func TestIntegration_Admin_Impersonation(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	superHeaders := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("both staff roles may impersonate", func(t *testing.T) {
		// Support reads everything and mutates nothing; impersonation is a
		// read, so it is on the read guard (docs/11-admin-panel.md §4).
		for _, staff := range []struct {
			role  string
			token string
		}{
			{"superadmin", fx.superUser.AccessToken},
			{"support", fx.supportU.AccessToken},
		} {
			t.Run(staff.role, func(t *testing.T) {
				target := createOrgWithOwner(t, client, ts.URL, "imp-"+staff.role)
				resp, body := doJSON(t, client, ts.URL, http.MethodPost,
					"/admin/users/"+target.Owner.UserID+"/impersonate",
					map[string]any{"reason": "reproducing their reported 401"},
					map[string]string{"Authorization": "Bearer " + staff.token})
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
				}
				if body["accessToken"] == nil || body["accessToken"] == "" {
					t.Errorf("missing accessToken: %v", body)
				}
				// 10 minutes, expressed in seconds.
				if body["expiresIn"] != 600.0 {
					t.Errorf("expiresIn = %v, want 600", body["expiresIn"])
				}
				// No refresh token, ever: the token cannot be extended,
				// only re-issued through this endpoint.
				if _, ok := body["refreshToken"]; ok {
					t.Errorf("response carries a refreshToken; impersonation tokens are non-refreshable: %v", body)
				}
				user, _ := body["user"].(map[string]any)
				if user["id"] != target.Owner.UserID {
					t.Errorf("user.id = %v, want the target %q", user["id"], target.Owner.UserID)
				}
			})
		}
	})

	t.Run("a tenant user cannot impersonate anyone", func(t *testing.T) {
		target := createOrgWithOwner(t, client, ts.URL, "imp-tenant-denied")
		resp, body := doJSON(t, client, ts.URL, http.MethodPost,
			"/admin/users/"+target.Owner.UserID+"/impersonate",
			map[string]any{"reason": "I would simply like to be an admin"},
			map[string]string{"Authorization": "Bearer " + fx.org.Owner.AccessToken})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("reason is mandatory and must be substantive", func(t *testing.T) {
		target := createOrgWithOwner(t, client, ts.URL, "imp-reason")
		for _, reason := range []any{nil, "", "x", "too short"} {
			resp, body := doJSON(t, client, ts.URL, http.MethodPost,
				"/admin/users/"+target.Owner.UserID+"/impersonate",
				map[string]any{"reason": reason}, superHeaders)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("reason %v: status = %d, want 422; body = %v", reason, resp.StatusCode, body)
			}
		}
	})

	t.Run("staff cannot be impersonated", func(t *testing.T) {
		// Closes impersonation as a privilege-escalation ladder: support
		// must not be able to borrow a superadmin's identity.
		resp, body := doJSON(t, client, ts.URL, http.MethodPost,
			"/admin/users/"+fx.superUser.UserID+"/impersonate",
			map[string]any{"reason": "attempting to escalate to superadmin"},
			map[string]string{"Authorization": "Bearer " + fx.supportU.AccessToken})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Cannot impersonate a platform staff account" {
			t.Errorf("message = %v, want the staff-impersonation refusal", body["message"])
		}
	})

	t.Run("unknown user 404s", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost,
			"/admin/users/"+uuid.NewString()+"/impersonate",
			map[string]any{"reason": "chasing a ghost user id"}, superHeaders)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("the token reads as the target and is scoped to the target's orgs", func(t *testing.T) {
		target := createOrgWithOwner(t, client, ts.URL, "imp-scope")
		token := startImpersonation(t, client, ts.URL, fx.superUser.AccessToken,
			target.Owner.UserID, "checking which orgs they can actually see")
		impHeaders := map[string]string{"Authorization": "Bearer " + token}

		// GET /auth/me resolves to the TARGET, not the staff member.
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/auth/me", nil, impHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/auth/me: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["id"] != target.Owner.UserID {
			t.Errorf("/auth/me id = %v, want the target %q", body["id"], target.Owner.UserID)
		}

		// Scoping is the target's own: their own org resolves...
		ownResp, ownBody := doJSONList(t, client, ts.URL, "/organizations/members",
			map[string]string{"Authorization": "Bearer " + token, "x-organization-id": target.ID})
		if ownResp.StatusCode != http.StatusOK {
			t.Fatalf("target's own org under impersonation: status = %d, want 200; body = %v",
				ownResp.StatusCode, ownBody)
		}

		// ...and the fixture org, which the target does not belong to, does
		// not — impersonation grants the target's reach, not the staff
		// member's.
		// doAdminGet rather than doJSONList here: the refusal body is an
		// error object, not the array the success path returns.
		memberResp, memberRaw := doAdminGet(t, client, ts.URL, "/organizations/members",
			map[string]string{"Authorization": "Bearer " + token, "x-organization-id": fx.org.ID})
		if memberResp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-org read under impersonation: status = %d, want 403; body = %s",
				memberResp.StatusCode, memberRaw)
		}
	})

	t.Run("every unsafe method is refused", func(t *testing.T) {
		target := createOrgWithOwner(t, client, ts.URL, "imp-readonly")
		token := startImpersonation(t, client, ts.URL, fx.superUser.AccessToken,
			target.Owner.UserID, "confirming the read-only rule holds")
		impHeaders := map[string]string{
			"Authorization":     "Bearer " + token,
			"x-organization-id": target.ID,
		}

		for _, tc := range []struct {
			method string
			path   string
			body   any
		}{
			{http.MethodPost, "/organizations", map[string]any{"name": "Sneaky", "slug": uniqueSlug("sneaky")}},
			{http.MethodPost, "/connectors", map[string]any{"name": "x", "type": "generic", "config": map[string]any{}}},
			{http.MethodPost, "/mcp-keys", map[string]any{"name": "sneaky-key"}},
			{http.MethodDelete, "/organizations/members/" + uuid.NewString(), nil},
		} {
			t.Run(tc.method+" "+tc.path, func(t *testing.T) {
				resp, body := doJSON(t, client, ts.URL, tc.method, tc.path, tc.body, impHeaders)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
				}
				if body["message"] != "Impersonated sessions are read-only" {
					t.Errorf("message = %v, want the read-only refusal", body["message"])
				}
			})
		}
	})

	// The one documented exception to "read-only is enforced at the guard,
	// so every route is covered": POST /auth/logout is deliberately
	// unauthenticated (it identifies the session by the refresh token in its
	// body, and must work for a caller whose access token has already
	// expired), so Guards.verify — and with it the read-only rule — never
	// runs on it. That is safe rather than merely tolerable, because
	// logout's destructive half needs the TARGET'S REFRESH TOKEN, which
	// impersonation never issues. This test pins that reasoning: the call
	// succeeds, does nothing, and leaves the target's real session alive.
	t.Run("logout is unguarded but harmless under impersonation", func(t *testing.T) {
		target := createOrgWithOwner(t, client, ts.URL, "imp-logout")
		token := startImpersonation(t, client, ts.URL, fx.superUser.AccessToken,
			target.Owner.UserID, "confirming logout cannot be weaponised")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/logout",
			map[string]any{"refreshToken": "not-the-targets-refresh-token"},
			map[string]string{"Authorization": "Bearer " + token})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("logout: status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		// The target's own session is untouched: the impersonator had no
		// refresh token to revoke it with.
		refreshResp, refreshBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": target.Owner.RefreshToken}, nil)
		if refreshResp.StatusCode != http.StatusOK {
			t.Fatalf("target's session died from an impersonated logout: status = %d; body = %v",
				refreshResp.StatusCode, refreshBody)
		}
	})

	t.Run("the token cannot reach any /admin route", func(t *testing.T) {
		// Even though this target is a plain tenant user, the refusal must
		// come from the token's imp claim rather than from their role — see
		// the unit test in internal/middleware/platform_test.go.
		target := createOrgWithOwner(t, client, ts.URL, "imp-noadmin")
		token := startImpersonation(t, client, ts.URL, fx.superUser.AccessToken,
			target.Owner.UserID, "verifying the console stays out of reach")
		impHeaders := map[string]string{"Authorization": "Bearer " + token}

		for _, path := range []string{"/admin/me", "/admin/users", "/admin/organizations", "/admin/system/stats"} {
			resp, body := doJSON(t, client, ts.URL, http.MethodGet, path, nil, impHeaders)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403; body = %v", path, resp.StatusCode, body)
			}
		}
	})

	t.Run("the token is rejected by /auth/refresh", func(t *testing.T) {
		// There is no sessions row behind an impersonation token, so the
		// refresh path has nothing to rotate — the token cannot be extended,
		// only re-issued (which writes a fresh audit entry).
		target := createOrgWithOwner(t, client, ts.URL, "imp-norefresh")
		token := startImpersonation(t, client, ts.URL, fx.superUser.AccessToken,
			target.Owner.UserID, "confirming it cannot be extended")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": token}, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("the start is audited with its reason", func(t *testing.T) {
		target := createOrgWithOwner(t, client, ts.URL, "imp-audited")
		const reason = "customer reported tools/list is empty for them"
		startImpersonation(t, client, ts.URL, fx.superUser.AccessToken, target.Owner.UserID, reason)

		resp, body := doJSON(t, client, ts.URL, http.MethodGet,
			"/admin/audit-logs?action=admin.impersonation.started&userId="+fx.superUser.UserID,
			nil, superHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("audit query: status = %d; body = %v", resp.StatusCode, body)
		}
		items, _ := body["items"].([]any)

		var found bool
		for _, it := range items {
			row, _ := it.(map[string]any)
			meta, _ := row["metadata"].(map[string]any)
			if meta["targetUserId"] == target.Owner.UserID && meta["reason"] == reason {
				found = true
			}
		}
		if !found {
			t.Fatalf("no admin.impersonation.started entry carrying the target and reason: %v", items)
		}
	})
}
