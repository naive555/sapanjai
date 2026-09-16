package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// adminMutationRoute is one Phase 3 mutation route, generic enough that the
// auth-gating table below can drive all eight without a route-specific
// happy-path assertion — the dedicated per-route tests further down cover
// that half, since each route's "it worked" check needs its own fixture.
type adminMutationRoute struct {
	name   string
	method string
	path   string
	body   map[string]any
}

// adminMutationRoutes lists every Phase 3 mutation route with a body shape
// that parses (its content doesn't matter for auth gating — RequirePlatformRole
// runs as middleware, before any handler ever binds a body).
func adminMutationRoutes(fx adminTestFixture, planID string) []adminMutationRoute {
	return []adminMutationRoute{
		{"assign plan", http.MethodPost, "/admin/organizations/" + fx.org.ID + "/plan",
			map[string]any{"planId": planID}},
		{"set limits", http.MethodPut, "/admin/organizations/" + fx.org.ID + "/limits",
			map[string]any{"customLimits": map[string]any{"max_members": 10}}},
		{"delete org", http.MethodDelete, "/admin/organizations/" + fx.org.ID,
			map[string]any{"confirm": "not-the-real-slug", "password": "password123"}},
		{"change platform role", http.MethodPatch, "/admin/users/" + fx.memberID + "/platform-role",
			map[string]any{"role": "support", "password": "password123"}},
		{"ban", http.MethodPatch, "/admin/users/" + fx.memberID + "/ban",
			map[string]any{"banned": true, "password": "password123"}},
		{"create plan", http.MethodPost, "/admin/plans",
			map[string]any{"name": "gating-" + uuid.NewString(), "limits": validPlanLimits(1, 1, 1, 1)}},
		{"update plan", http.MethodPut, "/admin/plans/" + planID,
			map[string]any{"name": "gating-" + uuid.NewString(), "limits": validPlanLimits(1, 1, 1, 1)}},
		{"delete plan", http.MethodDelete, "/admin/plans/" + planID, nil},
		// plan_prices (billing plan step 8). Both are superadmin-only for
		// the same reason every other row here is: pricing is a
		// money-changing decision, and support reads it (GET
		// /admin/plans/:planId/prices is on the read guard and is covered
		// by TestIntegration_Admin_ReadRoutes' "200 for both platform
		// roles").
		{"create plan price", http.MethodPost, "/admin/plans/" + fx.planID + "/prices",
			map[string]any{"stripePriceId": "price_gating", "unitAmount": 100, "currency": "thb", "interval": "month"}},
		{"set plan price active", http.MethodPatch, "/admin/plans/" + fx.planID + "/prices/" + fx.priceID,
			map[string]any{"active": false}},
	}
}

// validPlanLimits is a limits blob that satisfies requiredPlanLimitKeys in
// full. max_tool_calls_per_month joined that list in billing plan step 8
// (admin.requiredPlanLimitKeys' doc comment: it is enforced on the MCP
// gateway hot path, and a missing key means UNLIMITED), so every
// POST/PUT /admin/plans body in this file has to carry it or take a 422.
func validPlanLimits(members, roles, connectors, toolCalls int) map[string]any {
	return map[string]any{
		"max_members":              members,
		"max_roles":                roles,
		"max_connectors":           connectors,
		"max_tool_calls_per_month": toolCalls,
	}
}

