package googlesheets

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// requestTimeout bounds every upstream Google API call *except* a file
// download's body read — see downloadTimeout's doc comment for why the two
// need different budgets. Passed via option.WithHTTPClient rather than left
// to the SDK's default (no timeout), per docs/07-sheets-adapter-plan.md
// step 5.
const requestTimeout = 15 * time.Second

// downloadTimeout backstops DownloadFile's body read against a connection
// that never progresses. The real bound on a legitimately slow download is
// the caller's context, which Echo cancels when the client hangs up.
//
// It needs its own client because http.Client.Timeout covers the entire
// round trip *including* the body read — fine for this package's other
// calls, which return small responses, but wrong for a download paced by
// whoever reads the other end. Confirmed experimentally: a short Timeout
// against a slow body fails partway through, by which point a 200 and
// headers are already written, so the caller silently gets a truncated file
// with no error surfaced at all.
const downloadTimeout = 10 * time.Minute

// client is the thin implementation of sheetsAPI over the official
// google.golang.org/api/{sheets,drive} SDKs — no hand-rolled HTTP.
// downloadDrive is a *second* Drive service, authorized by the same
// TokenSource but built from a client carrying downloadTimeout instead of
// requestTimeout — see downloadTimeout's doc comment. DownloadFile is the
// only method that uses it; every other method uses drive/sheets as before.
type client struct {
	sheets        *sheets.Service
	drive         *drive.Service
	downloadDrive *drive.Service
}

var _ sheetsAPI = (*client)(nil)

// newClient builds a client authorized by ts. endpoint overrides the SDKs'
// default base path when non-empty — used only by client_test.go's contract
// test, which points every service at an httptest server.
func newClient(ctx context.Context, ts oauth2.TokenSource, endpoint string) (*client, error) {
	// oauth2.NewClient wires ts into the client's transport. This must be
	// the *same* client that carries the timeout: google.golang.org/api's
	// transport returns a WithHTTPClient client verbatim and never applies
	// WithTokenSource to it (transport/http/dial.go), so passing a bare
	// &http.Client alongside WithTokenSource would send every request
	// unauthenticated. client_test.go asserts the Authorization header
	// upstream to keep that regression from coming back.
	httpClient := oauth2.NewClient(ctx, ts)
	httpClient.Timeout = requestTimeout

	opts := []option.ClientOption{
		option.WithHTTPClient(httpClient),
	}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}

	sheetsSvc, err := sheets.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: build sheets client: %w", err)
	}
	driveSvc, err := drive.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: build drive client: %w", err)
	}

	// A second *http.Client (same ts, same oauth2 transport, so downloads
	// are still authenticated) carrying downloadTimeout instead of
	// requestTimeout — one *drive.Service can only ever have one Timeout
	// (option.WithHTTPClient's client is applied to the whole Service
	// verbatim), so DownloadFile's different budget needs its own Service
	// rather than a per-call override. ts.Token() is cached
	// (oauth2.ReuseTokenSource, see oauth.go), so building a second client
	// from the same ts does not mean a second token exchange.
	downloadHTTPClient := oauth2.NewClient(ctx, ts)
	downloadHTTPClient.Timeout = downloadTimeout
	downloadOpts := []option.ClientOption{
		option.WithHTTPClient(downloadHTTPClient),
	}
	if endpoint != "" {
		downloadOpts = append(downloadOpts, option.WithEndpoint(endpoint))
	}
	downloadDriveSvc, err := drive.NewService(ctx, downloadOpts...)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: build download drive client: %w", err)
	}

	return &client{sheets: sheetsSvc, drive: driveSvc, downloadDrive: downloadDriveSvc}, nil
}

