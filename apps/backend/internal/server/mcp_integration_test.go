package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/module/auditlog"
)

// ---- helpers ----

// createTestConnector creates a "generic" connector in org via POST
// /connectors, using org's owner as the caller, and returns its id.
func createTestConnector(t *testing.T, client *http.Client, baseURL string, org createdOrg, name string) string {
	t.Helper()

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/connectors",
		map[string]any{"name": name, "type": "generic", "config": map[string]any{"host": "db.example.com"}},
		map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create connector: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create connector: missing id: %v", body)
	}
	return id
}

// createGoogleSheetsTestConnector creates a "google_sheets" connector in org
// via POST /connectors, using org's owner as the caller, with a config that
// allowlists exactly one spreadsheet id — a fake but plausibly-shaped
// refresh token/client id/secret, mirroring
// connector_integration_test.go's TestIntegration_ConnectorsGoogleSheetsTypeAccepted.
// Every test that uses this connector must stop short of any code path that
// would actually exchange the refresh token (SPREADSHEET_NOT_ALLOWED and
// input-bound rejections both do, by design — see tools_sheets.go's check
// ordering) since this suite has no real Google credentials and must not
// attempt real network calls.
func createGoogleSheetsTestConnector(t *testing.T, client *http.Client, baseURL string, org createdOrg, name, allowlistedSpreadsheetID string) string {
	t.Helper()

	config := map[string]any{
		"oauth": map[string]any{
			"refresh_token": "1//0g-fake-refresh-token",
			"client_id":     "fake.apps.googleusercontent.com",
			"client_secret": "fake-client-secret",
		},
		"scope": map[string]any{
			"spreadsheet_ids": []any{allowlistedSpreadsheetID},
		},
	}

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/connectors",
		map[string]any{"name": name, "type": "google_sheets", "config": config},
		map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create google_sheets connector: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create google_sheets connector: missing id: %v", body)
	}
	return id
}