// TestIntegration_Admin_Mutations_AuthGating asserts the one rule the whole
// mutation surface hangs on (docs/11-admin-panel.md §4): support reads
// everything and mutates nothing. None of these requests are expected to
// reach the service layer — RequirePlatformRole rejects all three
// unauthorized/under-privileged cases before a body is ever bound, which is
// exactly why the request bodies above don't need to be individually valid.
func TestIntegration_Admin_Mutations_AuthGating(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	planID := createPlan(t, store, map[string]int{"max_members": 5, "max_roles": 5, "max_connectors": 5}).String()
	routes := adminMutationRoutes(fx, planID)

	t.Run("401 unauthenticated", func(t *testing.T) {
		for _, r := range routes {
			t.Run(r.name, func(t *testing.T) {
				resp, body := doJSON(t, client, ts.URL, r.method, r.path, r.body, nil)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
				}
			})
		}
	})

	t.Run("403 for a tenant user with no platform_role", func(t *testing.T) {
		headers := map[string]string{"Authorization": "Bearer " + fx.org.Owner.AccessToken}
		for _, r := range routes {
			t.Run(r.name, func(t *testing.T) {
				resp, body := doJSON(t, client, ts.URL, r.method, r.path, r.body, headers)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
				}
			})
		}
	})

	t.Run("403 for a support account", func(t *testing.T) {
		headers := map[string]string{"Authorization": "Bearer " + fx.supportU.AccessToken}
		for _, r := range routes {
			t.Run(r.name, func(t *testing.T) {
				resp, body := doJSON(t, client, ts.URL, r.method, r.path, r.body, headers)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
				}
				if body["message"] != "Insufficient permissions" {
					t.Errorf("message = %v, want %q (support must not learn it almost had access)", body["message"], "Insufficient permissions")
				}
			})
		}
	})
}

// ---- Organization mutations ----

func TestIntegration_Admin_AssignPlan(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	newPlanID := createPlan(t, store, map[string]int{"max_members": 42, "max_roles": 5, "max_connectors": 5})

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/organizations/"+fx.org.ID+"/plan",
		map[string]any{"planId": newPlanID.String()}, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
	}

	_, detail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+fx.org.ID, nil, headers)
	if detail["effectiveLimits"] == nil {
		t.Fatalf("effectiveLimits missing after plan assignment: %v", detail)
	}
	limits, _ := detail["effectiveLimits"].(map[string]any)
	if limits["max_members"] != 42.0 {
		t.Errorf("max_members = %v, want 42 (from the newly assigned plan)", limits["max_members"])
	}
}

func TestIntegration_Admin_SetOrgLimits(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("happy path overrides the plan's limits", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/organizations/"+fx.org.ID+"/limits",
			map[string]any{"customLimits": map[string]any{"max_members": 999}}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		_, detail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+fx.org.ID, nil, headers)
		limits, _ := detail["effectiveLimits"].(map[string]any)
		if limits["max_members"] != 999.0 {
			t.Errorf("max_members = %v, want 999 (custom_limits overriding the plan)", limits["max_members"])
		}
	})

	t.Run("null clears back to plan-only limits", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/organizations/"+fx.org.ID+"/limits",
			map[string]any{"customLimits": nil}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		_, detail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+fx.org.ID, nil, headers)
		limits, _ := detail["effectiveLimits"].(map[string]any)
		if limits["max_members"] == 999.0 {
			t.Errorf("max_members still 999 after clearing custom_limits: %v", limits)
		}
	})

	t.Run("404 for an organization with no subscription at all", func(t *testing.T) {
		noPlanOrg := createOrgWithOwner(t, client, ts.URL, "admin-nolimits")
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/organizations/"+noPlanOrg.ID+"/limits",
			map[string]any{"customLimits": map[string]any{"max_members": 1}}, headers)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
	})

	// A non-integer value must be refused at the edge rather than stored.
	// subscription.Service.EffectiveLimits unmarshals custom_limits into
	// map[string]float64 and, when that fails, drops the whole override
	// instead of the one bad key — so persisting this would silently
	// disable max_members below too, while the console kept showing both
	// as set. 422 here is what keeps the stored blob and the enforced
	// limits from ever disagreeing.
	t.Run("422 for a non-integer custom limit, and nothing is persisted", func(t *testing.T) {
		valid := map[string]any{"max_members": 42}
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/organizations/"+fx.org.ID+"/limits",
			map[string]any{"customLimits": valid}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("seed valid limits: status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		for _, bad := range []map[string]any{
			{"max_members": "ten"},
			{"max_members": 2.5},
			{"max_members": 25, "max_roles": true},
		} {
			resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/organizations/"+fx.org.ID+"/limits",
				map[string]any{"customLimits": bad}, headers)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("customLimits %v: status = %d, want 422; body = %v", bad, resp.StatusCode, body)
			}
		}

		// The previously-stored override survived every rejected write.
		_, detail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+fx.org.ID, nil, headers)
		limits, _ := detail["effectiveLimits"].(map[string]any)
		if limits["max_members"] != 42.0 {
			t.Errorf("max_members = %v, want 42 — a rejected write must not disturb the stored override", limits["max_members"])
		}
	})

	// The mirror of the plan-limits rule: custom_limits is a partial
	// overlay, so a single-key blob is valid here even though the same
	// blob would be rejected by POST /admin/plans for missing max_roles
	// and max_connectors. Overriding one limit and inheriting the rest is
	// the normal reason to set a custom limit at all.
	t.Run("a single-key override is accepted and inherits the rest from the plan", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/organizations/"+fx.org.ID+"/limits",
			map[string]any{"customLimits": map[string]any{"max_connectors": 77}}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		_, detail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+fx.org.ID, nil, headers)
		limits, _ := detail["effectiveLimits"].(map[string]any)
		if limits["max_connectors"] != 77.0 {
			t.Errorf("max_connectors = %v, want 77", limits["max_connectors"])
		}
		if _, ok := limits["max_members"]; !ok {
			t.Errorf("max_members missing from effective limits: a partial override must still inherit the plan's other keys: %v", limits)
		}
	})
}