// SpreadsheetMeta implements sheetsAPI.
func (c *client) SpreadsheetMeta(ctx context.Context, spreadsheetID string) (*Meta, error) {
	resp, err := c.sheets.Spreadsheets.Get(spreadsheetID).
		Fields("spreadsheetId,properties.title,sheets(properties(title,gridProperties(rowCount,columnCount)))").
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("googlesheets: get spreadsheet metadata: %w", err)
	}

	meta := &Meta{SpreadsheetID: resp.SpreadsheetId}
	if resp.Properties != nil {
		meta.Title = resp.Properties.Title
	}
	for _, sh := range resp.Sheets {
		if sh.Properties == nil {
			continue
		}
		sm := SheetMeta{Title: sh.Properties.Title}
		if sh.Properties.GridProperties != nil {
			sm.RowCount = int(sh.Properties.GridProperties.RowCount)
			sm.ColumnCount = int(sh.Properties.GridProperties.ColumnCount)
		}
		meta.Sheets = append(meta.Sheets, sm)
	}
	return meta, nil
}

// Values implements sheetsAPI.
func (c *client) Values(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
	resp, err := c.sheets.Spreadsheets.Values.Get(spreadsheetID, a1Range).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("googlesheets: get values: %w", err)
	}
	return resp.Values, nil
}

// ListFiles implements sheetsAPI. The query escapes folderID's single
// quotes — it comes from stored connector config, not agent input, but this
// is the one place it is interpolated into a Drive query string. The
// requested field mask includes size/modifiedTime (step 9) so
// drive.FileSummary's own fields never come back zero-valued for a listed
// file the way they would if this mask only asked for id/name/mimeType.
func (c *client) ListFiles(ctx context.Context, folderID string, pageToken string) (*FilePage, error) {
	call := c.drive.Files.List().
		Q(fmt.Sprintf("'%s' in parents and trashed = false", escapeDriveQueryValue(folderID))).
		Fields("nextPageToken, files(id, name, mimeType, size, modifiedTime)").
		Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}

	resp, err := call.Do()
	if err != nil {
		return nil, fmt.Errorf("googlesheets: list files: %w", err)
	}

	page := &FilePage{NextPageToken: resp.NextPageToken}
	for _, f := range resp.Files {
		page.Files = append(page.Files, File{
			ID: f.Id, Name: f.Name, MimeType: f.MimeType,
			SizeBytes: f.Size, ModifiedTime: f.ModifiedTime,
		})
	}
	return page, nil
}

// File implements sheetsAPI. The field mask includes parents (step 9's
// GetFile allowlist re-check needs a file's direct parent folder ids), size
// (the download route's maxDownloadBytes pre-check), and modifiedTime.
func (c *client) File(ctx context.Context, fileID string) (*File, error) {
	f, err := c.drive.Files.Get(fileID).Fields("id, name, mimeType, size, modifiedTime, parents").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("googlesheets: get file: %w", err)
	}
	return &File{
		ID: f.Id, Name: f.Name, MimeType: f.MimeType,
		Parents: f.Parents, SizeBytes: f.Size, ModifiedTime: f.ModifiedTime,
	}, nil
}

// DownloadFile implements sheetsAPI. Files.Get(fileID).Download() issues the
// same GET as a metadata fetch but with alt=media, returning the raw
// response so the caller (internal/module/mcp/handler.go's downloadFile)
// can stream resp.Body through a bounded reader without ever buffering the
// whole file — the same "never load a whole file into memory" discipline
// query.go's scan loop applies to sheet rows. The caller owns resp.Body and
// must close it.
//
// Deliberately uses c.downloadDrive, not c.drive: this call's body read can
// legitimately take far longer than requestTimeout (a large file over a
// slow connection), and requestTimeout is scoped to the client every other
// method here shares — see downloadTimeout's doc comment for what goes
// wrong if this call used that client instead.
func (c *client) DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, string, error) {
	resp, err := c.downloadDrive.Files.Get(fileID).Context(ctx).Download()
	if err != nil {
		return nil, "", fmt.Errorf("googlesheets: download file: %w", err)
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

func escapeDriveQueryValue(v string) string {
	return strings.ReplaceAll(v, "'", "\\'")
}
