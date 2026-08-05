package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/junctera/backend/internal/infra/database"
	"github.com/junctera/backend/internal/infra/database/db"
)

// createPlan upserts a uniquely-named plan with the given limits and
// returns its id, for use in subscription-flow tests.
func createPlan(t *testing.T, store *database.Store, limits map[string]int) uuid.UUID {
	t.Helper()

	name := uniqueSlug("plan")
	b, err := json.Marshal(limits)
	if err != nil {
		t.Fatalf("marshal limits: %v", err)
	}
	if err := store.UpsertPlan(context.Background(), db.UpsertPlanParams{Name: name, Limits: b}); err != nil {
		t.Fatalf("UpsertPlan: %v", err)
	}
	plan, err := store.GetPlanByName(context.Background(), name)
	if err != nil {
		t.Fatalf("GetPlanByName: %v", err)
	}
	return plan.ID
}

// doJSONList issues a GET request and decodes a JSON array response, for
// the list endpoints (GET /rbac/roles, GET /audit-logs) that return a bare
// array on success rather than an object.
func doJSONList(t *testing.T, client *http.Client, baseURL, path string, headers map[string]string) (*http.Response, []map[string]any) {
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

	var decoded []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response as array: %v", err)
	}
	return resp, decoded
}

func TestIntegration_RBAC(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "rbac")
	orgHeaders := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	var roleID string

	t.Run("create role returns the raw row, no permissions key", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/rbac/roles",
			map[string]any{"name": "editor", "permissions": []string{"project:create", "project:*"}},
			orgHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["name"] != "editor" {
			t.Errorf("name = %v, want %q", body["name"], "editor")
		}
		if _, ok := body["permissions"]; ok {
			t.Errorf("expected no permissions key in the create response, got %v", body["permissions"])
		}
		id, ok := body["id"].(string)
		if !ok || id == "" {
			t.Fatalf("missing/empty id: %v", body)
		}
		roleID = id
	})

	t.Run("list roles embeds permissions", func(t *testing.T) {
		resp, roles := doJSONList(t, client, ts.URL, "/rbac/roles", orgHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		found := findByID(roles, roleID)
		if found == nil {
			t.Fatalf("expected role %s in the list, got %v", roleID, roles)
		}
		perms, ok := found["permissions"].([]any)
		if !ok || len(perms) != 2 {
			t.Fatalf("expected 2 embedded permissions, got %v", found["permissions"])
		}
	})

	t.Run("update permissions replaces the set", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/rbac/roles/"+roleID+"/permissions",
			map[string]any{"permissions": []string{"doc:read"}}, orgHeaders)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
		}

		_, roles := doJSONList(t, client, ts.URL, "/rbac/roles", orgHeaders)
		found := findByID(roles, roleID)
		perms, _ := found["permissions"].([]any)
		if len(perms) != 1 {
			t.Fatalf("expected exactly 1 permission after replace, got %v", found["permissions"])
		}
		perm0, _ := perms[0].(map[string]any)
		if perm0["action"] != "doc:read" {
			t.Errorf("action = %v, want %q", perm0["action"], "doc:read")
		}
	})

	t.Run("update permissions on unknown role", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPut, "/rbac/roles/"+uuid.NewString()+"/permissions",
			map[string]any{"permissions": []string{"doc:read"}}, orgHeaders)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Role not found" {
			t.Fatalf("message = %v, want %q", body["message"], "Role not found")
		}
	})

	t.Run("assign role to a member", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "rbac-assign-member")
		inviteMember(t, client, ts.URL, org, member.Email, "member")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/rbac/assign",
			map[string]any{"userId": member.UserID, "roleId": roleID}, orgHeaders)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
		}
	})

	t.Run("assign role to a non-member", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/rbac/assign",
			map[string]any{"userId": uuid.NewString(), "roleId": roleID}, orgHeaders)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Member not found" {
			t.Fatalf("message = %v, want %q", body["message"], "Member not found")
		}
	})

	t.Run("assign an unknown role", func(t *testing.T) {
		member := registerUser(t, client, ts.URL, "rbac-assign-unknownrole")
		inviteMember(t, client, ts.URL, org, member.Email, "member")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/rbac/assign",
			map[string]any{"userId": member.UserID, "roleId": uuid.NewString()}, orgHeaders)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Role not found" {
			t.Fatalf("message = %v, want %q", body["message"], "Role not found")
		}
	})
}

func findByID(rows []map[string]any, id string) map[string]any {
	for _, r := range rows {
		if r["id"] == id {
			return r
		}
	}
	return nil
}