func TestIntegration_Admin_DeleteOrganization(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("confirm must equal the org's own slug", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "admin-delete-mismatch")
		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/admin/organizations/"+org.ID,
			map[string]any{"confirm": "definitely-wrong", "password": "password123"}, headers)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Confirmation does not match the organization's slug" {
			t.Errorf("message = %v, want the confirm-mismatch message", body["message"])
		}

		// Side effect must not have happened: the org still resolves.
		getResp, _ := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+org.ID, nil, headers)
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("org disappeared after a rejected delete: status = %d", getResp.StatusCode)
		}
	})

	t.Run("wrong password rejects with REAUTH_FAILED before deleting, and locks out after 5", func(t *testing.T) {
		// A dedicated superadmin for this subtest: it deliberately exhausts
		// the reauth limiter, which is keyed by the CALLING admin's own id
		// (internal/infra/redis/auth.go's admin:reauth:attempts:<userId>) —
		// sharing fx.superUser here would leave its budget burned for every
		// other subtest in this test function that still needs a
		// successful reauth.
		lockoutAdmin := registerUser(t, client, ts.URL, "admin-delete-lockout")
		promoteToPlatformRole(t, store, uuid.MustParse(lockoutAdmin.UserID), "superadmin")
		lockoutHeaders := map[string]string{"Authorization": "Bearer " + lockoutAdmin.AccessToken}

		org := createOrgWithOwner(t, client, ts.URL, "admin-delete-reauth")
		_, orgDetail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+org.ID, nil, lockoutHeaders)
		slug, _ := orgDetail["slug"].(string)
		if slug == "" {
			t.Fatalf("could not read the org's own slug: %v", orgDetail)
		}

		for i := 0; i < 5; i++ {
			resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/admin/organizations/"+org.ID,
				map[string]any{"confirm": slug, "password": "not-the-password"}, lockoutHeaders)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("attempt %d: status = %d, want 403; body = %v", i+1, resp.StatusCode, body)
			}
			if body["message"] != "Password confirmation failed" {
				t.Fatalf("attempt %d: message = %v, want %q", i+1, body["message"], "Password confirmation failed")
			}
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/admin/organizations/"+org.ID,
			map[string]any{"confirm": slug, "password": "not-the-password"}, lockoutHeaders)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("6th attempt: status = %d, want 429; body = %v", resp.StatusCode, body)
		}

		// Side effect never happened, across all six attempts.
		getResp, _ := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+org.ID, nil, headers)
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("org disappeared despite every delete attempt failing reauth: status = %d", getResp.StatusCode)
		}
	})

	t.Run("happy path deletes and audits before the delete", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "admin-delete-happy")
		_, orgDetail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+org.ID, nil, headers)
		slug, _ := orgDetail["slug"].(string)

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/admin/organizations/"+org.ID,
			map[string]any{"confirm": slug, "password": "password123"}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		getResp, _ := doJSON(t, client, ts.URL, http.MethodGet, "/admin/organizations/"+org.ID, nil, headers)
		if getResp.StatusCode != http.StatusNotFound {
			t.Fatalf("org still resolves after delete: status = %d", getResp.StatusCode)
		}

		// The audit trail survives the org it describes (no FK, migration
		// 00004) — admin.org.deleted is queryable by action even though
		// organizationId no longer resolves to anything.
		auditResp, auditBody := doJSON(t, client, ts.URL, http.MethodGet,
			"/admin/audit-logs?organizationId="+org.ID+"&action=admin.org.deleted", nil, headers)
		if auditResp.StatusCode != http.StatusOK {
			t.Fatalf("audit query: status = %d; body = %v", auditResp.StatusCode, auditBody)
		}
		items, _ := auditBody["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("admin.org.deleted entries = %v, want exactly 1", items)
		}
	})
}

