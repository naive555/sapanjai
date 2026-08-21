package googlesheets

import (
	"context"
	"io"
)

// Meta is the metadata sheetsAPI.SpreadsheetMeta returns: enough for step
// 5's health check (the spreadsheet resolves, its title and tabs are
// readable) and the schema-discovery shape step 6's sheets_describe_
// spreadsheet builds on.
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

// File is a Drive file's metadata. Parents/SizeBytes/ModifiedTime were
// added in step 9 (docs/07-sheets-adapter-plan.md): Parents is what
// GetFile's allowlist re-check needs (a file's direct parent folders,
// compared against config.scope.drive_folder_ids — see getFile's doc
// comment for why only *direct* parents count), SizeBytes is what the
// download route's maxDownloadBytes cap checks before ever opening a
// stream, and ModifiedTime (RFC3339, as Drive returns it) is surfaced to
// the model as-is. Still no content and no link — a signed, short-lived
// link is minted separately, in internal/module/mcp/filelink.go, never
// stored on this struct.
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

// sheetsAPI is the narrow seam over the Google Sheets/Drive SDKs that every
// adapter operation depends on — the house style of connector.connectorStore
// / connector.sealer (internal/module/connector/service.go): a small
// interface naming exactly the upstream operations this package needs, so
// unit tests can mock it directly and never touch the network. client.go is
// the only production implementation; client_test.go's one contract test is
// the sole place a real Google response shape is asserted.
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

	// DownloadFile streams one Drive file's raw bytes by id (step 9's
	// download route, internal/module/mcp/handler.go's downloadFile) — the
	// only sheetsAPI method that returns a live, caller-owned io.ReadCloser
	// rather than a fully-buffered value; the caller must Close it. The
	// second return is the upstream Content-Type, forwarded as-is so the
	// downloaded bytes are served with whatever type Drive itself reports.
	// Google-native files (Docs/Sheets/Slides — see
	// IsGoogleNativeMimeType) have no raw bytes to download at all; callers
	// must check that before ever calling this.
	DownloadFile(ctx context.Context, fileID string) (io.ReadCloser, string, error)
}