// mintMCPKeyAs mints an MCP API key using callerToken as the bearer (must
// carry mcpkey:write in org), and returns its id and raw apiKey — the
// caller's only opportunity to see the raw value, mirroring
// mcpkey.Service.Create.
func mintMCPKeyAs(t *testing.T, client *http.Client, baseURL, orgID, callerToken, name string) (id, apiKey string) {
	t.Helper()

	resp, body := doJSON(t, client, baseURL, http.MethodPost, "/mcp-keys",
		map[string]any{"name": name},
		map[string]string{
			"Authorization":     "Bearer " + callerToken,
			"x-organization-id": orgID,
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint mcp key: status = %d, want 200; body = %v", resp.StatusCode, body)
	}
	id, _ = body["id"].(string)
	apiKey, _ = body["apiKey"].(string)
	if id == "" || apiKey == "" {
		t.Fatalf("mint mcp key: missing id/apiKey: %v", body)
	}
	return id, apiKey
}

// bearerRoundTripper attaches an MCP PAT the way a real MCP client's
// configured Authorization header would — mirrors
// spikes/mcp-gateway/internal/gateway/gateway_test.go's bearerRoundTripper.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

// tryConnectMCP attempts a real MCP handshake against baseURL's
// /mcp/:connectorId endpoint with apiKey as the bearer credential, without
// failing the test — callers assert on the returned error themselves.
func tryConnectMCP(t *testing.T, baseURL, connectorID, apiKey string) (*gomcp.ClientSession, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	client := gomcp.NewClient(&gomcp.Implementation{Name: "mcp-integration-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, &gomcp.StreamableClientTransport{
		Endpoint:   baseURL + "/mcp/" + connectorID,
		HTTPClient: &http.Client{Transport: &bearerRoundTripper{token: apiKey, base: http.DefaultTransport}},
	}, nil)
	if cs != nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

// connectMCP is tryConnectMCP but fails the test on any handshake error —
// for the happy-path cases that must succeed.
func connectMCP(t *testing.T, baseURL, connectorID, apiKey string) *gomcp.ClientSession {
	t.Helper()
	cs, err := tryConnectMCP(t, baseURL, connectorID, apiKey)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	return cs
}

func toolNames(t *testing.T, cs *gomcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// ---- guard rejections: no MCP handshake needed, RequireMCPKey/resolveConnector short-circuit first ----

func TestIntegration_MCP_NoKey(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-guard-nokey")
	connID := createTestConnector(t, client, ts.URL, org, "conn-nokey")

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp/"+connID,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
	}
	if body["message"] != "Unauthorized" {
		t.Fatalf("message = %v, want %q", body["message"], "Unauthorized")
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="sapanjai"` {
		t.Errorf("WWW-Authenticate = %q, want %q", got, `Bearer realm="sapanjai"`)
	}
}

func TestIntegration_MCP_MalformedToken(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-guard-malformed")
	connID := createTestConnector(t, client, ts.URL, org, "conn-malformed")

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp/"+connID,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
		map[string]string{"Authorization": "Bearer not-a-real-mcp-key"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
	}
	if body["message"] != "Unauthorized" {
		t.Fatalf("message = %v, want %q", body["message"], "Unauthorized")
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="sapanjai", error="invalid_token"` {
		t.Errorf("WWW-Authenticate = %q, want the invalid_token variant", got)
	}
}

func TestIntegration_MCP_RevokedKey(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-guard-revoked")
	connID := createTestConnector(t, client, ts.URL, org, "conn-revoked")
	keyID, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "revoke-me")

	resp, body := doJSON(t, client, ts.URL, http.MethodDelete, "/mcp-keys/"+keyID, nil,
		map[string]string{"Authorization": "Bearer " + org.Owner.AccessToken, "x-organization-id": org.ID})
	if resp.StatusCode != http.StatusOK || body["success"] != true {
		t.Fatalf("revoke: status = %d, body = %v", resp.StatusCode, body)
	}

	resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/mcp/"+connID,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
		map[string]string{"Authorization": "Bearer " + apiKey})
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %v", resp2.StatusCode, body2)
	}
	if body2["message"] != "Unauthorized" {
		t.Fatalf("message = %v, want %q", body2["message"], "Unauthorized")
	}
}

func TestIntegration_MCP_ExpiredKey(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-guard-expired")
	connID := createTestConnector(t, client, ts.URL, org, "conn-expired")
	keyID, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "expire-me")

	// The API only accepts a future expiresInDays at mint time; backdating
	// requires a direct write, which is exactly the kind of assertion
	// docs/07-sheets-adapter-plan.md step 3 calls for testing via SQL.
	if _, err := store.Pool.Exec(context.Background(),
		`UPDATE mcp_api_keys SET expires_at = now() - interval '1 hour' WHERE id = $1`, keyID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp/"+connID,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
		map[string]string{"Authorization": "Bearer " + apiKey})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
	}
	if body["message"] != "Unauthorized" {
		t.Fatalf("message = %v, want %q", body["message"], "Unauthorized")
	}
}

// ---- tenant isolation ----

func TestIntegration_MCP_CrossOrgConnectorIsNotFound(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	orgA := createOrgWithOwner(t, client, ts.URL, "mcp-isolation-a")
	orgB := createOrgWithOwner(t, client, ts.URL, "mcp-isolation-b")
	connB := createTestConnector(t, client, ts.URL, orgB, "conn-org-b")
	_, apiKeyA := mintMCPKeyAs(t, client, ts.URL, orgA.ID, orgA.Owner.AccessToken, "org-a-key")

	resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/mcp/"+connB,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
		map[string]string{"Authorization": "Bearer " + apiKeyA})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %v", resp.StatusCode, body)
	}
	if body["message"] != "Resource not found" {
		t.Fatalf("message = %v, want %q", body["message"], "Resource not found")
	}
}

// ---- RBAC filtering: absent from tools/list AND IsError on a direct call ----

