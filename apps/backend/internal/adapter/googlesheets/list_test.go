package googlesheets

import (
	"context"
	"errors"
	"testing"
)

func TestListSpreadsheets_ReturnsOneSummaryPerAllowlistedID(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-a", "sheet-b"}}}
	var gotIDs []string
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			gotIDs = append(gotIDs, spreadsheetID)
			return &Meta{SpreadsheetID: spreadsheetID, Title: "Title-" + spreadsheetID}, nil
		},
	}

	out, err := listSpreadsheets(context.Background(), api, cfg)
	if err != nil {
		t.Fatalf("listSpreadsheets: %v", err)
	}
	if len(gotIDs) != 2 || gotIDs[0] != "sheet-a" || gotIDs[1] != "sheet-b" {
		t.Fatalf("SpreadsheetMeta called with %v, want [sheet-a sheet-b] in order", gotIDs)
	}
	if len(out.Spreadsheets) != 2 {
		t.Fatalf("Spreadsheets = %d entries, want 2", len(out.Spreadsheets))
	}
	if out.Spreadsheets[0].SpreadsheetID != "sheet-a" || out.Spreadsheets[0].Title != "Title-sheet-a" || !out.Spreadsheets[0].Accessible {
		t.Errorf("Spreadsheets[0] = %+v, want an accessible sheet-a with its title", out.Spreadsheets[0])
	}
	if out.Spreadsheets[1].SpreadsheetID != "sheet-b" || out.Spreadsheets[1].Title != "Title-sheet-b" || !out.Spreadsheets[1].Accessible {
		t.Errorf("Spreadsheets[1] = %+v, want an accessible sheet-b with its title", out.Spreadsheets[1])
	}
}

// TestListSpreadsheets_NeverConsultsFolders proves the tool sticks to
// docs/06-sheets-adapter.md §4's split: sheets_list_spreadsheets lists only
// config.scope.spreadsheet_ids, never drive_folder_ids (that's
// drive_list_folder, step 9, out of scope here). A ListFiles call would
// fail this test since mockSheetsAPI has no listFiles func configured.
func TestListSpreadsheets_NeverConsultsFolders(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{
		SpreadsheetIDs: []string{"sheet-a"},
		DriveFolderIDs: []string{"folder-a"},
	}}
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID}, nil
		},
	}

	out, err := listSpreadsheets(context.Background(), api, cfg)
	if err != nil {
		t.Fatalf("listSpreadsheets: %v", err)
	}
	if len(out.Spreadsheets) != 1 {
		t.Fatalf("Spreadsheets = %d entries, want exactly 1 (folders must never appear)", len(out.Spreadsheets))
	}
}

// TestListSpreadsheets_PerItemFailureDoesNotAbortTheWholeCall is the test
// that would catch a regression to fail-fast behavior: one allowlisted id
// whose share was revoked must not hide every other spreadsheet the
// connector can still reach.
func TestListSpreadsheets_PerItemFailureDoesNotAbortTheWholeCall(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-ok", "sheet-revoked"}}}
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			if spreadsheetID == "sheet-revoked" {
				return nil, errors.New("googleapi: Error 403: The caller does not have permission")
			}
			return &Meta{SpreadsheetID: spreadsheetID, Title: "OK"}, nil
		},
	}

	out, err := listSpreadsheets(context.Background(), api, cfg)
	if err != nil {
		t.Fatalf("listSpreadsheets: %v, want no error — per-item failures must not abort the call", err)
	}
	if len(out.Spreadsheets) != 2 {
		t.Fatalf("Spreadsheets = %d entries, want 2 (one accessible, one not)", len(out.Spreadsheets))
	}
	ok, revoked := out.Spreadsheets[0], out.Spreadsheets[1]
	if !ok.Accessible || ok.Title != "OK" {
		t.Errorf("Spreadsheets[0] = %+v, want accessible sheet-ok", ok)
	}
	if revoked.Accessible || revoked.Title != "" || revoked.SpreadsheetID != "sheet-revoked" {
		t.Errorf("Spreadsheets[1] = %+v, want inaccessible sheet-revoked with no title", revoked)
	}
}

func TestListSpreadsheets_EmptyAllowlistReturnsEmptyList(t *testing.T) {
	cfg := &Config{}
	api := &mockSheetsAPI{t: t}

	out, err := listSpreadsheets(context.Background(), api, cfg)
	if err != nil {
		t.Fatalf("listSpreadsheets: %v", err)
	}
	if len(out.Spreadsheets) != 0 {
		t.Fatalf("Spreadsheets = %v, want empty", out.Spreadsheets)
	}
}
