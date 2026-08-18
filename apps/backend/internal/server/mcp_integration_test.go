package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/config"
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