func TestIntegration_MCP_PrincipalWithoutConnectorReadSeesNoTools(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-perm-denied")
	connID := createTestConnector(t, client, ts.URL, org, "conn-perm-denied")

	member := registerUser(t, client, ts.URL, "mcp-perm-denied-member")
	inviteMember(t, client, ts.URL, org, member.Email, "member")
	// mcpkey:write only — enough to mint their own key, deliberately no
	// connector:read.
	roleID := createConnectorRole(t, client, ts.URL, org, []string{"mcpkey:write"})
	assignConnectorRole(t, client, ts.URL, org, member.UserID, roleID)
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, member.AccessToken, "no-connector-read")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	if got := toolNames(t, cs); len(got) != 0 {
		t.Errorf("tools/list = %v, want no tools for a principal without connector:read", got)
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		// SDK reports an unregistered tool as a protocol error — a refusal
		// either way, since the tool was never registered on this server.
		return
	}
	if !res.IsError {
		t.Fatal("denied tool call succeeded for a principal without connector:read")
	}
}

// ---- scoped-key narrowing (Decision 1) ----

func TestIntegration_MCP_ScopedKeyNarrowsOwnerBypass(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-scoped-owner")
	connID := createTestConnector(t, client, ts.URL, org, "conn-scoped-owner")
	keyID, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "scoped-key")

	// The owner would normally see sapanjai_describe_connector via the
	// owner bypass. Scope this specific key to an unrelated action —
	// mcpkey:read, nothing to do with connector:read — directly via SQL,
	// exactly as step 3 prescribes for exercising a code path nothing else
	// writes yet.
	if _, err := store.Pool.Exec(context.Background(),
		`UPDATE mcp_api_keys SET scopes = $1 WHERE id = $2`, []string{"mcpkey:read"}, keyID); err != nil {
		t.Fatalf("set scopes: %v", err)
	}

	cs := connectMCP(t, ts.URL, connID, apiKey)

	if got := toolNames(t, cs); len(got) != 0 {
		t.Fatalf("tools/list = %v, want no tools: an owner-held key scoped away from connector:read must not "+
			"fall back to the owner bypass", got)
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err == nil && !res.IsError {
		t.Fatal("owner bypass leaked through a non-NULL scopes list that excluded connector:read")
	}
}

func TestIntegration_MCP_ScopedKeyIncludingPermissionStillWorks(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-scoped-included")
	connID := createTestConnector(t, client, ts.URL, org, "conn-scoped-included")
	keyID, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "scoped-key-included")

	if _, err := store.Pool.Exec(context.Background(),
		`UPDATE mcp_api_keys SET scopes = $1 WHERE id = $2`, []string{"connector:read"}, keyID); err != nil {
		t.Fatalf("set scopes: %v", err)
	}

	cs := connectMCP(t, ts.URL, connID, apiKey)

	got := toolNames(t, cs)
	if len(got) != 1 || got[0] != "sapanjai_describe_connector" {
		t.Fatalf("tools/list = %v, want [sapanjai_describe_connector] when scopes include connector:read", got)
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("call denied despite scopes including connector:read: %v", res.Content)
	}
}

// ---- happy path: initialize -> tools/list -> tools/call, plus audit ----

func TestIntegration_MCP_HappyPath(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-happy-path")
	connID := createTestConnector(t, client, ts.URL, org, "warehouse-db")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "happy-path-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	if info := cs.InitializeResult().ServerInfo; info.Name == "" {
		t.Error("initialize did not return a server name")
	}

	got := toolNames(t, cs)
	if len(got) != 1 || got[0] != "sapanjai_describe_connector" {
		t.Fatalf("tools/list = %v, want [sapanjai_describe_connector] for the owner", got)
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("tools/call errored: %v", res.Content)
	}

	t.Run("audit trail recorded", func(t *testing.T) {
		_, rows := doJSONList(t, client, ts.URL, "/audit-logs", map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
		var sawSessionStarted, sawToolCalled bool
		for _, r := range rows {
			switch r["action"] {
			case "mcp.session.started":
				sawSessionStarted = true
			case "mcp.tool.called":
				sawToolCalled = true
			}
		}
		if !sawSessionStarted {
			t.Error("no mcp.session.started audit row found")
		}
		if !sawToolCalled {
			t.Error("no mcp.tool.called audit row found")
		}
	})
}