// ---- User mutations ----

func TestIntegration_Admin_ChangePlatformRole(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("CANNOT_TARGET_SELF", func(t *testing.T) {
		role := "support"
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/users/"+fx.superUser.UserID+"/platform-role",
			map[string]any{"role": role, "password": "password123"}, headers)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Cannot perform this action on your own account" {
			t.Errorf("message = %v, want the self-target message", body["message"])
		}
	})

	t.Run("SUPERADMIN_LIMIT", func(t *testing.T) {
		const superadminCap = 10
		ctx := context.Background()
		current, err := store.CountSuperadmins(ctx)
		if err != nil {
			t.Fatalf("CountSuperadmins: %v", err)
		}
		for ; current < superadminCap; current++ {
			filler := registerUser(t, client, ts.URL, "superadmin-cap-filler")
			promoteToPlatformRole(t, store, uuid.MustParse(filler.UserID), "superadmin")
		}

		target := registerUser(t, client, ts.URL, "superadmin-cap-target")
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/users/"+target.UserID+"/platform-role",
			map[string]any{"role": "superadmin", "password": "password123"}, headers)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Too many superadmin accounts" {
			t.Errorf("message = %v, want the superadmin-limit message", body["message"])
		}
	})

	t.Run("happy path revokes the target's sessions", func(t *testing.T) {
		target := registerUser(t, client, ts.URL, "platform-role-happy")

		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/users/"+target.UserID+"/platform-role",
			map[string]any{"role": "support", "password": "password123"}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		refreshResp, refreshBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": target.RefreshToken}, nil)
		if refreshResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("post-role-change refresh: status = %d, want 401; body = %v", refreshResp.StatusCode, refreshBody)
		}

		_, userDetail := doJSON(t, client, ts.URL, http.MethodGet, "/admin/users/"+target.UserID, nil, headers)
		if userDetail["platformRole"] != "support" {
			t.Errorf("platformRole = %v, want %q", userDetail["platformRole"], "support")
		}
	})
}