func TestIntegration_Subscription(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "subscription")
	orgHeaders := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	t.Run("no subscription returns null", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/subscription", nil, orgHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if body != nil {
			t.Fatalf("expected a null body, got %v", body)
		}
	})

	planA := createPlan(t, store, map[string]int{"max_members": 5})

	t.Run("assign then get embeds the plan", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/subscription/assign",
			map[string]any{"planId": planA.String()}, orgHeaders)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("assign: status = %d, body = %v", resp.StatusCode, body)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodGet, "/subscription", nil, orgHeaders)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("get: status = %d, body = %v", resp2.StatusCode, body2)
		}
		if body2["planId"] != planA.String() {
			t.Errorf("planId = %v, want %q", body2["planId"], planA.String())
		}
		if body2["customLimits"] != nil {
			t.Errorf("customLimits = %v, want nil", body2["customLimits"])
		}
		plan, ok := body2["plan"].(map[string]any)
		if !ok {
			t.Fatalf("expected an embedded plan object, got %v", body2["plan"])
		}
		if plan["id"] != planA.String() {
			t.Errorf("embedded plan id = %v, want %q", plan["id"], planA.String())
		}
	})

	t.Run("re-assign upserts to the new plan", func(t *testing.T) {
		planB := createPlan(t, store, map[string]int{"max_members": 50})

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/subscription/assign",
			map[string]any{"planId": planB.String()}, orgHeaders)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("re-assign: status = %d, body = %v", resp.StatusCode, body)
		}

		_, body2 := doJSON(t, client, ts.URL, http.MethodGet, "/subscription", nil, orgHeaders)
		if body2["planId"] != planB.String() {
			t.Errorf("planId after re-assign = %v, want %q", body2["planId"], planB.String())
		}
	})
}

func TestIntegration_AuditLogs(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "audit")
	invitee := registerUser(t, client, ts.URL, "audit-invitee")
	inviteMember(t, client, ts.URL, org, invitee.Email, "member")

	orgHeaders := map[string]string{
		"Authorization":     "Bearer " + org.Owner.AccessToken,
		"x-organization-id": org.ID,
	}

	t.Run("lists newest first with both recorded actions", func(t *testing.T) {
		resp, logs := doJSONList(t, client, ts.URL, "/audit-logs", orgHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var sawCreated, sawInvited bool
		for _, l := range logs {
			switch l["action"] {
			case "org.created":
				sawCreated = true
			case "org.member.invited":
				sawInvited = true
			}
		}
		if !sawCreated || !sawInvited {
			t.Fatalf("expected both org.created and org.member.invited, got %v", logs)
		}
	})

	t.Run("filters by action", func(t *testing.T) {
		_, logs := doJSONList(t, client, ts.URL, "/audit-logs?action=org.created", orgHeaders)
		if len(logs) == 0 {
			t.Fatal("expected at least one org.created log")
		}
		for _, l := range logs {
			if l["action"] != "org.created" {
				t.Errorf("unexpected action %v in a filtered list", l["action"])
			}
		}
	})

	t.Run("filters by userId", func(t *testing.T) {
		_, logs := doJSONList(t, client, ts.URL, "/audit-logs?userId="+org.Owner.UserID, orgHeaders)
		if len(logs) == 0 {
			t.Fatal("expected at least one log for the owner")
		}
		for _, l := range logs {
			if l["userId"] != org.Owner.UserID {
				t.Errorf("userId = %v, want %q", l["userId"], org.Owner.UserID)
			}
		}
	})

	t.Run("limit caps the result count", func(t *testing.T) {
		_, logs := doJSONList(t, client, ts.URL, "/audit-logs?limit=1", orgHeaders)
		if len(logs) != 1 {
			t.Fatalf("expected exactly 1 log, got %d", len(logs))
		}
	})

	t.Run("limit out of range fails validation", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/audit-logs?limit=0", nil, orgHeaders)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("limit=0: status = %d, want 422; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Validation failed" {
			t.Fatalf("limit=0: message = %v", body["message"])
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodGet, "/audit-logs?limit=101", nil, orgHeaders)
		if resp2.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("limit=101: status = %d, want 422; body = %v", resp2.StatusCode, body2)
		}
	})
}

func TestIntegration_Phase4Guards(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "phase4-guards")

	for _, path := range []string{"/rbac/roles", "/subscription", "/audit-logs"} {
		label := strings.TrimPrefix(path, "/")

		t.Run(label+" missing x-organization-id", func(t *testing.T) {
			resp, body := doJSON(t, client, ts.URL, http.MethodGet, path, nil,
				map[string]string{"Authorization": "Bearer " + org.Owner.AccessToken})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
			}
			if body["message"] != "Missing x-organization-id header" {
				t.Fatalf("message = %v, want %q", body["message"], "Missing x-organization-id header")
			}
		})

		t.Run(label+" not a member", func(t *testing.T) {
			resp, body := doJSON(t, client, ts.URL, http.MethodGet, path, nil,
				map[string]string{
					"Authorization":     "Bearer " + org.Owner.AccessToken,
					"x-organization-id": uuid.NewString(),
				})
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %v", resp.StatusCode, body)
			}
			if body["message"] != "Not a member of this organization" {
				t.Fatalf("message = %v, want %q", body["message"], "Not a member of this organization")
			}
		})
	}
}
