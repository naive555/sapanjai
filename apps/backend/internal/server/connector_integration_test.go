package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// createConnectorRole creates a role in org with the given permissions and
// returns its id, using org's owner as the caller.
func createConnectorRole(t *testing.T, client *http.Client, baseURL string, org createdOrg, permissions []string) string {
	t.Helper()

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/rbac/roles",
		map[string]any{"name": uniqueSlug("role"), "permissions": permissions},
		map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create role: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create role: missing id: %v", body)
	}
	return id
}

// assignConnectorRole assigns roleID to userID's membership in org, using
// org's owner as the caller.
func assignConnectorRole(t *testing.T, client *http.Client, baseURL string, org createdOrg, userID, roleID string) {
	t.Helper()

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/rbac/assign",
		map[string]any{"userId": userID, "roleId": roleID},
		map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
	if resp.StatusCode != http.StatusOK || body["success"] != true {
		t.Fatalf("assign role: status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestIntegration_ConnectorsCRUD_OwnerHappyPath(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "connectors-crud")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
		map[string]any{"name": "warehouse-db", "type": "generic", "config": map[string]any{"host": "db.example.com", "password": "hunter2"}},
		headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	if body["status"] != "inactive" {
		t.Errorf("status = %v, want %q", body["status"], "inactive")
	}
	if body["lastHealthCheckAt"] != nil {
		t.Errorf("lastHealthCheckAt = %v, want nil", body["lastHealthCheckAt"])
	}
	if _, ok := body["config"]; ok {
		t.Fatalf("create response leaks a config key: %v", body)
	}
	connID, _ := body["id"].(string)
	if connID == "" {
		t.Fatalf("create: missing id: %v", body)
	}

	t.Run("list contains it", func(t *testing.T) {
		_, rows := doJSONList(t, client, ts.URL, "/connectors", headers)
		found := findByID(rows, connID)
		if found == nil {
			t.Fatalf("expected connector %s in list, got %v", connID, rows)
		}
	})

	t.Run("get by id", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/connectors/"+connID, nil, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["name"] != "warehouse-db" {
			t.Errorf("name = %v, want %q", body["name"], "warehouse-db")
		}
	})

	t.Run("patch name", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/connectors/"+connID,
			map[string]any{"name": "warehouse-db-renamed"}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["name"] != "warehouse-db-renamed" {
			t.Errorf("name = %v, want %q", body["name"], "warehouse-db-renamed")
		}
	})

	t.Run("patch config", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPatch, "/connectors/"+connID,
			map[string]any{"config": map[string]any{"host": "rotated.example.com", "password": "new-secret"}}, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if _, ok := body["config"]; ok {
			t.Fatalf("patch response leaks a config key: %v", body)
		}
	})

	t.Run("health check is unsupported for generic connectors", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors/"+connID+"/health-check", nil, headers)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Health check not supported for this connector type" {
			t.Fatalf("message = %v, want %q", body["message"], "Health check not supported for this connector type")
		}
	})

	t.Run("delete then get 404s", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/connectors/"+connID, nil, headers)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("delete: status = %d, body = %v", resp.StatusCode, body)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodGet, "/connectors/"+connID, nil, headers)
		if resp2.StatusCode != http.StatusNotFound {
			t.Fatalf("get after delete: status = %d, want 404; body = %v", resp2.StatusCode, body2)
		}
		if body2["message"] != "Resource not found" {
			t.Fatalf("message = %v, want %q", body2["message"], "Resource not found")
		}
	})
}

func TestIntegration_ConnectorsConfigEncryptedAtRest(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "connectors-encrypted")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
		map[string]any{"name": "secret-db", "type": "generic", "config": map[string]any{"apiKey": "sk-super-secret-value"}},
		headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	connID, _ := body["id"].(string)
	orgUUID, err := uuid.Parse(org.ID)
	if err != nil {
		t.Fatalf("parse org id: %v", err)
	}
	connUUID, err := uuid.Parse(connID)
	if err != nil {
		t.Fatalf("parse connector id: %v", err)
	}

	row, err := store.GetConnector(context.Background(), db.GetConnectorParams{ID: connUUID, OrganizationID: orgUUID})
	if err != nil {
		t.Fatalf("GetConnector: %v", err)
	}

	raw := []byte(row.EncryptedConfig)
	if bytes.Contains(raw, []byte("sk-super-secret-value")) {
		t.Fatalf("encrypted_config leaks the plaintext secret: %s", raw)
	}
	if bytes.Contains(raw, []byte("apiKey")) {
		t.Fatalf("encrypted_config leaks the plaintext field name: %s", raw)
	}

	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("encrypted_config is not valid JSON: %v", err)
	}
	for _, key := range []string{"v", "kid", "dek", "ct"} {
		if _, ok := envelope[key]; !ok {
			t.Errorf("encrypted_config missing %q field: %v", key, envelope)
		}
	}
}

