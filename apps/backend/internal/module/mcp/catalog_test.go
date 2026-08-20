package mcp_test

import (
	"testing"

	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/mcp"
)

// TestCatalog_HasExactlyTheStepEightTools locks the catalog's contents so an
// accidental addition or removal fails loudly here rather than only
// surfacing as an unexplained tools/list count somewhere else. Step 3
// shipped sapanjai_describe_connector; step 6 added the two Google Sheets
// read tools; step 7 added sheets_query_rows; step 8 adds sheets_read_range.
func TestCatalog_HasExactlyTheStepEightTools(t *testing.T) {
	cat := mcp.Catalog()
	if len(cat) != 5 {
		t.Fatalf("Catalog() has %d entries, want exactly 5 for step 8", len(cat))
	}

	wantNames := []string{
		"sapanjai_describe_connector", "sheets_list_spreadsheets", "sheets_describe_spreadsheet", "sheets_query_rows", "sheets_read_range",
	}
	for i, want := range wantNames {
		if cat[i].Name != want {
			t.Errorf("Catalog()[%d].Name = %q, want %q", i, cat[i].Name, want)
		}
	}

	if cat[0].Permission != connector.PermissionRead {
		t.Errorf("sapanjai_describe_connector permission = %q, want connector.PermissionRead (%q)", cat[0].Permission, connector.PermissionRead)
	}
	if cat[0].ConnectorType != "" {
		t.Errorf("sapanjai_describe_connector ConnectorType = %q, want empty (every connector type)", cat[0].ConnectorType)
	}

	for _, e := range cat[1:] {
		if e.Permission != mcp.PermissionSheetsRead {
			t.Errorf("%s permission = %q, want mcp.PermissionSheetsRead (%q)", e.Name, e.Permission, mcp.PermissionSheetsRead)
		}
		if string(e.ConnectorType) != "google_sheets" {
			t.Errorf("%s ConnectorType = %q, want %q", e.Name, e.ConnectorType, "google_sheets")
		}
	}
}

func TestPermissionFor(t *testing.T) {
	action, known := mcp.PermissionFor("sapanjai_describe_connector")
	if !known {
		t.Fatal("sapanjai_describe_connector should be a known catalog tool")
	}
	if action != connector.PermissionRead {
		t.Errorf("action = %q, want %q", action, connector.PermissionRead)
	}

	action, known = mcp.PermissionFor("sheets_list_spreadsheets")
	if !known || action != mcp.PermissionSheetsRead {
		t.Errorf("sheets_list_spreadsheets: known=%v action=%q, want known=true action=%q", known, action, mcp.PermissionSheetsRead)
	}

	action, known = mcp.PermissionFor("sheets_describe_spreadsheet")
	if !known || action != mcp.PermissionSheetsRead {
		t.Errorf("sheets_describe_spreadsheet: known=%v action=%q, want known=true action=%q", known, action, mcp.PermissionSheetsRead)
	}

	action, known = mcp.PermissionFor("sheets_query_rows")
	if !known || action != mcp.PermissionSheetsRead {
		t.Errorf("sheets_query_rows: known=%v action=%q, want known=true action=%q", known, action, mcp.PermissionSheetsRead)
	}

	action, known = mcp.PermissionFor("sheets_read_range")
	if !known || action != mcp.PermissionSheetsRead {
		t.Errorf("sheets_read_range: known=%v action=%q, want known=true action=%q", known, action, mcp.PermissionSheetsRead)
	}

	if _, known := mcp.PermissionFor("not_a_real_tool"); known {
		t.Error("an unregistered tool name must report known = false")
	}
}