// ---- rate limiting (docs/07-sheets-adapter-plan.md step 4) ----

// TestIntegration_MCP_RateLimitTripsAndAudits exhausts a connector's
// per-minute bucket and confirms the next tools/call is refused cleanly —
// IsError with a RATE_LIMITED message stating a retry-after in seconds,
// never a protocol error or an HTTP failure — plus exactly one
// mcp.ratelimit.hit audit row. MCPRateLimitPerMin is set to 2 for this test
// via setupTestServer's configure hook, so tripping the limit takes 3 calls
// instead of the production default's 61.
func TestIntegration_MCP_RateLimitTripsAndAudits(t *testing.T) {
	ts, _, _ := setupTestServer(t, func(cfg *config.Config) {
		cfg.MCPRateLimitPerMin = 2
	})
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-ratelimit")
	connID := createTestConnector(t, client, ts.URL, org, "conn-ratelimit")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "ratelimit-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	// The first 2 calls spend the whole budget: capacity 2, floor of 1 unit
	// charged per tools/call (no real adapter exists yet to charge more).
	for i := 0; i < 2; i++ {
		res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if res.IsError {
			t.Fatalf("call %d unexpectedly errored: %v", i+1, res.Content)
		}
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		t.Fatalf("3rd call: transport/protocol error %v, want a clean IsError result", err)
	}
	if !res.IsError {
		t.Fatal("3rd call succeeded, want RATE_LIMITED: the 2-token budget was already spent")
	}
	text, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "RATE_LIMITED") {
		t.Errorf("text = %q, want it to mention RATE_LIMITED", text.Text)
	}
	if !strings.Contains(text.Text, "Retry after") || !strings.Contains(text.Text, "seconds") {
		t.Errorf("text = %q, want a stated retry-after in seconds so the agent can adapt", text.Text)
	}

	t.Run("audit trail has exactly one ratelimit hit", func(t *testing.T) {
		_, rows := doJSONList(t, client, ts.URL, "/audit-logs?action=mcp.ratelimit.hit", map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
		if len(rows) != 1 {
			t.Fatalf("mcp.ratelimit.hit rows = %d, want exactly 1 (one 3rd-call denial, nothing else in this org)", len(rows))
		}
	})
}

// ---- Google Sheets tools (docs/07-sheets-adapter-plan.md step 6) ----

// TestIntegration_MCP_SheetsToolsInvisibleWithoutSheetsRead proves the
// plan's required visibility test end to end: a member with some other
// grant (mcpkey:write, enough to mint their own key) but no sheets:read
// sees neither sheets_list_spreadsheets nor sheets_describe_spreadsheet
// against a real google_sheets connector, and a direct call is refused.
func TestIntegration_MCP_SheetsToolsInvisibleWithoutSheetsRead(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-sheets-perm-denied")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "sheets-conn-perm-denied", "1AbCFakeSpreadsheetId")

	member := registerUser(t, client, ts.URL, "mcp-sheets-perm-denied-member")
	inviteMember(t, client, ts.URL, org, member.Email, "member")
	roleID := createConnectorRole(t, client, ts.URL, org, []string{"mcpkey:write"})
	assignConnectorRole(t, client, ts.URL, org, member.UserID, roleID)
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, member.AccessToken, "no-sheets-read")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	if got := toolNames(t, cs); len(got) != 0 {
		t.Errorf("tools/list = %v, want no tools for a principal without sheets:read", got)
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sheets_describe_spreadsheet"})
	if err != nil {
		return // unregistered-tool protocol error is a refusal too
	}
	if !res.IsError {
		t.Fatal("sheets_describe_spreadsheet succeeded for a principal without sheets:read")
	}
}