func TestIntegration_Admin_Ban(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("CANNOT_TARGET_SELF", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/users/"+fx.superUser.UserID+"/ban",
			map[string]any{"banned": true, "password": "password123"}, headers)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Cannot perform this action on your own account" {
			t.Errorf("message = %v, want the self-target message", body["message"])
		}
	})

	t.Run("TARGET_IS_PLATFORM_STAFF", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/users/"+fx.supportU.UserID+"/ban",
			map[string]any{"banned": true, "password": "password123"}, headers)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Demote this account before banning or deleting it" {
			t.Errorf("message = %v, want the platform-staff-target message", body["message"])
		}
	})

	t.Run("happy path revokes sessions, blocks login, and leaves mcp_api_keys untouched", func(t *testing.T) {
		targetOrg := createOrgWithOwner(t, client, ts.URL, "ban-target")
		mcpKeyID, _ := mintMCPKeyAs(t, client, ts.URL, targetOrg.ID, targetOrg.Owner.AccessToken, "ban-target-key")

		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/users/"+targetOrg.Owner.UserID+"/ban",
			map[string]any{"banned": true, "reason": "integration test", "password": "password123"}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ban: status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		// Refresh is dead: RevokeAllUserSessions ran as part of the ban.
		refreshResp, refreshBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": targetOrg.Owner.RefreshToken}, nil)
		if refreshResp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("post-ban refresh: status = %d, want 401; body = %v", refreshResp.StatusCode, refreshBody)
		}

		// Login is refused with 403 ACCOUNT_SUSPENDED (not the guard's 401 —
		// this is docs/11-admin-panel.md's documented asymmetry).
		loginResp, loginBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
			map[string]any{"email": targetOrg.Owner.Email, "password": "password123"}, nil)
		if loginResp.StatusCode != http.StatusForbidden {
			t.Fatalf("post-ban login: status = %d, want 403; body = %v", loginResp.StatusCode, loginBody)
		}
		if loginBody["message"] != "Account suspended" {
			t.Errorf("post-ban login message = %v, want %q", loginBody["message"], "Account suspended")
		}

		// mcp_api_keys is untouched: still listed, still not revoked. The
		// gateway itself (not this module) is what refuses a banned
		// owner's key — see internal/middleware/mcpkey.go.
		mcpResp, mcpBody := doJSON(t, client, ts.URL, http.MethodGet,
			"/admin/mcp-keys?userId="+targetOrg.Owner.UserID, nil, headers)
		if mcpResp.StatusCode != http.StatusOK {
			t.Fatalf("mcp-keys: status = %d; body = %v", mcpResp.StatusCode, mcpBody)
		}
		items, _ := mcpBody["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("mcp key items = %v, want exactly 1", items)
		}
		row, _ := items[0].(map[string]any)
		if row["id"] != mcpKeyID {
			t.Errorf("id = %v, want %q", row["id"], mcpKeyID)
		}
		if row["revokedAt"] != nil {
			t.Errorf("revokedAt = %v, want null — a ban must not revoke mcp_api_keys (irreversible; the ban itself is not)", row["revokedAt"])
		}

		// Unban restores login.
		unbanResp, unbanBody := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/users/"+targetOrg.Owner.UserID+"/ban",
			map[string]any{"banned": false, "password": "password123"}, headers)
		if unbanResp.StatusCode != http.StatusOK {
			t.Fatalf("unban: status = %d, want 200; body = %v", unbanResp.StatusCode, unbanBody)
		}
		reloginResp, reloginBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
			map[string]any{"email": targetOrg.Owner.Email, "password": "password123"}, nil)
		if reloginResp.StatusCode != http.StatusOK {
			t.Fatalf("post-unban login: status = %d, want 200; body = %v", reloginResp.StatusCode, reloginBody)
		}
	})
}

// ---- Plan CRUD ----

func TestIntegration_Admin_PlanCRUD(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("create rejects a limits blob missing a required key", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans",
			map[string]any{"name": "invalid-" + uuid.NewString(), "limits": map[string]any{"max_members": 1}}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
	})

	var planID string
	t.Run("create", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans",
			map[string]any{"name": "crud-" + uuid.NewString(), "limits": validPlanLimits(3, 3, 3, 300)}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		planID, _ = body["id"].(string)
		if planID == "" {
			t.Fatalf("missing id: %v", body)
		}
	})

	t.Run("update", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/plans/"+planID,
			map[string]any{"name": "crud-updated-" + uuid.NewString(), "limits": validPlanLimits(4, 4, 4, 400)}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		limits, _ := body["limits"].(map[string]any)
		if limits["max_members"] != 4.0 {
			t.Errorf("max_members = %v, want 4", limits["max_members"])
		}
	})

	t.Run("PLAN_IN_USE blocks delete", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "plan-in-use")
		assignResp, assignBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/organizations/"+org.ID+"/plan",
			map[string]any{"planId": planID}, headers)
		if assignResp.StatusCode != http.StatusOK {
			t.Fatalf("assign: status = %d, want 200; body = %v", assignResp.StatusCode, assignBody)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/admin/plans/"+planID, nil, headers)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Plan has active subscriptions" {
			t.Errorf("message = %v, want the plan-in-use message", body["message"])
		}
	})

	t.Run("delete succeeds once nothing references it", func(t *testing.T) {
		freePlanID := createPlan(t, store, map[string]int{"max_members": 1, "max_roles": 1, "max_connectors": 1}).String()

		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/admin/plans/"+freePlanID, nil, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		listResp, listBody := doJSON(t, client, ts.URL, http.MethodGet, "/admin/plans", nil, headers)
		if listResp.StatusCode != http.StatusOK {
			t.Fatalf("list: status = %d; body = %v", listResp.StatusCode, listBody)
		}
		items, _ := listBody["items"].([]any)
		for _, it := range items {
			row, _ := it.(map[string]any)
			if row["id"] == freePlanID {
				t.Fatalf("deleted plan %s still present: %v", freePlanID, items)
			}
		}
	})
}

