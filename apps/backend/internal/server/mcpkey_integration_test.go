package server_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestIntegration_MCPKeysCRUD_OwnerHappyPath(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcpkeys-crud")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
		map[string]any{"name": "laptop"}, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	apiKey, _ := body["apiKey"].(string)
	if apiKey == "" {
		t.Fatalf("create: missing apiKey: %v", body)
	}
	if len(apiKey) < len("sk_live_")+10 {
		t.Fatalf("apiKey looks too short: %q", apiKey)
	}
	if body["name"] != "laptop" {
		t.Errorf("name = %v, want %q", body["name"], "laptop")
	}
	if body["expiresAt"] != nil {
		t.Errorf("expiresAt = %v, want nil (no expiresInDays supplied)", body["expiresAt"])
	}
	keyID, _ := body["id"].(string)
	if keyID == "" {
		t.Fatalf("create: missing id: %v", body)
	}

	t.Run("list contains it, no hash or token", func(t *testing.T) {
		_, rows := doJSONList(t, client, ts.URL, "/mcp-keys", headers)
		found := findByID(rows, keyID)
		if found == nil {
			t.Fatalf("expected key %s in list, got %v", keyID, rows)
		}
		if found["name"] != "laptop" {
			t.Errorf("name = %v, want %q", found["name"], "laptop")
		}
		if _, ok := found["apiKey"]; ok {
			t.Fatalf("list response leaks apiKey: %v", found)
		}
		if _, ok := found["keyHash"]; ok {
			t.Fatalf("list response leaks keyHash: %v", found)
		}
		if found["revokedAt"] != nil {
			t.Errorf("revokedAt = %v, want nil before revoke", found["revokedAt"])
		}
	})

	t.Run("expiresInDays sets an absolute expiry", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
			map[string]any{"name": "expiring-key", "expiresInDays": 30}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["expiresAt"] == nil {
			t.Fatalf("expiresAt = nil, want a timestamp when expiresInDays is supplied")
		}
	})

	t.Run("revoke then list shows revokedAt set", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+keyID, nil, headers)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("revoke: status = %d, body = %v", resp.StatusCode, body)
		}

		_, rows := doJSONList(t, client, ts.URL, "/mcp-keys", headers)
		found := findByID(rows, keyID)
		if found == nil {
			t.Fatalf("expected key %s still in list after revoke, got %v", keyID, rows)
		}
		if found["revokedAt"] == nil {
			t.Errorf("revokedAt = nil, want a timestamp after revoke")
		}
	})

	t.Run("revoking again is idempotent", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+keyID, nil, headers)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("revoke again: status = %d, body = %v", resp.StatusCode, body)
		}
	})

	t.Run("revoking a nonexistent id 404s", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+uuid.NewString(), nil, headers)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "MCP key not found" {
			t.Fatalf("message = %v, want %q", body["message"], "MCP key not found")
		}
	})
}

// TestIntegration_MCPKeysScopes exercises the three-state `scopes` contract
// through the live API — mint with scopes, mint without, and the `[]`
// rejection — end to end rather than around it with raw SQL.
func TestIntegration_MCPKeysScopes(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcpkeys-scopes")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	t.Run("minting with scopes persists them and GET echoes them back", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
			map[string]any{"name": "scoped-via-api", "scopes": []string{"connector:read", "sheets:*"}}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		keyID, _ := body["id"].(string)
		if keyID == "" {
			t.Fatalf("create: missing id: %v", body)
		}

		_, rows := doJSONList(t, client, ts.URL, "/mcp-keys", headers)
		found := findByID(rows, keyID)
		if found == nil {
			t.Fatalf("expected key %s in list, got %v", keyID, rows)
		}
		scopesAny, _ := found["scopes"].([]any)
		got := make([]string, len(scopesAny))
		for i, s := range scopesAny {
			got[i], _ = s.(string)
		}
		want := []string{"connector:read", "sheets:*"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("scopes = %v, want %v", got, want)
		}
	})

	t.Run("minting without scopes yields null", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
			map[string]any{"name": "unrestricted-via-api"}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		keyID, _ := body["id"].(string)

		_, rows := doJSONList(t, client, ts.URL, "/mcp-keys", headers)
		found := findByID(rows, keyID)
		if found == nil {
			t.Fatalf("expected key %s in list, got %v", keyID, rows)
		}
		if found["scopes"] != nil {
			t.Fatalf("scopes = %v, want nil (null) when scopes is omitted", found["scopes"])
		}
	})

	t.Run("scopes: [] is rejected 422", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
			map[string]any{"name": "empty-scopes", "scopes": []string{}}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Validation failed" {
			t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
		}
	})

	t.Run("a malformed scope string is rejected 422", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
			map[string]any{"name": "bad-scope", "scopes": []string{"Connector:Read"}}, headers)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Validation failed" {
			t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
		}
	})
}