func TestIntegration_ConnectorsDuplicateName(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("same org rejects duplicate name", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "connectors-dup-same")
		headers := map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		}

		doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
			map[string]any{"name": "dup-name", "type": "generic", "config": map[string]any{"a": 1}}, headers)

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
			map[string]any{"name": "dup-name", "type": "generic", "config": map[string]any{"a": 2}}, headers)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Connector name already taken" {
			t.Fatalf("message = %v, want %q", body["message"], "Connector name already taken")
		}
	})

	t.Run("different org allows the same name", func(t *testing.T) {
		orgA := createOrgWithOwner(t, client, ts.URL, "connectors-dup-a")
		orgB := createOrgWithOwner(t, client, ts.URL, "connectors-dup-b")

		doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
			map[string]any{"name": "shared-name", "type": "generic", "config": map[string]any{"a": 1}},
			map[string]string{"Authorization": "Bearer " + orgA.Owner.AccessToken, "x-organization-id": orgA.ID})

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
			map[string]any{"name": "shared-name", "type": "generic", "config": map[string]any{"a": 2}},
			map[string]string{"Authorization": "Bearer " + orgB.Owner.AccessToken, "x-organization-id": orgB.ID})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
	})
}

func TestIntegration_ConnectorsInvalidType(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "connectors-invalid-type")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
		map[string]any{"name": "bad-type", "type": "flowaccount", "config": map[string]any{"a": 1}}, headers)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
	}
	if body["message"] != "Validation failed" {
		t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
	}
}

// TestIntegration_ConnectorsGoogleSheetsTypeAccepted proves "google_sheets"
// is now a recognized connector type end to end through the request
// validator and the service's own IsValidType check
// (docs/07-sheets-adapter-plan.md step 5) — until this step it was rejected
// exactly like "flowaccount" above. The health-check route itself is not
// exercised here: it requires a real Google OAuth token exchange, which
// this suite must not attempt (internal/adapter/googlesheets's own tests
// mock the upstream instead).
func TestIntegration_ConnectorsGoogleSheetsTypeAccepted(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "connectors-google-sheets")
	headers := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	config := map[string]any{
		"oauth": map[string]any{
			"refresh_token": "1//0g-fake-refresh-token",
			"client_id":     "fake.apps.googleusercontent.com",
			"client_secret": "fake-client-secret",
		},
		"scope": map[string]any{
			"spreadsheet_ids": []any{"1AbCFakeSpreadsheetId"},
		},
	}

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
		map[string]any{"name": uniqueSlug("sheets-connector"), "type": "google_sheets", "config": config}, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	if body["type"] != "google_sheets" {
		t.Fatalf("type = %v, want %q", body["type"], "google_sheets")
	}
	if _, ok := body["config"]; ok {
		t.Fatalf("create response leaks a config key: %v", body)
	}

	connID, _ := body["id"].(string)
	resp2, body2 := doJSON(t, client, ts.URL, http.MethodGet, "/connectors/"+connID, nil, headers)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get: status = %d, want 200; body = %v", resp2.StatusCode, body2)
	}
	if body2["type"] != "google_sheets" {
		t.Fatalf("get type = %v, want %q", body2["type"], "google_sheets")
	}
}

