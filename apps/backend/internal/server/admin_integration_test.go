package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
)

// promoteToPlatformRole grants userID the given platform_role directly
// through the store, mirroring what cmd/grantadmin does in production —
// there is no HTTP route to do this in Phase 2 (the mutation routes are a
// later phase), so tests reach past the API the same way an operator
// running `make admin-grant` would.
func promoteToPlatformRole(t *testing.T, store *database.Store, userID uuid.UUID, role string) {
	t.Helper()

	r := role
	if err := store.SetUserPlatformRole(context.Background(), db.SetUserPlatformRoleParams{ID: userID, PlatformRole: &r}); err != nil {
		t.Fatalf("SetUserPlatformRole(%s, %s): %v", userID, role, err)
	}
}

// doAdminGet issues a GET with no body (mirrors doJSONList's request shape,
// which existing admin GETs need too — a JSON body on a GET can trip
// Echo's binder in ways a POST/PUT body never does) and returns both the
// raw response bytes (for the substring check) and, when the body is valid
// JSON, whatever it decodes to.
func doAdminGet(t *testing.T, client *http.Client, baseURL, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

// decodeJSONObject is a small helper for tests that need the parsed body
// after already reading it raw via doAdminGet.
func decodeJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response as object: %v; body = %s", err, raw)
	}
	return decoded
}

// fixtureConnSecret is the password inside the fixture connector's config.
// It is sealed at rest by envelope encryption and must never surface in an
// admin response in any form.
const fixtureConnSecret = "hunter2-fixture-connector-secret"

// adminTestFixture is the shared tenant-side state every admin test in this
// file reads: one organization with an owner, a member, a connector, an
// MCP key, and an assigned plan, so every admin list/detail view below has
// at least one real row to render (and, for the connectors/mcp-keys
// substring check, an actual encrypted_config/key_hash sitting in the
// database for the response to leak if the mapping is ever careless).
type adminTestFixture struct {
	org       createdOrg
	memberID  string
	connID    string
	mcpKeyID  string
	planID    string
	superUser registeredUser
	supportU  registeredUser

	// connSecret and rawMCPKey are the two live secrets the fixture puts
	// in the database: the connector config's password (sealed by envelope
	// encryption) and the raw PAT (stored only as a SHA-256 hash). The
	// substring table test asserts neither value appears in any admin
	// response — a value check, unlike the field-name check beside it,
	// still catches a leak served under a renamed or restructured key.
	connSecret string
	rawMCPKey  string
}

func setupAdminFixture(t *testing.T, client *http.Client, baseURL string, store *database.Store) adminTestFixture {
	t.Helper()

	org := createOrgWithOwner(t, client, baseURL, "admin-fixture")
	ownerHeaders := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	member := registerUser(t, client, baseURL, "admin-fixture-member")
	inviteMember(t, client, baseURL, org, member.Email, "member")

	connResp, connBody := doJSON(t, client, baseURL, http.MethodPost, "/connectors",
		map[string]any{"name": "warehouse-db", "type": "generic", "config": map[string]any{"host": "db.example.com", "password": fixtureConnSecret}},
		ownerHeaders)
	if connResp.StatusCode != http.StatusOK {
		t.Fatalf("create connector: status = %d, want 200; body = %v", connResp.StatusCode, connBody)
	}
	connID, _ := connBody["id"].(string)
	if connID == "" {
		t.Fatalf("create connector: missing id: %v", connBody)
	}

	keyResp, keyBody := doJSON(t, client, baseURL, http.MethodPost, "/mcp-keys",
		map[string]any{"name": "admin-fixture-key"}, ownerHeaders)
	if keyResp.StatusCode != http.StatusOK {
		t.Fatalf("create mcp key: status = %d, want 200; body = %v", keyResp.StatusCode, keyBody)
	}
	mcpKeyID, _ := keyBody["id"].(string)
	if mcpKeyID == "" {
		t.Fatalf("create mcp key: missing id: %v", keyBody)
	}
	rawMCPKey, _ := keyBody["apiKey"].(string)
	if rawMCPKey == "" {
		t.Fatalf("create mcp key: missing apiKey (the one and only time it is returned): %v", keyBody)
	}

	planID := createPlan(t, store, map[string]int{"max_members": 10})
	planResp, planRespBody := doJSON(t, client, baseURL, http.MethodPost, "/subscription/assign",
		map[string]any{"planId": planID.String()}, ownerHeaders)
	if planResp.StatusCode != http.StatusOK {
		t.Fatalf("assign plan: status = %d, want 200; body = %v", planResp.StatusCode, planRespBody)
	}

	superUser := registerUser(t, client, baseURL, "admin-fixture-superadmin")
	promoteToPlatformRole(t, store, uuid.MustParse(superUser.UserID), "superadmin")

	supportUser := registerUser(t, client, baseURL, "admin-fixture-support")
	promoteToPlatformRole(t, store, uuid.MustParse(supportUser.UserID), "support")

	return adminTestFixture{
		org: org, memberID: member.UserID, connID: connID, mcpKeyID: mcpKeyID,
		planID: planID.String(), superUser: superUser, supportU: supportUser,
		connSecret: fixtureConnSecret, rawMCPKey: rawMCPKey,
	}
}

