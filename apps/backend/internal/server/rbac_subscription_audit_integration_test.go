package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
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

// assignPlanDirect puts orgID on planID by writing the row, not by calling
// an API route: there is no tenant-facing way to do this any more. POST
// /subscription/assign was removed (any org member could put their own org
// on any plan), and the only remaining path is POST
// /admin/organizations/:orgId/plan, which needs a superadmin this test has
// no reason to mint. Exercised end-to-end through the API in
// admin_mutations_integration_test.go's TestIntegration_Admin_AssignPlan.
func assignPlanDirect(t *testing.T, store *database.Store, orgID uuid.UUID, planID uuid.UUID) {
	t.Helper()

	if err := store.UpsertOrgSubscription(context.Background(), db.UpsertOrgSubscriptionParams{
		OrganizationID: orgID,
		PlanID:         planID,
	}); err != nil {
		t.Fatalf("UpsertOrgSubscription: %v", err)
	}
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

// createGenericConnector creates a "generic" connector (the skeleton type
// with no adapter, so this never makes an external call) as org's owner,
// purely to produce a connector.created audit row — used by the audit-log
// "since" and multi-action tests below as a third, distinct action
// alongside org.created/org.member.invited. name becomes both the
// connector's own name and the connector.created audit row's
// metadata.name, so callers should pass a unique name and find their row
// afterward with findByMetadataName rather than by connector id: the
// audit row's own "id" is the audit_logs row id, not the connector's id,
// and the connector's id never appears in the audit metadata.
func createGenericConnector(t *testing.T, client *http.Client, baseURL string, org createdOrg, name string) {
	t.Helper()

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/connectors",
		map[string]any{"name": name, "type": "generic", "config": map[string]any{"note": "test"}},
		map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create connector %s: status = %d, want 200; body = %v", name, resp.StatusCode, body)
	}
}

