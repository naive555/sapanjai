package googlesheets

import (
	"context"
	"io"
)

// Meta is what sheetsAPI.SpreadsheetMeta returns: enough for the health
// check to prove a spreadsheet resolves, and the shape
// sheets_describe_spreadsheet builds its schema discovery on.
type Meta struct {
	SpreadsheetID string
	Title         string
	Sheets        []SheetMeta
}

// SheetMeta describes one tab within a spreadsheet.
type SheetMeta struct {
	Title       string
	RowCount    int
	ColumnCount int
}

// File is a Drive file's metadata. Parents feeds GetFile's allowlist
// re-check, SizeBytes the download route's size cap, ModifiedTime (RFC3339,
// as Drive reports it) the model. Deliberately no content and no link — a
// signed link is minted separately in the mcp module, never stored here.
type File struct {
	ID           string
	Name         string
	MimeType     string
	Parents      []string
	SizeBytes    int64
	ModifiedTime string
}

// FilePage is one page of ListFiles results.
type FilePage struct {
	Files         []File
	NextPageToken string
}

// sheetsAPI is the narrow seam over the Google SDKs every adapter operation
// depends on, naming exactly the upstream calls this package makes so unit
// tests can mock it and never touch the network. client.go is the only
// production implementation; client_test.go's contract test is the sole
// place a real Google response shape is asserted.
type sheetsAPI interface {
	// SpreadsheetMeta returns a spreadsheet's title, tabs, and each tab's
	// dimensions.
	SpreadsheetMeta(ctx context.Context, spreadsheetID string) (*Meta, error)

	// Values returns the raw cell values for an A1-notation range
	// (e.g. "Sheet1!A1:D100"). Rows are returned as Google returns them:
	// ragged (a row shorter than its header has fewer entries, not nils),
	// possibly empty.
	Values(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error)

	// ListFiles lists one page of a Drive folder's direct children.
	// pageToken is empty for the first page; a non-empty
	// FilePage.NextPageToken means more pages remain.
	ListFiles(ctx context.Context, folderID string, pageToken string) (*FilePage, error)

	// File returns one Drive file's metadata by id.
	File(ctx context.Context, fileID string) (*File, error)

	// DownloadFile streams one Drive file's raw bytes — the only method here
	// returning a live, caller-owned io.ReadCloser rather than a buffered
	// value, so the caller must Close it. The second return is the upstream
	// Content-Type, forwarded as-is. Google-native files have no raw bytes;
	// callers must check IsGoogleNativeMimeType before calling this.
	DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, string, error)
}