// adminGetRoutes returns every Phase 2 GET route, using fixture's ids for
// the two path-parameterized ones. Shared by the per-role 200 check and the
// no-forbidden-substring table test so both walk exactly the same list.
func adminGetRoutes(fx adminTestFixture) []string {
	return []string{
		"/admin/me",
		"/admin/organizations",
		"/admin/organizations/" + fx.org.ID,
		"/admin/users",
		"/admin/users/" + fx.memberID,
		"/admin/connectors",
		"/admin/mcp-keys",
		"/admin/audit-logs",
		"/admin/system/stats",
		"/admin/plans",
	}
}

func TestIntegration_Admin_ReadRoutes(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)

	staffHeaders := func(u registeredUser) map[string]string {
		return map[string]string{"Authorization": "Bearer " + u.AccessToken}
	}

	t.Run("200 for both platform roles", func(t *testing.T) {
		for _, staff := range []struct {
			role string
			user registeredUser
		}{
			{"superadmin", fx.superUser},
			{"support", fx.supportU},
		} {
			for _, path := range adminGetRoutes(fx) {
				t.Run(staff.role+" "+path, func(t *testing.T) {
					resp, raw := doAdminGet(t, client, ts.URL, path, staffHeaders(staff.user))
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
					}
				})
			}
		}
	})

	t.Run("401 unauthenticated", func(t *testing.T) {
		for _, path := range adminGetRoutes(fx) {
			t.Run(path, func(t *testing.T) {
				resp, raw := doAdminGet(t, client, ts.URL, path, nil)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401; body = %s", resp.StatusCode, raw)
				}
			})
		}
	})

	t.Run("403 for a tenant user with no platform_role", func(t *testing.T) {
		headers := map[string]string{"Authorization": "Bearer " + fx.org.Owner.AccessToken}
		for _, path := range adminGetRoutes(fx) {
			t.Run(path, func(t *testing.T) {
				resp, raw := doAdminGet(t, client, ts.URL, path, headers)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403; body = %s", resp.StatusCode, raw)
				}
				body := decodeJSONObject(t, raw)
				if body["message"] != "Insufficient permissions" {
					t.Errorf("message = %v, want %q (must not reveal which platform roles exist)", body["message"], "Insufficient permissions")
				}
			})
		}
	})

	// The one that matters: no admin response, anywhere, leaks
	// encrypted_config, password_hash, or key_hash. This walks the exact
	// same route list as the 200 check above, using the superadmin
	// caller, and inspects the RAW response bytes rather than a decoded
	// map — a decoded-and-reserialized body could hide a field under a
	// renamed or restructured key that the raw wire response would not.
	t.Run("no admin response leaks a forbidden field", func(t *testing.T) {
		// Field names first, then the live secret VALUES: a leak served
		// under a renamed key ("config", "credentials", a nested blob)
		// passes the name check and fails the value check.
		forbidden := []string{
			"encrypted_config", "password_hash", "key_hash",
			fx.connSecret, fx.rawMCPKey,
		}

		for _, path := range adminGetRoutes(fx) {
			t.Run(path, func(t *testing.T) {
				resp, raw := doAdminGet(t, client, ts.URL, path, staffHeaders(fx.superUser))
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
				}
				for _, substr := range forbidden {
					if strings.Contains(string(raw), substr) {
						t.Errorf("response body for %s contains forbidden substring %q: %s", path, substr, raw)
					}
				}
			})
		}
	})
}