// TestIntegration_MCP_SheetsToolsVisibleWithSheetsRead is the positive
// counterpart: a member granted exactly sheets:read (nothing else) sees
// both tools in tools/list. It never calls either tool — this suite has no
// real Google credentials — visibility alone is the assertion.
func TestIntegration_MCP_SheetsToolsVisibleWithSheetsRead(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-sheets-perm-granted")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "sheets-conn-perm-granted", "1AbCFakeSpreadsheetId")

	member := registerUser(t, client, ts.URL, "mcp-sheets-perm-granted-member")
	inviteMember(t, client, ts.URL, org, member.Email, "member")
	roleID := createConnectorRole(t, client, ts.URL, org, []string{"mcpkey:write", "sheets:read"})
	assignConnectorRole(t, client, ts.URL, org, member.UserID, roleID)
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, member.AccessToken, "with-sheets-read")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	got := toolNames(t, cs)
	want := map[string]bool{
		"sheets_list_spreadsheets":    true,
		"sheets_describe_spreadsheet": true,
		"sheets_query_rows":           true,
		"sheets_read_range":           true,
	}
	if len(got) != 4 {
		t.Fatalf("tools/list = %v, want exactly the 4 sheets tools", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected tool %q in tools/list", name)
		}
	}
}

// TestIntegration_MCP_SheetsToolsAbsentFromGenericConnector proves
// connector-type gating end to end: even the owner, who bypasses RBAC
// entirely, never sees the Sheets tools against a plain "generic"
// connector — mirrors TestIntegration_MCP_HappyPath's existing assertion
// that a generic connector's tools/list is exactly
// [sapanjai_describe_connector], now stated as its own test so a future
// regression here is unambiguous about which invariant broke.
func TestIntegration_MCP_SheetsToolsAbsentFromGenericConnector(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-sheets-wrong-type")
	connID := createTestConnector(t, client, ts.URL, org, "generic-conn")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "wrong-type-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	got := toolNames(t, cs)
	if len(got) != 1 || got[0] != "sapanjai_describe_connector" {
		t.Fatalf("tools/list = %v, want only sapanjai_describe_connector against a generic connector", got)
	}
}

// TestIntegration_MCP_DescribeSpreadsheet_SpreadsheetNotAllowed is the
// plan's required allowlist test: connID's config allowlists exactly
// "1AbCFakeSpreadsheetId"; calling describe with a different,
// plausibly-real-shaped id must be rejected by the allowlist alone — before
// any OAuth token exchange, which would fail loudly (this suite's refresh
// token is fake) if the check ran too late or not at all. Also asserts the
// mcp.tool.called audit row records spreadsheet_id, per docs/06-sheets-
// adapter.md §7.
func TestIntegration_MCP_DescribeSpreadsheet_SpreadsheetNotAllowed(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-sheets-not-allowed")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "sheets-conn-not-allowed", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "not-allowed-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	const notAllowedID = "1XyZDifferentRealLookingSpreadsheetId"
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "sheets_describe_spreadsheet",
		Arguments: map[string]any{"spreadsheet_id": notAllowedID},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for a spreadsheet id absent from the allowlist")
	}
	text, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "SPREADSHEET_NOT_ALLOWED") {
		t.Errorf("text = %q, want it to mention SPREADSHEET_NOT_ALLOWED", text.Text)
	}

	t.Run("audit row records spreadsheet_id, never cell values", func(t *testing.T) {
		_, rows := doJSONList(t, client, ts.URL, "/audit-logs?action=mcp.tool.called", map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
		var found bool
		for _, r := range rows {
			// doJSON/doJSONList decode the response body's JSON directly,
			// so the metadata object (json.RawMessage on the wire) arrives
			// here as a map[string]any, not a string to re-unmarshal.
			meta, ok := r["metadata"].(map[string]any)
			if !ok {
				continue
			}
			if meta["tool"] == "sheets_describe_spreadsheet" {
				found = true
				if meta["spreadsheet_id"] != notAllowedID {
					t.Errorf("audit metadata spreadsheet_id = %v, want %q", meta["spreadsheet_id"], notAllowedID)
				}
			}
		}
		if !found {
			t.Error("no mcp.tool.called audit row found for sheets_describe_spreadsheet")
		}
	})
}

