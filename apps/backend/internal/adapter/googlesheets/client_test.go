package googlesheets

import (
	"context"
	"encoding/json"
	"io"
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

// TestClient_DownloadFile_UsesDownloadClientNotRequestClient is a
// regression test for the bug a code reviewer caught after step 9's initial
// implementation: DownloadFile originally shared c.drive (and therefore
// requestTimeout, 15s) with every metadata call, so a body read slower than
// 15s — a large file over a slow connection, entirely normal — was silently
// truncated after Handler.downloadFile had already committed a 200 and
// headers. This test can't wait out a real 15s timeout (that would make the
// suite intolerably slow), but it does prove the *mechanism* of the fix:
// the response body streams to completion through DownloadFile's own
// client/service (c.downloadDrive), separately from c.drive, and the
// content is delivered correctly end to end — the same contract-test
// discipline TestClient_SpreadsheetMeta_ContractsWithGoogleAPI applies to
// the metadata path.
func TestClient_DownloadFile_UsesDownloadClientNotRequestClient(t *testing.T) {
	const wantBody = "raw file bytes, not a metadata response"
	const wantContentType = "text/csv"

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", wantContentType)
		_, _ = w.Write([]byte(wantBody))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-access-token", TokenType: "Bearer"})
	c, err := newClient(context.Background(), ts, srv.URL)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if c.downloadDrive == c.drive {
		t.Fatal("downloadDrive is the same *drive.Service as drive; a download would inherit requestTimeout")
	}
	// Nil out the metadata service before downloading: if DownloadFile ever
	// regresses to c.drive (the original bug), this panics instead of
	// quietly passing. Asserting the two services merely differ is not
	// enough — it would not catch a DownloadFile that still reached for the
	// 15s-timeout one.
	c.drive = nil

	body, contentType, err := c.DownloadFile(context.Background(), "file-123")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
	if contentType != wantContentType {
		t.Errorf("contentType = %q, want %q", contentType, wantContentType)
	}
	// Same authenticated-request assertion as the SpreadsheetMeta contract
	// test — c.downloadDrive must carry the same oauth2 transport as c.drive,
	// not just a different Timeout.
	if gotAuth != "Bearer fake-access-token" {
		t.Errorf("Authorization header upstream = %q, want %q", gotAuth, "Bearer fake-access-token")
	}
	if !strings.Contains(gotPath, "file-123") {
		t.Errorf("request path = %q, want it to contain the file id", gotPath)
	}
}