func TestIntegration_Admin_Me(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	staff := registerUser(t, client, ts.URL, "admin-me")
	promoteToPlatformRole(t, store, uuid.MustParse(staff.UserID), "superadmin")

	resp, raw := doAdminGet(t, client, ts.URL, "/admin/me", map[string]string{"Authorization": "Bearer " + staff.AccessToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
	}
	body := decodeJSONObject(t, raw)
	if body["email"] != staff.Email {
		t.Errorf("email = %v, want %q", body["email"], staff.Email)
	}
	if body["platformRole"] != "superadmin" {
		t.Errorf("platformRole = %v, want %q", body["platformRole"], "superadmin")
	}
	if body["id"] != staff.UserID {
		t.Errorf("id = %v, want %q", body["id"], staff.UserID)
	}
}

func TestIntegration_Admin_OrganizationsListAndDetail(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("list has items/total shape and finds the fixture org", func(t *testing.T) {
		// The shared test database accumulates organizations across the
		// whole integration suite's history, so search by the fixture's
		// own slug prefix rather than relying on it falling within an
		// unfiltered page.
		// Search on the fixture org's OWN slug, not the shared
		// "admin-fixture" prefix: uniqueSlug appends a UUID, so this matches
		// exactly one row. The prefix alone matches every past run's fixture
		// org too, and since AdminListOrganizations orders created_at ASC the
		// just-created one drops off the first page once a long-lived dev
		// database accumulates more than ?limit= of them — the same
		// history-dependent trap the users-list test above documents.
		resp, raw := doAdminGet(t, client, ts.URL,
			"/admin/organizations?search="+url.QueryEscape(fx.org.Slug), headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
		}
		body := decodeJSONObject(t, raw)
		items, ok := body["items"].([]any)
		if !ok {
			t.Fatalf("items is not an array: %v", body)
		}
		if _, ok := body["total"]; !ok {
			t.Fatalf("missing total: %v", body)
		}
		var found bool
		for _, it := range items {
			row, _ := it.(map[string]any)
			if row["id"] == fx.org.ID {
				found = true
				if row["memberCount"].(float64) < 2 {
					t.Errorf("memberCount = %v, want >= 2 (owner + invited member)", row["memberCount"])
				}
				if row["connectorCount"].(float64) < 1 {
					t.Errorf("connectorCount = %v, want >= 1", row["connectorCount"])
				}
				if row["mcpKeyCount"].(float64) < 1 {
					t.Errorf("mcpKeyCount = %v, want >= 1", row["mcpKeyCount"])
				}
			}
		}
		if !found {
			t.Fatalf("fixture org %s not found in list: %v", fx.org.ID, items)
		}
	})

	t.Run("detail embeds plan, limits, members, connectors, mcp keys, audit logs", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/organizations/"+fx.org.ID, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
		}
		body := decodeJSONObject(t, raw)

		if body["id"] != fx.org.ID {
			t.Errorf("id = %v, want %q", body["id"], fx.org.ID)
		}
		if body["planName"] == nil {
			t.Errorf("planName = nil, want the assigned plan's name")
		}
		if _, ok := body["effectiveLimits"].(map[string]any); !ok {
			t.Errorf("effectiveLimits missing or not an object: %v", body["effectiveLimits"])
		}
		members, _ := body["members"].([]any)
		if len(members) < 2 {
			t.Errorf("members = %v, want >= 2", members)
		}
		connectors, _ := body["connectors"].([]any)
		if len(connectors) < 1 {
			t.Fatalf("connectors = %v, want >= 1", connectors)
		}
		firstConn, _ := connectors[0].(map[string]any)
		if firstConn["id"] != fx.connID {
			t.Errorf("connector id = %v, want %q", firstConn["id"], fx.connID)
		}
		if _, ok := firstConn["config"]; ok {
			t.Fatalf("nested connector leaks a config key: %v", firstConn)
		}
		mcpKeys, _ := body["mcpKeys"].([]any)
		if len(mcpKeys) < 1 {
			t.Fatalf("mcpKeys = %v, want >= 1", mcpKeys)
		}
		firstKey, _ := mcpKeys[0].(map[string]any)
		if _, ok := firstKey["keyHash"]; ok {
			t.Fatalf("nested mcp key leaks keyHash: %v", firstKey)
		}
		if _, ok := body["recentAuditLogs"].([]any); !ok {
			t.Errorf("recentAuditLogs missing or not an array: %v", body["recentAuditLogs"])
		}
	})

	t.Run("unknown org 404s", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/organizations/"+uuid.NewString(), headers)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", resp.StatusCode, raw)
		}
	})
}

