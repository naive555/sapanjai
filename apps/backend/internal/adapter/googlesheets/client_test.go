package googlesheets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// TestClient_SpreadsheetMeta_ContractsWithGoogleAPI is the one place in
// this package's test suite a real Google response shape is asserted
// (docs/07-sheets-adapter-plan.md step 5). It drives the actual client —
// the real google.golang.org/api/sheets/v4 wrapper, not a mock — against an
// httptest server via option.WithEndpoint, proving client.SpreadsheetMeta's
// mapping from the Sheets API's JSON shape to our Meta type is correct.
// Every other test in this package mocks sheetsAPI and never touches the
// network.
func TestClient_SpreadsheetMeta_ContractsWithGoogleAPI(t *testing.T) {
	const wantID = "1AbCSpreadsheetID"

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"spreadsheetId": wantID,
			"properties":    map[string]any{"title": "ระบบสัญญา 2568"},
			"sheets": []map[string]any{
				{
					"properties": map[string]any{
						"title": "Contracts",
						"gridProperties": map[string]any{
							"rowCount":    12450,
							"columnCount": 8,
						},
					},
				},
				{
					"properties": map[string]any{
						"title": "Notes",
						// No gridProperties at all — must not panic.
					},
				},
			},
		})
	}))
	defer srv.Close()

	// A static token source: no refresh, no network call of its own — the
	// contract under test is the response mapping, not the OAuth flow.
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-access-token", TokenType: "Bearer"})

	c, err := newClient(context.Background(), ts, srv.URL)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	meta, err := c.SpreadsheetMeta(context.Background(), wantID)
	if err != nil {
		t.Fatalf("SpreadsheetMeta: %v", err)
	}

	// The token actually has to reach Google. google.golang.org/api returns
	// an option.WithHTTPClient client verbatim and never applies
	// option.WithTokenSource to it, so a client built the obvious-looking
	// way sends every request unauthenticated and 401s in production while
	// still passing a test that only checks response mapping.
	if gotAuth != "Bearer fake-access-token" {
		t.Errorf("Authorization header upstream = %q, want %q", gotAuth, "Bearer fake-access-token")
	}
	if !strings.Contains(gotPath, wantID) {
		t.Errorf("request path = %q, want it to contain the spreadsheet id %q", gotPath, wantID)
	}
	if meta.SpreadsheetID != wantID {
		t.Errorf("SpreadsheetID = %q, want %q", meta.SpreadsheetID, wantID)
	}
	if meta.Title != "ระบบสัญญา 2568" {
		t.Errorf("Title = %q, want %q", meta.Title, "ระบบสัญญา 2568")
	}
	if len(meta.Sheets) != 2 {
		t.Fatalf("len(Sheets) = %d, want 2", len(meta.Sheets))
	}
	if got := meta.Sheets[0]; got.Title != "Contracts" || got.RowCount != 12450 || got.ColumnCount != 8 {
		t.Errorf("Sheets[0] = %+v, want {Contracts 12450 8}", got)
	}
	if got := meta.Sheets[1]; got.Title != "Notes" || got.RowCount != 0 || got.ColumnCount != 0 {
		t.Errorf("Sheets[1] = %+v, want zero-valued dimensions for a sheet with no gridProperties", got)
	}
}