// ---- Plan catalogue columns + plan_prices (billing plan step 8) ----

// TestIntegration_Admin_PlanCatalogueColumns covers the three columns
// migration 00013 added to plans, and the coherence problem making
// is_public settable created: POST /billing/checkout already refuses a
// non-public plan, but GET /plans (subscription.Service) predated the
// column and listed every plan regardless — so a plan hidden from the
// catalogue was still advertised to tenants while being unbuyable.
// Step 8 closes that by pointing GET /plans at ListPublicPlans; this is
// the test for it.
func TestIntegration_Admin_PlanCatalogueColumns(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}
	tenantHeaders := map[string]string{"Authorization": "Bearer " + fx.org.Owner.AccessToken}

	name := "hidden-" + uuid.NewString()
	var planID string

	t.Run("create records stripeProductId, isPublic and sortOrder", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans", map[string]any{
			"name":            name,
			"limits":          validPlanLimits(5, 5, 5, 500),
			"stripeProductId": "prod_step8roundtrip",
			"isPublic":        false,
			"sortOrder":       42,
		}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		planID, _ = body["id"].(string)
		if planID == "" {
			t.Fatalf("missing id: %v", body)
		}
		if body["stripeProductId"] != "prod_step8roundtrip" {
			t.Errorf("stripeProductId = %v, want %q", body["stripeProductId"], "prod_step8roundtrip")
		}
		if body["isPublic"] != false {
			t.Errorf("isPublic = %v, want false", body["isPublic"])
		}
		if body["sortOrder"] != 42.0 {
			t.Errorf("sortOrder = %v, want 42", body["sortOrder"])
		}
	})

	t.Run("the console still sees a hidden plan", func(t *testing.T) {
		// If it did not, a plan could be hidden and never un-hidden. This
		// is why admin.Service.ListPlans reads AdminListPlans rather than
		// the tenant catalogue.
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/plans", nil, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if !planListContains(body["items"], planID) {
			t.Fatalf("hidden plan %s missing from /admin/plans: %v", planID, body["items"])
		}
	})

	t.Run("the tenant catalogue does not", func(t *testing.T) {
		resp, rows := doJSONList(t, client, ts.URL, "/plans", tenantHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		for _, row := range rows {
			if row["id"] == planID {
				t.Fatalf("GET /plans lists a plan with is_public = false (%s); "+
					"POST /billing/checkout refuses it, so the catalogue would "+
					"render a button that cannot work: %v", planID, rows)
			}
		}
	})

	t.Run("publishing it through the console puts it in the catalogue", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/plans/"+planID, map[string]any{
			"name":            name,
			"limits":          validPlanLimits(5, 5, 5, 500),
			"stripeProductId": "prod_step8roundtrip",
			"isPublic":        true,
			"sortOrder":       42,
		}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		listResp, rows := doJSONList(t, client, ts.URL, "/plans", tenantHeaders)
		if listResp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", listResp.StatusCode)
		}
		var found bool
		for _, row := range rows {
			if row["id"] == planID {
				found = true
			}
		}
		if !found {
			t.Fatalf("published plan %s missing from GET /plans: %v", planID, rows)
		}
	})
}