func TestIntegration_ConnectorsPermissionEnforcement(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "connectors-perm")
	ownerHeaders := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	t.Run("member with no role is denied read", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "connectors-perm-norole")
		inviteMember(t, client, ts.URL, org, member.Email, "member")

		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/connectors", nil,
			map[string]string{"Authorization": "Bearer " + member.AccessToken, "x-organization-id": org.ID})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Missing permission: connector:read" {
			t.Fatalf("message = %v, want %q", body["message"], "Missing permission: connector:read")
		}
	})

	t.Run("member with connector:read role can read but not write or delete", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "connectors-perm-read")
		inviteMember(t, client, ts.URL, org, member.Email, "member")
		roleID := createConnectorRole(t, client, ts.URL, org, []string{"connector:read"})
		assignConnectorRole(t, client, ts.URL, org, member.UserID, roleID)

		memberHeaders := map[string]string{"Authorization": "Bearer " + member.AccessToken, "x-organization-id": org.ID}

		resp, rows := doJSONList(t, client, ts.URL, "/connectors", memberHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("read: status = %d, want 200; body = %v", resp.StatusCode, rows)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
			map[string]any{"name": "read-only-should-fail", "type": "generic", "config": map[string]any{"a": 1}}, memberHeaders)
		if resp2.StatusCode != http.StatusForbidden {
			t.Fatalf("write: status = %d, want 403; body = %v", resp2.StatusCode, body2)
		}
		if body2["message"] != "Missing permission: connector:write" {
			t.Fatalf("write message = %v, want %q", body2["message"], "Missing permission: connector:write")
		}

		resp3, body3 := doJSON(t, client, ts.URL, http.MethodDelete, "/connectors/"+uuid.NewString(), nil, memberHeaders)
		if resp3.StatusCode != http.StatusForbidden {
			t.Fatalf("delete: status = %d, want 403; body = %v", resp3.StatusCode, body3)
		}
		if body3["message"] != "Missing permission: connector:delete" {
			t.Fatalf("delete message = %v, want %q", body3["message"], "Missing permission: connector:delete")
		}
	})

	t.Run("member with connector:* role can read, write, and delete", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "connectors-perm-wildcard")
		inviteMember(t, client, ts.URL, org, member.Email, "member")
		roleID := createConnectorRole(t, client, ts.URL, org, []string{"connector:*"})
		assignConnectorRole(t, client, ts.URL, org, member.UserID, roleID)

		memberHeaders := map[string]string{"Authorization": "Bearer " + member.AccessToken, "x-organization-id": org.ID}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
			map[string]any{"name": uniqueSlug("wildcard-connector"), "type": "generic", "config": map[string]any{"a": 1}}, memberHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("create: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		connID, _ := body["id"].(string)

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodGet, "/connectors/"+connID, nil, memberHeaders)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("read: status = %d, want 200; body = %v", resp2.StatusCode, body2)
		}

		resp3, body3 := doJSON(t, client, ts.URL, http.MethodDelete, "/connectors/"+connID, nil, memberHeaders)
		if resp3.StatusCode != http.StatusOK || body3["success"] != true {
			t.Fatalf("delete: status = %d, body = %v", resp3.StatusCode, body3)
		}
	})

	// Sanity: the owner used to set up the fixtures above must have full
	// access throughout, via RBAC's owner bypass.
	t.Run("owner bypass still works", func(t *testing.T) {
		resp, body := doJSONList(t, client, ts.URL, "/connectors", ownerHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
	})
}

func TestIntegration_ConnectorsCrossOrgIsolation(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	orgA := createOrgWithOwner(t, client, ts.URL, "connectors-isolation-a")
	orgB := createOrgWithOwner(t, client, ts.URL, "connectors-isolation-b")

	_, body := doJSON(t, client, ts.URL, http.MethodPost, "/connectors",
		map[string]any{"name": "org-a-connector", "type": "generic", "config": map[string]any{"a": 1}},
		map[string]string{"Authorization": "Bearer " + orgA.Owner.AccessToken, "x-organization-id": orgA.ID})
	connID, _ := body["id"].(string)
	if connID == "" {
		t.Fatalf("create in org A: missing id: %v", body)
	}

	orgBHeaders := map[string]string{"Authorization": "Bearer " + orgB.Owner.AccessToken, "x-organization-id": orgB.ID}

	cases := []struct {
		name, method, path string
	}{
		{"get", http.MethodGet, "/connectors/" + connID},
		{"patch", http.MethodPatch, "/connectors/" + connID},
		{"delete", http.MethodDelete, "/connectors/" + connID},
		{"health-check", http.MethodPost, "/connectors/" + connID + "/health-check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reqBody any
			if tc.method == http.MethodPatch {
				reqBody = map[string]any{"name": "should-not-apply"}
			}
			resp, body := doJSON(t, client, ts.URL, tc.method, tc.path, reqBody, orgBHeaders)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
			}
			if body["message"] != "Resource not found" {
				t.Fatalf("message = %v, want %q", body["message"], "Resource not found")
			}
		})
	}
}

func TestIntegration_ConnectorsGuardBasics(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("no authorization", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/connectors", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Unauthorized" {
			t.Fatalf("message = %v, want %q", body["message"], "Unauthorized")
		}
	})

	t.Run("missing x-organization-id header", func(t *testing.T) {
		org := createOrgWithOwner(t, client, ts.URL, "connectors-guard-noheader")

		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/connectors", nil,
			map[string]string{"Authorization": "Bearer " + org.Owner.AccessToken})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Missing x-organization-id header" {
			t.Fatalf("message = %v, want %q", body["message"], "Missing x-organization-id header")
		}
	})
}
