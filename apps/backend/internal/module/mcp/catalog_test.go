package mcp_test

import (
	"testing"

	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/mcp"
)

func TestCatalog_HasExactlyTheStepThreeTool(t *testing.T) {
	cat := mcp.Catalog()
	if len(cat) != 1 {
		t.Fatalf("Catalog() has %d entries, want exactly 1 for step 3", len(cat))
	}
	if cat[0].Name != "sapanjai_describe_connector" {
		t.Errorf("tool name = %q, want %q", cat[0].Name, "sapanjai_describe_connector")
	}
	if cat[0].Permission != connector.PermissionRead {
		t.Errorf("permission = %q, want connector.PermissionRead (%q)", cat[0].Permission, connector.PermissionRead)
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

	if _, known := mcp.PermissionFor("not_a_real_tool"); known {
		t.Error("an unregistered tool name must report known = false")
	}
}