// TestIntegration_Admin_PlanRequiresToolCallQuota pins the consequence of
// adding max_tool_calls_per_month to admin.requiredPlanLimitKeys. Before
// step 8 this body was accepted and produced a plan with a silently
// UNLIMITED tool-call quota, because subscription.Service.EnforceLimit
// treats a missing key as unlimited — the one limit that maps to an
// upstream API bill, failing open.
func TestIntegration_Admin_PlanRequiresToolCallQuota(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("create without max_tool_calls_per_month is 422", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans", map[string]any{
			"name":   "uncapped-" + uuid.NewString(),
			"limits": map[string]any{"max_members": 5, "max_roles": 5, "max_connectors": 5},
		}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("update without max_tool_calls_per_month is 422", func(t *testing.T) {
		createResp, createBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans", map[string]any{
			"name":   "capped-" + uuid.NewString(),
			"limits": validPlanLimits(5, 5, 5, 500),
		}, headers)
		if createResp.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d, want 200; body = %v", createResp.StatusCode, createBody)
		}
		planID, _ := createBody["id"].(string)

		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/admin/plans/"+planID, map[string]any{
			"name":   "capped-updated-" + uuid.NewString(),
			"limits": map[string]any{"max_members": 6, "max_roles": 6, "max_connectors": 6},
		}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (a PUT must not be able to drop the cap); body = %v", resp.StatusCode, body)
		}
	})
}