func TestIntegration_Admin_UsersListAndDetail(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	// Every case below pins ?search= to one fixture user's own email
	// (uniqueEmail appends a UUID, so it matches exactly one row) rather
	// than paging the unfiltered list and hunting for the fixture in it.
	// AdminListUsers orders by created_at ASC, so a bare
	// ?role=superadmin&limit=100 only finds a just-created fixture while
	// the database holds fewer than 100 superadmins total — true on a
	// fresh CI database, and steadily less true on a long-lived dev one
	// that every run of this suite adds more staff accounts to. Scoping by
	// email removes the dependency on how much history the database has.
	//
	// The pair of role filters against a KNOWN-matching search term is
	// also what gives the assertion teeth: an empty result only proves the
	// role filter excluded the row if the same search with the right role
	// returns it.
	t.Run("role filter selects staff by platform role", func(t *testing.T) {
		cases := []struct {
			name     string
			role     string
			email    string
			wantID   string // "" means the filter must exclude the row entirely
			wantRole any
		}{
			{name: "superadmin matches the fixture superadmin", role: "superadmin", email: fx.superUser.Email, wantID: fx.superUser.UserID, wantRole: "superadmin"},
			{name: "support matches the fixture support user", role: "support", email: fx.supportU.Email, wantID: fx.supportU.UserID, wantRole: "support"},
			{name: "superadmin excludes the support user", role: "superadmin", email: fx.supportU.Email, wantID: ""},
			{name: "none excludes the superadmin", role: "none", email: fx.superUser.Email, wantID: ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resp, raw := doAdminGet(t, client, ts.URL,
					"/admin/users?role="+tc.role+"&search="+url.QueryEscape(tc.email), headers)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
				}
				body := decodeJSONObject(t, raw)
				items, _ := body["items"].([]any)

				if tc.wantID == "" {
					if len(items) != 0 {
						t.Fatalf("items = %v, want none: role=%q must exclude %s", items, tc.role, tc.email)
					}
					return
				}

				if len(items) != 1 {
					t.Fatalf("items = %v, want exactly 1 (search pins one unique email)", items)
				}
				row, _ := items[0].(map[string]any)
				if row["id"] != tc.wantID {
					t.Errorf("id = %v, want %q", row["id"], tc.wantID)
				}
				if row["platformRole"] != tc.wantRole {
					t.Errorf("platformRole = %v, want %v", row["platformRole"], tc.wantRole)
				}
			})
		}
	})

	t.Run("detail embeds memberships and active session count, never password_hash", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/users/"+fx.memberID, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
		}
		body := decodeJSONObject(t, raw)
		if _, ok := body["passwordHash"]; ok {
			t.Fatalf("user detail leaks passwordHash: %v", body)
		}
		memberships, _ := body["memberships"].([]any)
		if len(memberships) < 1 {
			t.Fatalf("memberships = %v, want >= 1", memberships)
		}
		row, _ := memberships[0].(map[string]any)
		if row["organizationId"] != fx.org.ID {
			t.Errorf("membership organizationId = %v, want %q", row["organizationId"], fx.org.ID)
		}
		if _, ok := body["activeSessions"].(float64); !ok {
			t.Errorf("activeSessions missing or not a number: %v", body["activeSessions"])
		}
	})

	t.Run("unknown user 404s", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/users/"+uuid.NewString(), headers)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", resp.StatusCode, raw)
		}
	})
}