// TestIntegration_MCP_DescribeSpreadsheet_IncludeSampleRowsBounded is the
// plan's required bound test: include_sample_rows outside 0-5 must be
// rejected — using the connector's *allowlisted* spreadsheet id, so the
// rejection is unambiguously about the bound and not the allowlist, while
// still never reaching the network (this suite has no real Google
// credentials).
func TestIntegration_MCP_DescribeSpreadsheet_IncludeSampleRowsBounded(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-sheets-bound")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "sheets-conn-bound", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "bound-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	for _, n := range []int{-1, 6} {
		res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
			Name: "sheets_describe_spreadsheet",
			Arguments: map[string]any{
				"spreadsheet_id":      "1AbCFakeSpreadsheetId",
				"include_sample_rows": n,
			},
		})
		if err != nil {
			t.Fatalf("include_sample_rows=%d: call: %v", n, err)
		}
		if !res.IsError {
			t.Fatalf("include_sample_rows=%d: expected IsError, got success", n)
		}
	}
}

// ---- sheets_query_rows (docs/07-sheets-adapter-plan.md step 7) ----

// TestIntegration_MCP_QueryRows_SpreadsheetNotAllowed mirrors
// TestIntegration_MCP_DescribeSpreadsheet_SpreadsheetNotAllowed: the
// allowlist check runs before any config-derived OAuth token exchange, so
// this is safe to drive end to end against a fake refresh token. It also
// asserts the step 7 audit-timing resolution: mcp.tool.called now carries
// duration_ms (available only once the call has actually returned) and
// filter_columns (the column NAMES a query filtered on, extracted
// pre-dispatch) — and never the filter's actual value, which here is
// business data ("หจก. ก่อสร้าง") that must not appear anywhere in the
// audit trail.
// TestIntegration_MCP_ToolCallAuditSurvivesClientDisconnect is the
// regression test for a bug found reviewing step 7. Step 7 moved
// mcp.tool.called from before dispatch to after it, so that row_count and
// duration_ms could land in the same row — the right call, but it put the
// audit write on the far side of a scan that can run for seconds across
// several page fetches. auditlog.Record writes synchronously on the
// request context, so a client hanging up mid-scan cancelled it and the
// row silently never landed: the audit trail went missing for exactly the
// abandoned and timed-out calls that most warrant one. Verified against
// the real database before the fix — zero rows written under a cancelled
// context. enforce now defers the write on a context.WithoutCancel copy.
func TestIntegration_MCP_ToolCallAuditSurvivesClientDisconnect(t *testing.T) {
	_, _, store := setupTestServer(t)
	svc := auditlog.NewService(store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	orgID := uuid.New()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // the client hung up mid-scan

	svc.Record(context.WithoutCancel(cancelled), "mcp.tool.called", nil, &orgID,
		[]byte(`{"tool":"sheets_query_rows"}`))

	var n int
	if err := store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE organization_id = $1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit rows recorded after a client disconnect = %d, want 1", n)
	}
}