func TestIntegration_MCPKeysDuplicateName(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("same org rejects duplicate name", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "mcpkeys-dup-same")
		headers := map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		}

		doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys", map[string]any{"name": "dup-name"}, headers)

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys", map[string]any{"name": "dup-name"}, headers)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "MCP key name already taken" {
			t.Fatalf("message = %v, want %q", body["message"], "MCP key name already taken")
		}
	})

	t.Run("different org allows the same name", func(t *testing.T) {
		orgA := createOrgWithOwner(t, client, ts.URL, "mcpkeys-dup-a")
		orgB := createOrgWithOwner(t, client, ts.URL, "mcpkeys-dup-b")

		doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys", map[string]any{"name": "shared-name"},
			map[string]string{"Authorization": "Bearer " + orgA.Owner.AccessToken, "x-organization-id": orgA.ID})

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys", map[string]any{"name": "shared-name"},
			map[string]string{"Authorization": "Bearer " + orgB.Owner.AccessToken, "x-organization-id": orgB.ID})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
	})
}

func TestIntegration_MCPKeysValidation(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcpkeys-validation")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	cases := []struct {
		name string
		body map[string]any
	}{
		{"empty name", map[string]any{"name": ""}},
		{"name too long", map[string]any{"name": string(make([]byte, 101))}},
		{"expiresInDays zero", map[string]any{"name": "x", "expiresInDays": 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys", tc.body, headers)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
			}
			if body["message"] != "Validation failed" {
				t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
			}
		})
	}
}

func TestIntegration_MCPKeysPermissionEnforcement(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcpkeys-perm")
	ownerHeaders := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	t.Run("member with no role is denied read", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "mcpkeys-perm-norole")
		inviteMember(t, client, ts.URL, org, member.Email, "member")

		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/mcp-keys", nil,
			map[string]string{"Authorization": "Bearer " + member.AccessToken, "x-organization-id": org.ID})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Missing permission: mcpkey:read" {
			t.Fatalf("message = %v, want %q", body["message"], "Missing permission: mcpkey:read")
		}
	})

	t.Run("member with mcpkey:read role can read but not write or delete", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "mcpkeys-perm-read")
		inviteMember(t, client, ts.URL, org, member.Email, "member")
		roleID := createConnectorRole(t, client, ts.URL, org, []string{"mcpkey:read"})
		assignConnectorRole(t, client, ts.URL, org, member.UserID, roleID)

		memberHeaders := map[string]string{"Authorization": "Bearer " + member.AccessToken, "x-organization-id": org.ID}

		resp, rows := doJSONList(t, client, ts.URL, "/mcp-keys", memberHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("read: status = %d, want 200; body = %v", resp.StatusCode, rows)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
			map[string]any{"name": "read-only-should-fail"}, memberHeaders)
		if resp2.StatusCode != http.StatusForbidden {
			t.Fatalf("write: status = %d, want 403; body = %v", resp2.StatusCode, body2)
		}
		if body2["message"] != "Missing permission: mcpkey:write" {
			t.Fatalf("write message = %v, want %q", body2["message"], "Missing permission: mcpkey:write")
		}

		resp3, body3 := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+uuid.NewString(), nil, memberHeaders)
		if resp3.StatusCode != http.StatusForbidden {
			t.Fatalf("delete: status = %d, want 403; body = %v", resp3.StatusCode, body3)
		}
		if body3["message"] != "Missing permission: mcpkey:delete" {
			t.Fatalf("delete message = %v, want %q", body3["message"], "Missing permission: mcpkey:delete")
		}
	})

	t.Run("member with mcpkey:* role can read, write, and delete", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "mcpkeys-perm-wildcard")
		inviteMember(t, client, ts.URL, org, member.Email, "member")
		roleID := createConnectorRole(t, client, ts.URL, org, []string{"mcpkey:*"})
		assignConnectorRole(t, client, ts.URL, org, member.UserID, roleID)

		memberHeaders := map[string]string{"Authorization": "Bearer " + member.AccessToken, "x-organization-id": org.ID}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
			map[string]any{"name": uniqueSlug("wildcard-key")}, memberHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		keyID, _ := body["id"].(string)

		resp2, rows := doJSONList(t, client, ts.URL, "/mcp-keys", memberHeaders)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("read: status = %d, want 200; body = %v", resp2.StatusCode, rows)
		}

		resp3, body3 := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+keyID, nil, memberHeaders)
		if resp3.StatusCode != http.StatusOK || body3["success"] != true {
			t.Fatalf("delete: status = %d, body = %v", resp3.StatusCode, body3)
		}
	})

	// Sanity: the owner used to set up the fixtures above must have full
	// access throughout, via RBAC's owner bypass.
	t.Run("owner bypass still works", func(t *testing.T) {
		resp, body := doJSONList(t, client, ts.URL, "/mcp-keys", ownerHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
	})
}