// findByMetadataName finds the audit row with the given action whose
// metadata.name matches name (connector.created's metadata is
// {"name":..., "type":...}), or nil. Use this instead of findByID to
// locate a connector-triggered audit row: the row's own "id" is the
// audit_logs row id, not the connector's id.
func findByMetadataName(rows []map[string]any, action, name string) map[string]any {
	for _, r := range rows {
		if r["action"] != action {
			continue
		}
		meta, ok := r["metadata"].(map[string]any)
		if !ok {
			continue
		}
		if meta["name"] == name {
			return r
		}
	}
	return nil
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

	// A tenant can no longer assign a plan at all: POST /subscription/assign
	// is gone, not merely denied, so the route table stops advertising a
	// capability no tenant token can use. A stale client gets the contract's
	// 404 "Route not found", the same as any unmounted path.
	t.Run("assign route is gone for tenants", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/subscription/assign",
			map[string]any{"planId": planA.String()}, orgHeaders)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("assign: status = %d, want 404; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Route not found" {
			t.Errorf("assign: message = %v, want %q", body["message"], "Route not found")
		}
	})

	t.Run("get embeds the plan once one is assigned", func(t *testing.T) {
		assignPlanDirect(t, store, uuid.MustParse(org.ID), planA)

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

	t.Run("re-assigning upserts rather than duplicating", func(t *testing.T) {
		planB := createPlan(t, store, map[string]int{"max_members": 50})
		assignPlanDirect(t, store, uuid.MustParse(org.ID), planB)

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

	t.Run("since includes rows at/after the bound and excludes older ones", func(t *testing.T) {
		// Boundary semantics: QueryAuditLogs compares with `created_at >=
		// since`, so a since value equal to a row's own createdAt must
		// still include that row (inclusive). Two connector.created rows
		// are created here, strictly ordered in time (A, then B); org's
		// setup (org.created, org.member.invited) happened even earlier,
		// in the outer test function.
		//
		// createdAt is read back from each row's own audit-log entry (via
		// findByMetadataName), not from the connector-creation response:
		// the connector row and its audit row are separate INSERTs, each
		// stamped by its own now(), so the connector's own createdAt is a
		// few hundred microseconds off from the audit row's created_at and
		// is not a safe boundary value here.
		createGenericConnector(t, client, ts.URL, org, "since-marker-a")
		_, afterA := doJSONList(t, client, ts.URL, "/audit-logs?action=connector.created", orgHeaders)
		aRow := findByMetadataName(afterA, "connector.created", "since-marker-a")
		if aRow == nil {
			t.Fatalf("expected a connector.created row for since-marker-a, got %v", afterA)
		}
		aCreatedAt, _ := aRow["createdAt"].(string)

		time.Sleep(5 * time.Millisecond) // guarantee a distinct, later created_at for B
		createGenericConnector(t, client, ts.URL, org, "since-marker-b")
		_, afterB := doJSONList(t, client, ts.URL, "/audit-logs?action=connector.created", orgHeaders)
		bRow := findByMetadataName(afterB, "connector.created", "since-marker-b")
		if bRow == nil {
			t.Fatalf("expected a connector.created row for since-marker-b, got %v", afterB)
		}
		bCreatedAt, _ := bRow["createdAt"].(string)

		// Inclusive lower bound: since = A's own createdAt includes A (and
		// B, created after it), but excludes the org-setup rows, which are
		// strictly older.
		_, logsFromA := doJSONList(t, client, ts.URL, "/audit-logs?since="+url.QueryEscape(aCreatedAt), orgHeaders)
		if findByMetadataName(logsFromA, "connector.created", "since-marker-a") == nil {
			t.Fatalf("since=%s (A's own createdAt) should include A, got %v", aCreatedAt, logsFromA)
		}
		if findByMetadataName(logsFromA, "connector.created", "since-marker-b") == nil {
			t.Fatalf("since=%s should include B (created after A), got %v", aCreatedAt, logsFromA)
		}
		for _, l := range logsFromA {
			if l["action"] == "org.created" || l["action"] == "org.member.invited" {
				t.Errorf("since=%s should exclude older org-setup rows, got %v in %v", aCreatedAt, l["action"], l)
			}
		}

		// Strictly later bound: since = B's own createdAt still includes B
		// (inclusive) but now excludes A (older than B).
		_, logsFromB := doJSONList(t, client, ts.URL, "/audit-logs?since="+url.QueryEscape(bCreatedAt), orgHeaders)
		if findByMetadataName(logsFromB, "connector.created", "since-marker-b") == nil {
			t.Fatalf("since=%s (B's own createdAt) should include B, got %v", bCreatedAt, logsFromB)
		}
		if findByMetadataName(logsFromB, "connector.created", "since-marker-a") != nil {
			t.Fatalf("since=%s should exclude A (strictly older than B), got %v", bCreatedAt, logsFromB)
		}
	})

	t.Run("malformed since fails validation", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/audit-logs?since=not-a-timestamp", nil, orgHeaders)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Validation failed" {
			t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
		}
	})

	t.Run("multi-action returns the union and nothing else", func(t *testing.T) {
		createGenericConnector(t, client, ts.URL, org, "multi-action-marker")

		_, logs := doJSONList(t, client, ts.URL,
			"/audit-logs?action=org.created&action=connector.created", orgHeaders)

		var sawOrgCreated, sawConnectorCreated bool
		for _, l := range logs {
			switch l["action"] {
			case "org.created":
				sawOrgCreated = true
			case "connector.created":
				sawConnectorCreated = true
			default:
				t.Errorf("expected only org.created/connector.created, got %v in %v", l["action"], l)
			}
		}
		if !sawOrgCreated || !sawConnectorCreated {
			t.Fatalf("expected the union of both actions, got %v", logs)
		}
		if findByMetadataName(logs, "connector.created", "multi-action-marker") == nil {
			t.Fatalf("expected the multi-action-marker connector.created row in the result, got %v", logs)
		}
		// org.member.invited is excluded from the requested union; the
		// switch above already fails the test (via its default case) if
		// any action outside {org.created, connector.created} appears.
	})

	t.Run("single action=x behaves exactly as before (back-compat)", func(t *testing.T) {
		resp, logs := doJSONList(t, client, ts.URL, "/audit-logs?action=org.created", orgHeaders)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if len(logs) == 0 {
			t.Fatal("expected at least one org.created log")
		}
		for _, l := range logs {
			if l["action"] != "org.created" {
				t.Errorf("unexpected action %v in a single-action filtered list", l["action"])
			}
		}
	})

	t.Run("no action filter still returns everything", func(t *testing.T) {
		// Guards against the empty-array trap: if an absent ?action=
		// param were ever bound as a non-nil empty slice instead of nil,
		// QueryAuditLogs' `action = ANY($actions)` would match zero rows
		// instead of "unfiltered". By this point the org has at least
		// org.created, org.member.invited, and several connector.created
		// rows from earlier subtests.
		_, logs := doJSONList(t, client, ts.URL, "/audit-logs?limit=100", orgHeaders)
		var sawOrgCreated, sawConnectorCreated bool
		for _, l := range logs {
			switch l["action"] {
			case "org.created":
				sawOrgCreated = true
			case "connector.created":
				sawConnectorCreated = true
			}
		}
		if !sawOrgCreated || !sawConnectorCreated {
			t.Fatalf("expected an unfiltered list to include both org.created and connector.created, got %v", logs)
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