// TestIntegration_Admin_PlanPriceCRUD is the plan_prices round trip
// against real Postgres, plus the two shapes that are deliberately absent
// (no amount edit, no delete) expressed as the behaviour that replaces
// them: supersede-then-deactivate, guarded so it cannot be done in the
// order that strands a public plan.
func TestIntegration_Admin_PlanPriceCRUD(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("unknown plan is 404, not an empty list", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/plans/"+uuid.NewString()+"/prices", nil, headers)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
	})

	// A public plan of its own, so the last-active guard below is about
	// this test's prices and not the shared fixture's.
	createResp, createBody := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans", map[string]any{
		"name":     "priced-" + uuid.NewString(),
		"limits":   validPlanLimits(5, 5, 5, 500),
		"isPublic": true,
	}, headers)
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("create plan: status = %d, want 200; body = %v", createResp.StatusCode, createBody)
	}
	planID, _ := createBody["id"].(string)

	firstStripeID := "price_" + uuid.NewString()
	var firstPriceID string
	t.Run("create", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans/"+planID+"/prices", map[string]any{
			"stripePriceId": firstStripeID,
			"unitAmount":    29900,
			"currency":      "thb",
			"interval":      "month",
		}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		firstPriceID, _ = body["id"].(string)
		if firstPriceID == "" {
			t.Fatalf("missing id: %v", body)
		}
		// The identifier side of the secret/identifier boundary: a
		// price_... is exactly what the console exists to show.
		if body["stripePriceId"] != firstStripeID {
			t.Errorf("stripePriceId = %v, want %q", body["stripePriceId"], firstStripeID)
		}
		if body["unitAmount"] != 29900.0 {
			t.Errorf("unitAmount = %v, want 29900", body["unitAmount"])
		}
		if body["active"] != true {
			t.Errorf("active = %v, want true (the documented default)", body["active"])
		}
	})

	t.Run("a bad interval is 422", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans/"+planID+"/prices", map[string]any{
			"stripePriceId": "price_" + uuid.NewString(),
			"unitAmount":    100,
			"currency":      "thb",
			"interval":      "week",
		}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("an id that is not a Stripe Price id is 422", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans/"+planID+"/prices", map[string]any{
			"stripePriceId": "prod_thisisaproductnotaprice",
			"unitAmount":    100,
			"currency":      "thb",
			"interval":      "month",
		}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("deactivating the only active price of a public plan is 409", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/plans/"+planID+"/prices/"+firstPriceID,
			map[string]any{"active": false}, headers)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		// The error envelope is {message} only (server.newErrorHandler),
		// so the PLAN_PRICE_LAST_ACTIVE code is asserted through its
		// mapped message.
		if body["message"] != "Cannot deactivate a public plan's only active price" {
			t.Errorf("message = %v, want the PLAN_PRICE_LAST_ACTIVE message", body["message"])
		}
	})

	var secondPriceID string
	t.Run("re-pricing is insert the successor, then deactivate", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/admin/plans/"+planID+"/prices", map[string]any{
			"stripePriceId": "price_" + uuid.NewString(),
			"unitAmount":    34900,
			"currency":      "thb",
			"interval":      "month",
		}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create successor: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		secondPriceID, _ = body["id"].(string)

		deactivate, deactivateBody := doJSON(t, client, ts.URL, http.MethodPatch, "/admin/plans/"+planID+"/prices/"+firstPriceID,
			map[string]any{"active": false}, headers)
		if deactivate.StatusCode != http.StatusOK {
			t.Fatalf("deactivate: status = %d, want 200; body = %v", deactivate.StatusCode, deactivateBody)
		}
		if deactivateBody["active"] != false {
			t.Errorf("active = %v, want false", deactivateBody["active"])
		}
	})

	t.Run("list keeps the deactivated row — it is a subscriber's price history", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/admin/plans/"+planID+"/prices", nil, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		items, _ := body["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("got %d prices, want 2 (the superseded one must survive: "+
				"GetPlanByStripePriceID resolves an existing subscriber's plan "+
				"from a price that is no longer sellable): %v", len(items), items)
		}
		seen := map[string]bool{}
		for _, it := range items {
			row, _ := it.(map[string]any)
			id, _ := row["id"].(string)
			seen[id] = true
		}
		if !seen[firstPriceID] || !seen[secondPriceID] {
			t.Errorf("expected both %s and %s in the list: %v", firstPriceID, secondPriceID, items)
		}
	})

	t.Run("a price belonging to another plan is 404", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch,
			"/admin/plans/"+fx.planID+"/prices/"+secondPriceID, map[string]any{"active": false}, headers)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("a missing active flag is 422, but active:false is not", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch,
			"/admin/plans/"+planID+"/prices/"+secondPriceID, map[string]any{}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}

		// And now the other half: {"active": false} must NOT be rejected
		// as missing. That is the trap PlanPriceActiveRequest.Active is a
		// nil-checked pointer for — `validate:"required"` dereferences
		// before checking and would reject false as absent.
		//
		// This price is the plan's last active one, so deactivating it
		// while the plan is public would (correctly) be a 409. Hiding the
		// tier first is the documented retirement path, and it is what
		// makes the last-active guard an ordering constraint rather than
		// a dead end.
		hide, hideBody := doJSON(t, client, ts.URL, http.MethodPut, "/admin/plans/"+planID, map[string]any{
			"name":     "priced-hidden-" + uuid.NewString(),
			"limits":   validPlanLimits(5, 5, 5, 500),
			"isPublic": false,
		}, headers)
		if hide.StatusCode != http.StatusOK {
			t.Fatalf("hide plan: status = %d, want 200; body = %v", hide.StatusCode, hideBody)
		}

		off, offBody := doJSON(t, client, ts.URL, http.MethodPatch,
			"/admin/plans/"+planID+"/prices/"+secondPriceID, map[string]any{"active": false}, headers)
		if off.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (hide the tier, then retire its prices); body = %v", off.StatusCode, offBody)
		}
		if offBody["active"] != false {
			t.Errorf("active = %v, want false", offBody["active"])
		}
	})
}

// planListContains reports whether a decoded /admin/plans items array
// holds a plan with the given id.
func planListContains(items any, planID string) bool {
	rows, _ := items.([]any)
	for _, it := range rows {
		row, _ := it.(map[string]any)
		if row["id"] == planID {
			return true
		}
	}
	return false
}