func TestIntegration_MCP_QueryRows_SpreadsheetNotAllowed(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-query-not-allowed")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "query-conn-not-allowed", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "query-not-allowed-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	const notAllowedID = "1XyZDifferentRealLookingSpreadsheetId"
	const secretPartnerName = "หจก. ก่อสร้าง จำกัด"
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "sheets_query_rows",
		Arguments: map[string]any{
			"spreadsheet_id": notAllowedID,
			"sheet_name":     "Contracts",
			"filters": []any{
				map[string]any{"column": "partner_name", "op": "eq", "value": secretPartnerName},
			},
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for a spreadsheet id absent from the allowlist")
	}
	text, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "SPREADSHEET_NOT_ALLOWED") {
		t.Errorf("text = %q, want it to mention SPREADSHEET_NOT_ALLOWED", text.Text)
	}

	t.Run("audit row records filter_columns, duration_ms, never the filter value", func(t *testing.T) {
		_, rows := doJSONList(t, client, ts.URL, "/audit-logs?action=mcp.tool.called", map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
		var found bool
		for _, r := range rows {
			meta, ok := r["metadata"].(map[string]any)
			if !ok {
				continue
			}
			if meta["tool"] != "sheets_query_rows" {
				continue
			}
			found = true
			if meta["spreadsheet_id"] != notAllowedID {
				t.Errorf("audit metadata spreadsheet_id = %v, want %q", meta["spreadsheet_id"], notAllowedID)
			}
			cols, ok := meta["filter_columns"].([]any)
			if !ok || len(cols) != 1 || cols[0] != "partner_name" {
				t.Errorf("audit metadata filter_columns = %v, want [\"partner_name\"]", meta["filter_columns"])
			}
			if _, ok := meta["duration_ms"]; !ok {
				t.Error("audit metadata missing duration_ms — the audit-timing fix should populate it post-dispatch")
			}
		}
		if !found {
			t.Fatal("no mcp.tool.called audit row found for sheets_query_rows")
		}

		// The secret filter value must never appear anywhere in the audit
		// trail's raw JSON — not as a metadata value, not nested, nowhere.
		_, raw := doJSONList(t, client, ts.URL, "/audit-logs?action=mcp.tool.called", map[string]string{
			"Authorization":     "Bearer " + org.Owner.AccessToken,
			"x-organization-id": org.ID,
		})
		for _, r := range raw {
			for _, v := range r {
				if s, ok := v.(string); ok && strings.Contains(s, secretPartnerName) {
					t.Fatalf("audit row leaked the filter value: %v", r)
				}
			}
		}
	})
}

// TestIntegration_MCP_QueryRows_LimitBound is the plan's required test:
// limit=201 must be rejected. Uses the connector's allowlisted spreadsheet
// id so the rejection is unambiguously about the limit bound, and never
// reaches the network — input validation runs before config decryption.
func TestIntegration_MCP_QueryRows_LimitBound(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-query-limit-bound")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "query-conn-limit-bound", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "limit-bound-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "sheets_query_rows",
		Arguments: map[string]any{
			"spreadsheet_id": "1AbCFakeSpreadsheetId",
			"sheet_name":     "Contracts",
			"limit":          201,
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("limit=201: expected IsError, got success")
	}
}

// TestIntegration_MCP_QueryRows_MissingSheetName proves sheet_name is
// required and checked before any config decryption or network call.
func TestIntegration_MCP_QueryRows_MissingSheetName(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-query-missing-sheet")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "query-conn-missing-sheet", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "missing-sheet-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "sheets_query_rows",
		Arguments: map[string]any{
			"spreadsheet_id": "1AbCFakeSpreadsheetId",
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("missing sheet_name: expected IsError, got success")
	}
}

// TestIntegration_MCP_QueryRows_InvalidFilterOperator proves a filter with
// an unsupported operator is rejected before any network call — this is
// also, structurally, the guarantee that no free-form query language can
// ever be smuggled in as a bogus "op" value.
func TestIntegration_MCP_QueryRows_InvalidFilterOperator(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-query-bad-op")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "query-conn-bad-op", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "bad-op-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "sheets_query_rows",
		Arguments: map[string]any{
			"spreadsheet_id": "1AbCFakeSpreadsheetId",
			"sheet_name":     "Contracts",
			"filters": []any{
				map[string]any{"column": "status", "op": "regexp", "value": ".*"},
			},
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("unsupported operator: expected IsError, got success")
	}
}

// ---- sheets_read_range (docs/07-sheets-adapter-plan.md step 8) ----

