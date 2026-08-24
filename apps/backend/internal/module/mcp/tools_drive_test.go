package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/module/mcp"
	"github.com/sapanjai/backend/internal/module/rbac"
)

// validGoogleSheetsConfigWithFolder mirrors tools_sheets_test.go's
// validGoogleSheetsConfig, additionally allowlisting one Drive folder id —
// every test in this file that uses it must never reach the network, same
// as every sheets tool unit test.
func validGoogleSheetsConfigWithFolder() map[string]any {
	return map[string]any{
		"oauth": map[string]any{
			"refresh_token": "1//0g-fake-refresh-token",
			"client_id":     "fake.apps.googleusercontent.com",
			"client_secret": "fake-client-secret",
		},
		"scope": map[string]any{
			"spreadsheet_ids":  []any{"1AbCAllowedSpreadsheetId"},
			"drive_folder_ids": []any{"0B1aAllowedFolderId"},
		},
	}
}

// ---- connector-type + permission gating (BuildServer) ----

func TestBuildServer_DriveToolsHiddenForNonGoogleSheetsConnector(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfigWithFolder()}, nil, nil, nil, nil)
	conn := testConnector() // Type: "generic"

	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))
	got := toolNames(t, cs)
	// connector:read gates both connector-agnostic tools — sapanjai_describe_connector
	// and sapanjai_whoami — neither of which is connector-type-restricted, so
	// both remain visible for a generic connector even with owner bypass;
	// no drive_* tool must join them.
	want := map[string]bool{"sapanjai_describe_connector": true, "sapanjai_whoami": true}
	if len(got) != 2 {
		t.Fatalf("tools/list = %v, want only the 2 connector-agnostic tools for a generic connector even with owner bypass", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected tool %q in tools/list", name)
		}
	}
}

func TestBuildServer_DriveToolsVisibleForGoogleSheetsConnectorWithDriveRead(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfigWithFolder()}, nil, nil, nil, nil)
	conn := testGoogleSheetsConnector()

	cs := connect(t, svc.BuildServer(&rbac.Principal{Actions: []string{mcp.PermissionDriveRead}}, conn, mcp.RequestInfo{}))
	got := toolNames(t, cs)
	want := map[string]bool{"drive_list_folder": true, "drive_get_file": true}
	if len(got) != 2 {
		t.Fatalf("tools/list = %v, want exactly the 2 drive tools", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected tool %q in tools/list", name)
		}
	}
}

// TestBuildServer_SheetsReadDoesNotGrantDriveTools is the plan's explicit
// requirement: sheets:read must never also unlock drive_* tools. The
// principal here has sheets:read (and so sees the 4 sheets tools) but not
// drive:read, and must see neither drive_list_folder nor drive_get_file.
func TestBuildServer_SheetsReadDoesNotGrantDriveTools(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfigWithFolder()}, nil, nil, nil, nil)
	conn := testGoogleSheetsConnector()

	cs := connect(t, svc.BuildServer(&rbac.Principal{Actions: []string{mcp.PermissionSheetsRead}}, conn, mcp.RequestInfo{}))
	got := toolNames(t, cs)
	for _, name := range got {
		if name == "drive_list_folder" || name == "drive_get_file" {
			t.Errorf("tools/list = %v, sheets:read must not grant a drive_* tool", got)
		}
	}

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "drive_list_folder", Arguments: map[string]any{"folder_id": "x"}})
	if err != nil {
		return // unregistered-tool protocol error is a refusal too
	}
	if !res.IsError {
		t.Fatal("drive_list_folder succeeded for a principal with sheets:read but not drive:read")
	}
}

// TestBuildServer_DriveReadDoesNotGrantSheetsTools is the mirror image: a
// principal with only drive:read must not see any sheets_* tool.
func TestBuildServer_DriveReadDoesNotGrantSheetsTools(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfigWithFolder()}, nil, nil, nil, nil)
	conn := testGoogleSheetsConnector()

	cs := connect(t, svc.BuildServer(&rbac.Principal{Actions: []string{mcp.PermissionDriveRead}}, conn, mcp.RequestInfo{}))
	got := toolNames(t, cs)
	for _, name := range got {
		if name == "sheets_list_spreadsheets" || name == "sheets_describe_spreadsheet" ||
			name == "sheets_query_rows" || name == "sheets_read_range" {
			t.Errorf("tools/list = %v, drive:read must not grant a sheets_* tool", got)
		}
	}
}

// ---- drive_list_folder: allowlist enforcement ----

// TestDriveListFolder_RejectsNonAllowlistedFolderID mirrors
// TestDescribeSpreadsheet_RejectsNonAllowlistedID: the allowlist check must
// reject a plausible-looking, non-allowlisted folder id before any network
// call — this suite's refresh token is fake and would fail loudly if the
// code path ever reached the network.
func TestDriveListFolder_RejectsNonAllowlistedFolderID(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfigWithFolder()}, nil, nil, nil, nil)
	conn := testGoogleSheetsConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	args, _ := json.Marshal(map[string]any{"folder_id": "0XyZNotOnTheAllowlist"})
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "drive_list_folder",
		Arguments: json.RawMessage(args),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for a folder id absent from the allowlist")
	}
	text, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", res.Content[0])
	}
	if !contains(text.Text, "FOLDER_NOT_ALLOWED") {
		t.Errorf("text = %q, want it to mention FOLDER_NOT_ALLOWED", text.Text)
	}
}

// TestDriveListFolder_MissingFolderID proves folder_id is required and
// checked before any config decryption or network call.
func TestDriveListFolder_MissingFolderID(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfigWithFolder()}, nil, nil, nil, nil)
	conn := testGoogleSheetsConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "drive_list_folder"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("missing folder_id: expected IsError, got success")
	}
}

// ---- drive_get_file: missing input ----

// TestDriveGetFile_MissingFileID proves file_id is required and checked
// before any config decryption or network call — drive_get_file has no
// allowlist pre-check to piggyback this on (see googlesheets.GetFile's doc
// comment), so this is the only thing standing between a malformed call and
// a network attempt this suite must never make.
func TestDriveGetFile_MissingFileID(t *testing.T) {
	svc := mcp.NewService(&fakeConfigGetter{cfg: validGoogleSheetsConfigWithFolder()}, nil, nil, nil, nil)
	conn := testGoogleSheetsConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "drive_get_file"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("missing file_id: expected IsError, got success")
	}
}

// Note on coverage this file deliberately stops short of: unlike every
// sheets_* tool and drive_list_folder, drive_get_file has no allowlist (or
// any other) pre-check between "file_id is non-empty" and calling
// googlesheets.GetFile — by design, since there is nothing to check against
// the allowlist until GetFile has fetched the file's own parent folders
// (see googlesheets.GetFile's doc comment). That means every other behavior
// this tool has — FILE_NOT_ALLOWED, FILE_NOT_FOUND, the Google-native-mime
// no-download-link path, and link minting itself — sits on the far side of
// a real network call this package's unit tests must never make. Those are
// covered instead at the adapter level, against a mocked sheetsAPI, in
// internal/adapter/googlesheets/drive_test.go (TestDrive_GetFile_*), plus
// filelink_test.go for signing/verification in isolation.
