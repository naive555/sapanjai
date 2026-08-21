package googlesheets

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sapanjai/backend/internal/module/connector"
)

// mockSheetsAPI implements sheetsAPI for tests that must never touch the
// network. Only the methods a given test needs are set; an unset method
// fails the test if called.
type mockSheetsAPI struct {
	spreadsheetMeta func(ctx context.Context, spreadsheetID string) (*Meta, error)
	values          func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error)
	listFiles       func(ctx context.Context, folderID string, pageToken string) (*FilePage, error)
	file            func(ctx context.Context, fileID string) (*File, error)
	downloadFile    func(ctx context.Context, fileID string) (io.ReadCloser, string, error)
	t               *testing.T
}

var _ sheetsAPI = (*mockSheetsAPI)(nil)

func (m *mockSheetsAPI) SpreadsheetMeta(ctx context.Context, spreadsheetID string) (*Meta, error) {
	if m.spreadsheetMeta == nil {
		m.t.Fatal("unexpected call to SpreadsheetMeta")
	}
	return m.spreadsheetMeta(ctx, spreadsheetID)
}

func (m *mockSheetsAPI) Values(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
	if m.values == nil {
		m.t.Fatal("unexpected call to Values")
	}
	return m.values(ctx, spreadsheetID, a1Range)
}

func (m *mockSheetsAPI) ListFiles(ctx context.Context, folderID string, pageToken string) (*FilePage, error) {
	if m.listFiles == nil {
		m.t.Fatal("unexpected call to ListFiles")
	}
	return m.listFiles(ctx, folderID, pageToken)
}

func (m *mockSheetsAPI) File(ctx context.Context, fileID string) (*File, error) {
	if m.file == nil {
		m.t.Fatal("unexpected call to File")
	}
	return m.file(ctx, fileID)
}

func (m *mockSheetsAPI) DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, string, error) {
	if m.downloadFile == nil {
		m.t.Fatal("unexpected call to DownloadFile")
	}
	return m.downloadFile(ctx, fileID)
}

func TestChecker_Type(t *testing.T) {
	c := NewChecker()
	if got := c.Type(); got != connector.TypeGoogleSheets {
		t.Errorf("Type() = %q, want %q", got, connector.TypeGoogleSheets)
	}
}

// TestChecker_Check_InvalidConfig proves a malformed config is rejected
// before any client is built — no network is reachable from this unit test
// suite, so a bug that skipped this early return would hang or fail this
// test rather than silently pass.
func TestChecker_Check_InvalidConfig(t *testing.T) {
	c := NewChecker()
	err := c.Check(context.Background(), map[string]any{"oauth": map[string]any{}})
	if err == nil {
		t.Fatal("expected an error for a config missing scope entirely")
	}
}

func TestProbe_UsesSpreadsheetMetaWhenSpreadsheetsAllowlisted(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-a"}}}
	called := false
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			called = true
			if spreadsheetID != "sheet-a" {
				t.Errorf("spreadsheetID = %q, want %q", spreadsheetID, "sheet-a")
			}
			return &Meta{SpreadsheetID: spreadsheetID}, nil
		},
	}

	if err := probe(context.Background(), api, cfg); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !called {
		t.Fatal("expected SpreadsheetMeta to be called")
	}
}

func TestProbe_FallsBackToListFilesWhenNoSpreadsheetsAllowlisted(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{DriveFolderIDs: []string{"folder-a"}}}
	called := false
	api := &mockSheetsAPI{
		t: t,
		listFiles: func(ctx context.Context, folderID string, pageToken string) (*FilePage, error) {
			called = true
			if folderID != "folder-a" {
				t.Errorf("folderID = %q, want %q", folderID, "folder-a")
			}
			return &FilePage{}, nil
		},
	}

	if err := probe(context.Background(), api, cfg); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !called {
		t.Fatal("expected ListFiles to be called")
	}
}

func TestProbe_PropagatesUpstreamError(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-a"}}}
	upstreamErr := errors.New("googleapi: Error 401: invalid_grant")
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return nil, upstreamErr
		},
	}

	err := probe(context.Background(), api, cfg)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("probe error does not wrap the upstream error: %v", err)
	}
}
