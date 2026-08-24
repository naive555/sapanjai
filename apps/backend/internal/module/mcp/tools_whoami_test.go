package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/rbac"
)

// connectWhoamiOnly registers only sapanjai_whoami on a bare server and
// drives a real MCP handshake over an in-memory transport pair, the same
// technique service_test.go's connect uses. Deliberately built without
// BuildServer: that function's per-entry permission gate and Service.enforce
// are both proven separately (TestBuildServer_*, TestEnforce_* in
// service_test.go); this helper isolates sapanjai_whoami's own output
// rendering from both enforcement layers, including the "no actions at all"
// case a real request could never reach past BuildServer's gate.
func connectWhoamiOnly(t *testing.T, p *rbac.Principal, req RequestInfo) *gomcp.ClientSession {
	t.Helper()

	srv := gomcp.NewServer(&gomcp.Implementation{Name: "whoami-test", Version: "0.1.0"}, nil)
	registerWhoami(srv, nil, p, db.Connector{}, req)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	serverT, clientT := gomcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := gomcp.NewClient(&gomcp.Implementation{Name: "whoami-test-client", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

// whoamiWire mirrors whoamiOutput's wire shape, with Permissions left as
// json.RawMessage so a test can tell a JSON `[]` apart from `null` — both
// unmarshal to a nil []string, which would hide exactly the bug these tests
// exist to catch.
type whoamiWire struct {
	OrganizationID string          `json:"organizationId"`
	KeyName        string          `json:"keyName"`
	Permissions    json.RawMessage `json:"permissions"`
}

func callWhoami(t *testing.T, p *rbac.Principal, req RequestInfo) whoamiWire {
	t.Helper()

	cs := connectWhoamiOnly(t, p, req)
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_whoami"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("sapanjai_whoami errored: %v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out whoamiWire
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestWhoami_NarrowedPrincipalListsExactlyItsActions(t *testing.T) {
	orgID := uuid.New()
	p := &rbac.Principal{
		OrganizationID: orgID,
		Actions:        []string{"connector:read", "mcpkey:read"},
	}

	out := callWhoami(t, p, RequestInfo{})

	if out.OrganizationID != orgID.String() {
		t.Errorf("organizationId = %q, want %q", out.OrganizationID, orgID.String())
	}

	var perms []string
	if err := json.Unmarshal(out.Permissions, &perms); err != nil {
		t.Fatalf("unmarshal permissions: %v", err)
	}
	want := []string{"connector:read", "mcpkey:read"}
	if len(perms) != len(want) {
		t.Fatalf("permissions = %v, want %v", perms, want)
	}
	for i, w := range want {
		if perms[i] != w {
			t.Errorf("permissions[%d] = %q, want %q", i, perms[i], w)
		}
	}
}

func TestWhoami_OwnerPrincipalReportsWildcard(t *testing.T) {
	p := &rbac.Principal{
		OrganizationID: uuid.New(),
		Role:           "owner",
		// Actions deliberately empty: the owner bypass, not a literal "*"
		// grant row, is what makes this render ["*"].
	}

	out := callWhoami(t, p, RequestInfo{})

	var perms []string
	if err := json.Unmarshal(out.Permissions, &perms); err != nil {
		t.Fatalf("unmarshal permissions: %v", err)
	}
	if len(perms) != 1 || perms[0] != "*" {
		t.Errorf("permissions = %v, want [\"*\"] for an owner principal", perms)
	}
}

func TestWhoami_NoActionsReportsEmptyArrayNeverNull(t *testing.T) {
	p := &rbac.Principal{OrganizationID: uuid.New()} // Role == "", Actions == nil

	out := callWhoami(t, p, RequestInfo{})

	if string(out.Permissions) != "[]" {
		t.Errorf("permissions wire value = %s, want the literal JSON []", out.Permissions)
	}
}

func TestWhoami_KeyNameComesFromRequestInfo(t *testing.T) {
	p := &rbac.Principal{OrganizationID: uuid.New(), Role: "owner"}

	out := callWhoami(t, p, RequestInfo{KeyName: "laptop-key"})

	if out.KeyName != "laptop-key" {
		t.Errorf("keyName = %q, want %q", out.KeyName, "laptop-key")
	}
}

func TestWhoami_EmptyKeyNameWhenRequestInfoOmitsIt(t *testing.T) {
	p := &rbac.Principal{OrganizationID: uuid.New(), Role: "owner"}

	out := callWhoami(t, p, RequestInfo{})

	if out.KeyName != "" {
		t.Errorf("keyName = %q, want empty string when RequestInfo carries none", out.KeyName)
	}
}

func TestWhoami_OutputHasNoOtherFields(t *testing.T) {
	p := &rbac.Principal{OrganizationID: uuid.New(), Role: "owner"}

	cs := connectWhoamiOnly(t, p, RequestInfo{KeyName: "laptop-key"})
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_whoami"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("sapanjai_whoami errored: %v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Structural check on the wire shape, not just this instance's field
	// values: no credential, no config, no key id or hash can ever leak
	// through a fourth field a future edit adds without this test noticing.
	if len(out) != 3 {
		t.Errorf("output has %d fields (%v), want exactly 3 (organizationId, keyName, permissions)", len(out), out)
	}
	for _, key := range []string{"organizationId", "keyName", "permissions"} {
		if _, ok := out[key]; !ok {
			t.Errorf("output missing field %q: %v", key, out)
		}
	}
}
