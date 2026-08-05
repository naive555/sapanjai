package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/junctera/spikes/mcp-gateway/internal/gateway"
	"github.com/junctera/spikes/mcp-gateway/internal/mockdata"
	"github.com/junctera/spikes/mcp-gateway/internal/principal"
	"github.com/junctera/spikes/mcp-gateway/internal/rbac"
)

// connect drives a real MCP handshake against a scoped server over the
// in-memory transport pair, exercising the same code path as stdio.
func connect(t *testing.T, p *rbac.Principal) *mcp.ClientSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	serverT, clientT := mcp.NewInMemoryTransports()

	server := gateway.BuildServer(p, nil)
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "spike-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect (handshake failed): %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func mustPrincipal(t *testing.T, token string) *rbac.Principal {
	t.Helper()
	p, err := principal.Resolve(token)
	if err != nil {
		t.Fatalf("resolve %s: %v", token, err)
	}
	return p
}

// TestHandshake is task item 2's minimum bar: initialize completes and the
// server identifies itself.
func TestHandshake(t *testing.T) {
	cs := connect(t, mustPrincipal(t, "tok_owner_siam"))

	info := cs.InitializeResult().ServerInfo
	if info.Name != gateway.ServerName {
		t.Errorf("server name = %q, want %q", info.Name, gateway.ServerName)
	}
	if info.Version != gateway.ServerVersion {
		t.Errorf("server version = %q, want %q", info.Version, gateway.ServerVersion)
	}
}

// TestToolVisibilityByPermission is the core claim: the tool list is a pure
// function of the principal's RBAC grant.
func TestToolVisibilityByPermission(t *testing.T) {
	tests := []struct {
		token string
		want  []string
	}{
		// owner short-circuits HasPermission -> everything.
		{"tok_owner_siam", []string{"create_invoice", "get_invoice_by_id", "list_invoices"}},
		// exact "invoice:read" -> the two read tools only.
		{"tok_reader_siam", []string{"get_invoice_by_id", "list_invoices"}},
		// "invoice:*" wildcard -> read and write, without owner.
		{"tok_bookkeeper_siam", []string{"create_invoice", "get_invoice_by_id", "list_invoices"}},
		// only "report:read" -> nothing in this catalog.
		{"tok_nogrants_siam", []string{}},
	}

	for _, tc := range tests {
		t.Run(tc.token, func(t *testing.T) {
			got := toolNames(t, connect(t, mustPrincipal(t, tc.token)))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeniedToolIsNotCallable checks that hiding a tool is not the only
// defense: a client that calls it anyway is refused.
func TestDeniedToolIsNotCallable(t *testing.T) {
	cs := connect(t, mustPrincipal(t, "tok_reader_siam"))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "create_invoice",
		Arguments: map[string]any{"customerName": "Nope Co.", "subtotal": 100.0},
	})
	// The SDK reports an unregistered tool as a protocol error; the
	// middleware reports a denied-but-registered one as IsError. Either is a
	// refusal — what must never happen is a successful create.
	if err != nil {
		if !strings.Contains(err.Error(), "create_invoice") {
			t.Errorf("unexpected error: %v", err)
		}
		return
	}
	if !res.IsError {
		t.Fatal("denied tool call succeeded")
	}
}

// TestMiddlewareDeniesEvenWhenRegistered isolates enforcement layer 2 by
// building a server for a permitted principal and then invoking the
// middleware with a downgraded one — the shape of a mid-session permission
// revocation.
func TestMiddlewareDeniesEvenWhenRegistered(t *testing.T) {
	downgraded := &rbac.Principal{
		UserID:  "usr_bbbb2222",
		OrgID:   mockdata.OrgSiamTrading,
		Role:    "member",
		Actions: []string{"invoice:read"},
	}

	var reached bool
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		reached = true
		return &mcp.CallToolResult{}, nil
	}

	h := gateway.EnforcePermissions(downgraded, nil)(next)
	res, err := h(context.Background(), "tools/call", &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "create_invoice"},
	})
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	if reached {
		t.Error("handler was reached despite missing permission")
	}
	ctr, ok := res.(*mcp.CallToolResult)
	if !ok || !ctr.IsError {
		t.Fatalf("want IsError result, got %#v", res)
	}
	text := ctr.Content[0].(*mcp.TextContent).Text
	if text != "Missing permission: invoice:write" {
		t.Errorf("message = %q, want controlplane's 403 wording", text)
	}
}