func TestIntegration_MCPKeysCrossOrgIsolation(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	orgA := createOrgWithOwner(t, client, ts.URL, "mcpkeys-isolation-a")
	orgB := createOrgWithOwner(t, client, ts.URL, "mcpkeys-isolation-b")

	_, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp-keys",
		map[string]any{"name": "org-a-key"},
		map[string]string{"Authorization": "Bearer " + orgA.Owner.AccessToken, "x-organization-id": orgA.ID})
	keyID, _ := body["id"].(string)
	if keyID == "" {
		t.Fatalf("create in org A: missing id: %v", body)
	}

	orgBHeaders := map[string]string{"Authorization": "Bearer " + orgB.Owner.AccessToken, "x-organization-id": orgB.ID}

	t.Run("org B's list never sees org A's key", func(t *testing.T) {
		_, rows := doJSONList(t, client, ts.URL, "/mcp-keys", orgBHeaders)
		if found := findByID(rows, keyID); found != nil {
			t.Fatalf("org B's list leaked org A's key: %v", found)
		}
	})

	t.Run("org B revoking org A's key 404s, indistinguishable from nonexistent", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+keyID, nil, orgBHeaders)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "MCP key not found" {
			t.Fatalf("message = %v, want %q", body["message"], "MCP key not found")
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+uuid.NewString(), nil, orgBHeaders)
		if resp2.StatusCode != http.StatusNotFound {
			t.Fatalf("nonexistent id: status = %d, want 404; body = %v", resp2.StatusCode, body2)
		}
		if body2["message"] != body["message"] {
			t.Fatalf("cross-org message %q differs from nonexistent-id message %q — id is probeable", body["message"], body2["message"])
		}
	})
}

func TestIntegration_MCPKeysGuardBasics(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("no authorization", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/mcp-keys", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Unauthorized" {
			t.Fatalf("message = %v, want %q", body["message"], "Unauthorized")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/mcp-keys", nil,
			map[string]string{"Authorization": "Bearer not-a-real-token"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Unauthorized" {
			t.Fatalf("message = %v, want %q", body["message"], "Unauthorized")
		}
	})

	t.Run("missing x-organization-id header", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "mcpkeys-guard-noheader")

		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/mcp-keys", nil,
			map[string]string{"Authorization": "Bearer " + org.Owner.AccessToken})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Missing x-organization-id header" {
			t.Fatalf("message = %v, want %q", body["message"], "Missing x-organization-id header")
		}
	})
}
