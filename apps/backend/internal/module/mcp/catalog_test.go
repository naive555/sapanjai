package mcp_test

import (
	"testing"

	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/mcp"
)

// TestCatalog_HasExactlyTheStepNineTools locks the catalog's contents so an
// accidental addition or removal fails loudly here rather than only
// surfacing as an unexplained tools/list count somewhere else. Step 3
// shipped sapanjai_describe_connector; step 6 added the two Google Sheets
// read tools; step 7 added sheets_query_rows; step 8 added
// sheets_read_range; step 9 added drive_list_folder and drive_get_file;
// gateway-core step 3 adds sapanjai_whoami, directly after
// sapanjai_describe_connector so the two connector-agnostic tools stay
// grouped and tools/list order stays deterministic.
func TestCatalog_HasExactlyTheStepNineTools(t *testing.T) {
	cat := mcp.Catalog()
	if len(cat) != 8 {
		t.Fatalf("Catalog() has %d entries, want exactly 8", len(cat))
	}

	wantNames := []string{
		"sapanjai_describe_connector", "sapanjai_whoami", "sheets_list_spreadsheets", "sheets_describe_spreadsheet",
		"sheets_query_rows", "sheets_read_range", "drive_list_folder", "drive_get_file",
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

	// sapanjai_whoami is gated on the same connector:read action as
	// sapanjai_describe_connector — deliberately, so it is never invisible
	// to a key that can see any tool at all (docs/08-gateway-core.md §6 Q1).
	if cat[1].Permission != connector.PermissionRead {
		t.Errorf("sapanjai_whoami permission = %q, want connector.PermissionRead (%q)", cat[1].Permission, connector.PermissionRead)
	}
	if cat[1].ConnectorType != "" {
		t.Errorf("sapanjai_whoami ConnectorType = %q, want empty (every connector type)", cat[1].ConnectorType)
	}

	sheetsEntries := cat[2:6]
	for _, e := range sheetsEntries {
		if e.Permission != mcp.PermissionSheetsRead {
			t.Errorf("%s permission = %q, want mcp.PermissionSheetsRead (%q)", e.Name, e.Permission, mcp.PermissionSheetsRead)
		}
		if string(e.ConnectorType) != "google_sheets" {
			t.Errorf("%s ConnectorType = %q, want %q", e.Name, e.ConnectorType, "google_sheets")
		}
	}

	// drive_* tools are gated by drive:read specifically — never
	// sheets:read, per docs/06-sheets-adapter.md §5 and the plan's explicit
	// requirement that the two permissions stay independent.
	driveEntries := cat[6:8]
	for _, e := range driveEntries {
		if e.Permission != mcp.PermissionDriveRead {
			t.Errorf("%s permission = %q, want mcp.PermissionDriveRead (%q)", e.Name, e.Permission, mcp.PermissionDriveRead)
		}
		if e.Permission == mcp.PermissionSheetsRead {
			t.Errorf("%s permission must not be sheets:read", e.Name)
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

	action, known = mcp.PermissionFor("sapanjai_whoami")
	if !known || action != connector.PermissionRead {
		t.Errorf("sapanjai_whoami: known=%v action=%q, want known=true action=%q", known, action, connector.PermissionRead)
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

	action, known = mcp.PermissionFor("drive_list_folder")
	if !known || action != mcp.PermissionDriveRead {
		t.Errorf("drive_list_folder: known=%v action=%q, want known=true action=%q", known, action, mcp.PermissionDriveRead)
	}

	action, known = mcp.PermissionFor("drive_get_file")
	if !known || action != mcp.PermissionDriveRead {
		t.Errorf("drive_get_file: known=%v action=%q, want known=true action=%q", known, action, mcp.PermissionDriveRead)
	}

	if _, known := mcp.PermissionFor("not_a_real_tool"); known {
		t.Error("an unregistered tool name must report known = false")
	}
}