func TestIntegration_Admin_ConnectorsAndMCPKeys(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("connectors filtered by organizationId", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/connectors?organizationId="+fx.org.ID, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
		}
		body := decodeJSONObject(t, raw)
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items = %v, want exactly 1 for this org", items)
		}
		row, _ := items[0].(map[string]any)
		if row["id"] != fx.connID {
			t.Errorf("id = %v, want %q", row["id"], fx.connID)
		}
		if row["organizationName"] != "Test Org" {
			t.Errorf("organizationName = %v, want %q", row["organizationName"], "Test Org")
		}
		for _, forbiddenKey := range []string{"config", "encryptedConfig", "encrypted_config"} {
			if _, ok := row[forbiddenKey]; ok {
				t.Fatalf("connector item leaks %q: %v", forbiddenKey, row)
			}
		}
	})

	t.Run("mcp keys filtered by userId", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/mcp-keys?userId="+fx.org.Owner.UserID, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
		}
		body := decodeJSONObject(t, raw)
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items = %v, want exactly 1 for this owner", items)
		}
		row, _ := items[0].(map[string]any)
		if row["id"] != fx.mcpKeyID {
			t.Errorf("id = %v, want %q", row["id"], fx.mcpKeyID)
		}
		if row["userEmail"] != fx.org.Owner.Email {
			t.Errorf("userEmail = %v, want %q", row["userEmail"], fx.org.Owner.Email)
		}
		for _, forbiddenKey := range []string{"keyHash", "key_hash", "apiKey"} {
			if _, ok := row[forbiddenKey]; ok {
				t.Fatalf("mcp key item leaks %q: %v", forbiddenKey, row)
			}
		}
	})
}

func TestIntegration_Admin_AuditLogsPrefixFilter(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	// org.created is written when createOrgWithOwner ran; "org.*" should
	// match it by prefix.
	resp, raw := doAdminGet(t, client, ts.URL, "/admin/audit-logs?organizationId="+fx.org.ID+"&action=org.*&limit=200", headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
	}
	body := decodeJSONObject(t, raw)
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("expected at least one org.* audit entry, got none: %v", body)
	}
	for _, it := range items {
		row, _ := it.(map[string]any)
		action, _ := row["action"].(string)
		if !strings.HasPrefix(action, "org.") {
			t.Errorf("action = %q, want an org.* prefix match", action)
		}
		if row["organizationName"] != "Test Org" {
			t.Errorf("organizationName = %v, want %q", row["organizationName"], "Test Org")
		}
	}
}

func TestIntegration_Admin_SystemStatsAndPlans(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	fx := setupAdminFixture(t, client, ts.URL, store)
	headers := map[string]string{"Authorization": "Bearer " + fx.superUser.AccessToken}

	t.Run("system stats", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/system/stats", headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
		}
		body := decodeJSONObject(t, raw)
		if v, ok := body["organizations"].(float64); !ok || v < 1 {
			t.Errorf("organizations = %v, want >= 1", body["organizations"])
		}
		if v, ok := body["users"].(float64); !ok || v < 1 {
			t.Errorf("users = %v, want >= 1", body["users"])
		}
		emailOutbox, ok := body["emailOutbox"].(map[string]any)
		if !ok {
			t.Fatalf("emailOutbox missing or not an object: %v", body)
		}
		for _, key := range []string{"pending", "sent", "failed"} {
			if _, ok := emailOutbox[key]; !ok {
				t.Errorf("emailOutbox missing %q: %v", key, emailOutbox)
			}
		}
		if _, ok := body["planBreakdown"].([]any); !ok {
			t.Errorf("planBreakdown missing or not an array: %v", body)
		}
	})

	t.Run("plans", func(t *testing.T) {
		resp, raw := doAdminGet(t, client, ts.URL, "/admin/plans", headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, raw)
		}
		body := decodeJSONObject(t, raw)
		items, _ := body["items"].([]any)
		var found bool
		for _, it := range items {
			row, _ := it.(map[string]any)
			if row["id"] == fx.planID {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the fixture plan %s in /admin/plans: %v", fx.planID, items)
		}
	})
}
