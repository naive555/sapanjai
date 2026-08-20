package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/mcp"
	"github.com/sapanjai/backend/internal/module/rbac"
)

// validGoogleSheetsConfig is a config map shaped exactly like
// googlesheets.ParseConfig expects, allowlisting exactly one spreadsheet id
// that is deliberately plausible-looking (a real Google id shape) but never
// actually reachable — every test in this file that uses it must never
// reach the network, since these are unit tests with no Google credentials
// and no egress.
func validGoogleSheetsConfig() map[string]any {
	return map[string]any{
		"oauth": map[string]any{
			"refresh_token": "1//0g-fake-refresh-token",
			"client_id":     "fake.apps.googleusercontent.com",
			"client_secret": "fake-client-secret",
		},
		"scope": map[string]any{
			"spreadsheet_ids": []any{"1AbCAllowedSpreadsheetId"},
		},
	}
}

func testGoogleSheetsConnector() db.Connector {
	return db.Connector{
		ID:              uuid.New(),
		OrganizationID:  uuid.New(),
		Name:            "sheets-connector",
		Type:            "google_sheets",
		Status:          "active",
		EncryptedConfig: json.RawMessage(`{}`), // opaque to the fake getter below; never actually decrypted
	}
}

// fakeConfigGetter is a connectorGetter whose OpenConfig always returns cfg,
// regardless of the (organizationID, encryptedConfig) it's called with —
// enough to drive the sheets tool handlers without a real envelope-sealed
// row.
type fakeConfigGetter struct {
	cfg map[string]any
}

func (f *fakeConfigGetter) Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	return db.Connector{}, nil
}

func (f *fakeConfigGetter) OpenConfig(ctx context.Context, organizationID uuid.UUID, encryptedConfig json.RawMessage) (map[string]any, error) {
	return f.cfg, nil
}

// ---- connector-type gating (BuildServer's Entry.appliesTo) ----

func TestBuildServer_SheetsToolsHiddenForNonGoogleSheetsConnector(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfig()}, nil, nil, nil)
	conn := testConnector() // Type: "generic"

	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn))
	got := toolNames(t, cs)
	if len(got) != 1 || got[0] != "sapanjai_describe_connector" {
		t.Fatalf("tools/list = %v, want only sapanjai_describe_connector for a generic connector even with owner bypass", got)
	}
}

func TestBuildServer_SheetsToolsVisibleForGoogleSheetsConnectorWithPermission(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfig()}, nil, nil, nil)
	conn := testGoogleSheetsConnector()

	cs := connect(t, svc.BuildServer(&rbac.Principal{Actions: []string{mcp.PermissionSheetsRead}}, conn))
	got := toolNames(t, cs)
	want := map[string]bool{"sheets_list_spreadsheets": true, "sheets_describe_spreadsheet": true, "sheets_query_rows": true}
	if len(got) != 3 {
		t.Fatalf("tools/list = %v, want exactly the 3 sheets tools", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected tool %q in tools/list", name)
		}
	}
}

// TestBuildServer_SheetsToolsInvisibleWithoutSheetsRead is the plan's
// required test: a principal with some unrelated grant (or none) must never
// see sheets_list_spreadsheets / sheets_describe_spreadsheet / sheets_query_rows,
// even against a google_sheets connector.
func TestBuildServer_SheetsToolsInvisibleWithoutSheetsRead(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfig()}, nil, nil, nil)
	conn := testGoogleSheetsConnector()

	cases := []struct {
		name           string
		p              *rbac.Principal
		wantOtherTools int // tools unrelated to sheets:read that this principal is still entitled to see
	}{
		{"no grant at all", &rbac.Principal{}, 0},
		// connector:read grants sapanjai_describe_connector (Permission
		// connector:read, ConnectorType "" so it applies to every
		// connector) but must never also unlock the sheets:read-gated
		// tools — a different action must not leak into a different
		// permission's tools.
		{"unrelated grant", &rbac.Principal{Actions: []string{"connector:read"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := connect(t, svc.BuildServer(tc.p, conn))
			got := toolNames(t, cs)
			if len(got) != tc.wantOtherTools {
				t.Errorf("tools/list = %v, want %d tool(s) unrelated to sheets:read", got, tc.wantOtherTools)
			}
			for _, name := range got {
				if name == "sheets_list_spreadsheets" || name == "sheets_describe_spreadsheet" || name == "sheets_query_rows" {
					t.Errorf("tools/list = %v, must not include a sheets:read-gated tool without the grant", got)
				}
			}

			res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sheets_describe_spreadsheet"})
			if err != nil {
				// SDK reports an unregistered tool as a protocol error — a
				// refusal either way.
				return
			}
			if !res.IsError {
				t.Fatal("sheets_describe_spreadsheet call succeeded without sheets:read")
			}
		})
	}
}

// ---- sheets_describe_spreadsheet: allowlist enforcement ----

// TestDescribeSpreadsheet_RejectsNonAllowlistedID is the plan's required
// test: an id the OAuth token could plausibly reach (real Google id shape)
// but that is absent from the connector's own allowlist must be rejected by
// the allowlist alone, before any network call — the fake refresh token in
// validGoogleSheetsConfig would fail immediately if this test's code path
// ever reached the network.
func TestDescribeSpreadsheet_RejectsNonAllowlistedID(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfig()}, nil, nil, nil)
	conn := testGoogleSheetsConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn))

	args, _ := json.Marshal(map[string]any{"spreadsheet_id": "1XyZNotOnTheAllowlist"})
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "sheets_describe_spreadsheet",
		Arguments: json.RawMessage(args),
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
	if got := text.Text; got == "" || !contains(got, "SPREADSHEET_NOT_ALLOWED") {
		t.Errorf("text = %q, want it to mention SPREADSHEET_NOT_ALLOWED", got)
	}
}

// ---- sheets_describe_spreadsheet: include_sample_rows bound ----

// TestDescribeSpreadsheet_IncludeSampleRowsBounded is the plan's required
// test: include_sample_rows must be rejected outside 0-5, and — critically
// — this check must fire before any config decryption or network call, so
// it uses a spreadsheet_id that *is* allowlisted (proving the rejection is
// about the bound, not the allowlist) yet still never reaches the network.
func TestDescribeSpreadsheet_IncludeSampleRowsBounded(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfig()}, nil, nil, nil)
	conn := testGoogleSheetsConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn))

	for _, n := range []int{-1, 6, 100} {
		args, _ := json.Marshal(map[string]any{
			"spreadsheet_id":      "1AbCAllowedSpreadsheetId",
			"include_sample_rows": n,
		})
		res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
			Name:      "sheets_describe_spreadsheet",
			Arguments: json.RawMessage(args),
		})
		if err != nil {
			t.Fatalf("include_sample_rows=%d: call: %v", n, err)
		}
		if !res.IsError {
			t.Fatalf("include_sample_rows=%d: expected IsError, got a success result", n)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