// TestTenantIsolation is the multi-tenancy claim: two principals with
// identical permissions see identical tools and disjoint data, and neither
// can reach the other's invoice by id.
func TestTenantIsolation(t *testing.T) {
	siam := connect(t, mustPrincipal(t, "tok_reader_siam"))
	bkl := connect(t, mustPrincipal(t, "tok_reader_bkl"))

	if a, b := toolNames(t, siam), toolNames(t, bkl); strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("tool surfaces differ: %v vs %v", a, b)
	}

	siamIDs := listInvoiceIDs(t, siam)
	bklIDs := listInvoiceIDs(t, bkl)
	if len(siamIDs) == 0 || len(bklIDs) == 0 {
		t.Fatalf("expected data for both orgs, got %v and %v", siamIDs, bklIDs)
	}
	for _, a := range siamIDs {
		for _, b := range bklIDs {
			if a == b {
				t.Fatalf("invoice %s visible to both orgs", a)
			}
		}
	}

	// Cross-tenant read by guessed id must fail even though the id is valid
	// in the other org.
	res, err := bkl.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_invoice_by_id",
		Arguments: map[string]any{"invoiceId": siamIDs[0]},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("org B read org A's invoice %s", siamIDs[0])
	}
}

func listInvoiceIDs(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_invoices"})
	if err != nil {
		t.Fatalf("list_invoices: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_invoices errored: %v", res.Content)
	}

	var out struct {
		Invoices []mockdata.Invoice `json:"invoices"`
	}
	// StructuredContent is the typed output the SDK derives from the
	// handler's Out type; Content carries the text fallback for clients
	// that ignore output schemas.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	ids := make([]string, 0, len(out.Invoices))
	for _, inv := range out.Invoices {
		ids = append(ids, inv.ID)
	}
	return ids
}

// TestWritePathForWildcardGrant exercises a successful call through the
// wildcard branch of HasPermission.
func TestWritePathForWildcardGrant(t *testing.T) {
	cs := connect(t, mustPrincipal(t, "tok_bookkeeper_siam"))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_invoice",
		Arguments: map[string]any{
			"customerName":  "Wildcard Test Co., Ltd.",
			"customerTaxId": "0105566000001",
			"subtotal":      1000.0,
			"dueDate":       "2026-09-01",
		},
	})
	if err != nil {
		t.Fatalf("create_invoice: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_invoice refused for invoice:* holder: %v", res.Content)
	}

	var inv mockdata.Invoice
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if inv.OrganizationID != mockdata.OrgSiamTrading {
		t.Errorf("created in org %q, want %q", inv.OrganizationID, mockdata.OrgSiamTrading)
	}
	if inv.Total != 1070 { // 1000 + 7% VAT
		t.Errorf("total = %v, want 1070", inv.Total)
	}
}

// ---------------------------------------------------------------------------
// Streamable HTTP transport — the shape the real gateway will ship.
// ---------------------------------------------------------------------------

// newHTTPServer mirrors cmd/httpsrv's wiring closely enough to prove the
// transport, without importing package main.
func newHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()

	type ctxKey struct{}

	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		p, _ := r.Context().Value(ctxKey{}).(*rbac.Principal)
		if p == nil {
			p = &rbac.Principal{}
		}
		return gateway.BuildServer(p, nil)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, err := principal.Resolve(tok)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="junctera"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})

	srv := httptest.NewServer(authed)
	t.Cleanup(srv.Close)
	return srv
}

// bearerRoundTripper attaches the token the way a real MCP client's
// configured auth header would.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(clone)
}

func TestStreamableHTTPPerRequestAuth(t *testing.T) {
	srv := newHTTPServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Same URL, different credentials, different tool surfaces — the thing
	// stdio structurally cannot do.
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"tok_owner_siam", 3},
		{"tok_reader_siam", 2},
		{"tok_nogrants_siam", 0},
	} {
		t.Run(tc.token, func(t *testing.T) {
			client := mcp.NewClient(&mcp.Implementation{Name: "spike-http-test", Version: "0.1.0"}, nil)
			cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:   srv.URL,
				HTTPClient: &http.Client{Transport: &bearerRoundTripper{token: tc.token, base: http.DefaultTransport}},
			}, nil)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer cs.Close()

			res, err := cs.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			if len(res.Tools) != tc.want {
				t.Errorf("got %d tools, want %d", len(res.Tools), tc.want)
			}
		})
	}
}

func TestStreamableHTTPRejectsBadToken(t *testing.T) {
	srv := newHTTPServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "spike-http-test", Version: "0.1.0"}, nil)
	_, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Transport: &bearerRoundTripper{token: "tok_bogus", base: http.DefaultTransport}},
	}, nil)
	if err == nil {
		t.Fatal("connect succeeded with an invalid token")
	}
}