// TestIntegration_MCP_ReadRange_SpreadsheetNotAllowed mirrors
// TestIntegration_MCP_QueryRows_SpreadsheetNotAllowed: the allowlist check
// runs before any config-derived OAuth token exchange, so this is safe to
// drive end to end against a fake refresh token — a well-formed, bounded
// range is used so the rejection is unambiguously about the allowlist, not
// the range's own shape.
func TestIntegration_MCP_ReadRange_SpreadsheetNotAllowed(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-readrange-not-allowed")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "readrange-conn-not-allowed", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "readrange-not-allowed-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	const notAllowedID = "1XyZDifferentRealLookingSpreadsheetId"
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "sheets_read_range",
		Arguments: map[string]any{
			"spreadsheet_id": notAllowedID,
			"range":          "Contracts!A1:D100",
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for a spreadsheet id absent from the allowlist")
	}
	text, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "SPREADSHEET_NOT_ALLOWED") {
		t.Errorf("text = %q, want it to mention SPREADSHEET_NOT_ALLOWED", text.Text)
	}
}

// TestIntegration_MCP_ReadRange_UnboundedRangeRejected proves an unbounded
// range (docs/07 step 8: a bare sheet name, a column-only range) is
// rejected before any config decryption or network call — using the
// connector's allowlisted spreadsheet id, so the rejection is unambiguously
// about the range's own shape, and the error names the fix.
func TestIntegration_MCP_ReadRange_UnboundedRangeRejected(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-readrange-unbounded")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "readrange-conn-unbounded", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "readrange-unbounded-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	for _, rangeStr := range []string{"Contracts", "Contracts!A:D"} {
		res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
			Name: "sheets_read_range",
			Arguments: map[string]any{
				"spreadsheet_id": "1AbCFakeSpreadsheetId",
				"range":          rangeStr,
			},
		})
		if err != nil {
			t.Fatalf("range=%q: call: %v", rangeStr, err)
		}
		if !res.IsError {
			t.Fatalf("range=%q: expected IsError, got success", rangeStr)
		}
		text, ok := res.Content[0].(*gomcp.TextContent)
		if !ok {
			t.Fatalf("range=%q: content[0] = %#v, want *TextContent", rangeStr, res.Content[0])
		}
		if !strings.Contains(text.Text, "row bounds") {
			t.Errorf("range=%q: text = %q, want it to name the fix (explicit row bounds)", rangeStr, text.Text)
		}
	}
}

// TestIntegration_MCP_ReadRange_MissingRange proves range is required and
// checked before any config decryption or network call.
func TestIntegration_MCP_ReadRange_MissingRange(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-readrange-missing")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "readrange-conn-missing", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "readrange-missing-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "sheets_read_range",
		Arguments: map[string]any{
			"spreadsheet_id": "1AbCFakeSpreadsheetId",
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("missing range: expected IsError, got success")
	}
}

// TestIntegration_MCP_ReadRange_OverCapRejected proves the pre-fetch
// row/cell cap (docs/07 step 8: 1,000 rows, 20,000 cells) is enforced
// before any config decryption or network call, using the connector's
// allowlisted spreadsheet id so the rejection is unambiguously about the
// range's size.
func TestIntegration_MCP_ReadRange_OverCapRejected(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	org := createOrgWithOwner(t, client, ts.URL, "mcp-readrange-overcap")
	connID := createGoogleSheetsTestConnector(t, client, ts.URL, org, "readrange-conn-overcap", "1AbCFakeSpreadsheetId")
	_, apiKey := mintMCPKeyAs(t, client, ts.URL, org.ID, org.Owner.AccessToken, "readrange-overcap-key")

	cs := connectMCP(t, ts.URL, connID, apiKey)

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "sheets_read_range",
		Arguments: map[string]any{
			"spreadsheet_id": "1AbCFakeSpreadsheetId",
			"range":          "Contracts!A1:A1001",
		},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("range spanning 1001 rows: expected IsError, got success")
	}
	text, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "narrow the range") {
		t.Errorf("text = %q, want it to name the fix (narrow the range)", text.Text)
	}
}
