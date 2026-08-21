package googlesheets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// This file is docs/07-sheets-adapter-plan.md step 9: the Drive half of the
// adapter, drive_list_folder and drive_get_file. It shares the same
// allowlist and OAuth machinery steps 5-8 built for Sheets — Config,
// newClient, RateCharger, RateLimitedError — because Sheets and Drive are
// one adapter behind one OAuth scope (docs/06-sheets-adapter.md §1).

// ErrFolderNotAllowed is returned when a Drive folder id is absent from the
// connector's own allowlist (config.scope.drive_folder_ids) — the Drive
// analogue of ErrSpreadsheetNotAllowed, same doc-comment discipline: wrapped
// with the offending id via fmt.Errorf's %w, so a caller can both errors.Is
// against this sentinel and read the id out of Error(). A folder id is an
// allowlist entry, not a credential, so it is safe to surface to the model
// (docs/06-sheets-adapter.md §8's convention of naming the fix).
var ErrFolderNotAllowed = errors.New("googlesheets: folder not on connector allowlist")

// ErrFileNotAllowed is returned by GetFile when a file's own direct parent
// folders — not the folder id the caller happened to ask about, since
// drive_get_file takes a bare file id with no folder context at all — fail
// to intersect the connector's allowlisted drive_folder_ids. Only *direct*
// parents are ever considered: a file two levels deep inside an allowlisted
// folder is rejected unless its immediate parent is itself allowlisted.
// This is the conservative reading of docs/06-sheets-adapter.md §3 ("the
// adapter must reject any spreadsheet/folder not in the allowlist, always")
// — traversing nested subfolders would mean one allowlisted folder id
// silently grants everything beneath it, an unbounded and unaudited scope
// expansion the config's author never explicitly opted into. If nested
// access is wanted, each subfolder must be allowlisted itself.
var ErrFileNotAllowed = errors.New("googlesheets: file's parent folder not on connector allowlist")

// ErrFileNotFound is returned when a file id doesn't resolve at all — an
// upstream 404, a malformed id, or any other failure fetching a single
// file's metadata. getFile collapses every such failure into this one
// sentinel rather than propagating the upstream error text verbatim: an
// upstream error can carry more detail than is safe to hand to the model
// (see this package's doc comment on never letting a connector's decrypted
// config, and by extension anything upstream-error-shaped that might
// reference it, leave this package), and from the caller's perspective "the
// file id doesn't work" is the only actionable fact either way.
var ErrFileNotFound = errors.New("googlesheets: file not found")

// maxFolderFiles caps how many files drive_list_folder returns from a
// single upstream page, defensively — the Drive API's own default page
// size for Files.List is already well under this, but a future change to
// ListFiles' requested page size (or a Drive API behavior change) should
// never let one tool call return an unbounded number of files to the model.
// A page over the cap is truncated and reported via HasMore rather than
// rejected outright, mirroring sheets_query_rows' "return what fits, say
// there's more" convention rather than sheets_read_range's "reject
// up front" one — the difference being that read_range's caller chose an
// oversized range itself, while a folder's file count is not something the
// caller controls at all.
const maxFolderFiles = 200

// ---------------------------------------------------------------------------
// drive_list_folder
// ---------------------------------------------------------------------------

// ListFolderInput is drive_list_folder's input. PageToken is empty for the
// first page; a non-empty ListFolderOutput.NextPageToken means more pages
// remain.
type ListFolderInput struct {
	FolderID  string
	PageToken string
}

// FileSummary is one entry ListFolder returns — enough for the model to
// recognize a file and pass its id to drive_get_file next, without a
// separate metadata round trip for every file in a folder listing.
type FileSummary struct {
	FileID       string
	Name         string
	MimeType     string
	SizeBytes    int64
	ModifiedTime string
}

// ListFolderOutput is drive_list_folder's result.
type ListFolderOutput struct {
	FolderID      string
	Files         []FileSummary
	NextPageToken string
	HasMore       bool
}

// ListFolder checks cfg's allowlist first — before any client construction
// or network call, the same "checked fresh on every call, before spending
// any OAuth or rate-limit budget" discipline QueryRows and ReadRange follow
// — then lists one page of folderID's direct children.
func ListFolder(ctx context.Context, ts oauth2.TokenSource, cfg *Config, connectorID uuid.UUID, charger RateCharger, in ListFolderInput) (*ListFolderOutput, error) {
	if !cfg.IsFolderAllowed(in.FolderID) {
		return nil, fmt.Errorf("%w: %s", ErrFolderNotAllowed, in.FolderID)
	}
	api, err := newClient(ctx, ts, "")
	if err != nil {
		return nil, err
	}
	return listFolder(ctx, api, connectorID, charger, in)
}

// listFolder is ListFolder minus client construction and the allowlist
// check (already done by the caller), so drive_test.go can drive it against
// a mocked sheetsAPI instead of a real Google client.
func listFolder(ctx context.Context, api sheetsAPI, connectorID uuid.UUID, charger RateCharger, in ListFolderInput) (*ListFolderOutput, error) {
	// One charge per upstream ListFiles page — this call fetches exactly
	// one page, so exactly one charge, mirroring readRange's single charge
	// for its single Values.Get (docs/07 step 9: "charge 1 rate unit per
	// upstream ListFiles page"). Unlike queryRows' scan, a folder listing
	// has no partial answer to fall back to across an exhausted bucket: an
	// empty bucket here is always a full denial.
	if charger != nil {
		allowed, retryAfter, err := charger.ChargeRateLimit(ctx, connectorID, 1)
		if err != nil {
			return nil, fmt.Errorf("googlesheets: list folder: charge rate limit: %w", err)
		}
		if !allowed {
			return nil, &RateLimitedError{RetryAfter: retryAfter}
		}
	}

	page, err := api.ListFiles(ctx, in.FolderID, in.PageToken)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: list folder %q: %w", in.FolderID, err)
	}

	// NextPageToken is always echoed back exactly as Drive returned it
	// (docs/07 step 9: "pass NextPageToken straight back out"), even when
	// this page is also truncated below — a caller resuming from it
	// continues after this whole upstream page, not from the truncation
	// point, since Drive itself has no notion of "resume mid-page".
	hasMore := page.NextPageToken != ""
	files := page.Files
	if len(files) > maxFolderFiles {
		files = files[:maxFolderFiles]
		hasMore = true
	}

	out := &ListFolderOutput{FolderID: in.FolderID, NextPageToken: page.NextPageToken, HasMore: hasMore}
	out.Files = make([]FileSummary, 0, len(files))
	for _, f := range files {
		out.Files = append(out.Files, FileSummary{
			FileID: f.ID, Name: f.Name, MimeType: f.MimeType,
			SizeBytes: f.SizeBytes, ModifiedTime: f.ModifiedTime,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// drive_get_file
// ---------------------------------------------------------------------------

// FileInfo is drive_get_file's metadata result — the same shape as
// FileSummary, kept as a distinct type because the two are conceptually
// different results (one entry among many vs. the single file the caller
// asked about by id) even though their fields happen to coincide today.
type FileInfo struct {
	FileID       string
	Name         string
	MimeType     string
	SizeBytes    int64
	ModifiedTime string
}

// GetFile builds a client and fetches fileID's metadata. Unlike ListFolder
// (and QueryRows/ReadRange before it), the allowlist check here cannot run
// up front: drive_get_file takes a bare file id with no folder context, so
// there is nothing to check against the allowlist until the file's own
// parent folders are known — which requires the fetch itself. getFile
// therefore charges and fetches first, then re-checks.
func GetFile(ctx context.Context, ts oauth2.TokenSource, cfg *Config, connectorID uuid.UUID, charger RateCharger, fileID string) (*FileInfo, error) {
	api, err := newClient(ctx, ts, "")
	if err != nil {
		return nil, err
	}
	return getFile(ctx, api, cfg, connectorID, charger, fileID)
}

// getFile is GetFile minus client construction, so drive_test.go can drive
// it against a mocked sheetsAPI. Order of operations:
//
//  1. Charge one rate-limit unit for the single upstream File() call about
//     to be made — same one-charge-per-upstream-call discipline as
//     readRange's Values.Get charge.
//  2. Fetch the file's metadata. Any failure (a real 404, a revoked share,
//     a malformed id — sheetsAPI.File doesn't distinguish, and this package
//     never surfaces upstream error detail that might reference a
//     connector's credentials) collapses to ErrFileNotFound.
//  3. Re-check cfg's allowlist against the *fetched* Parents — never a
//     value assumed from the caller's input, since the caller supplied only
//     a file id, not a folder. Only direct parents count (see
//     ErrFileNotAllowed's doc comment for why nested subfolders are
//     deliberately not traversed).
func getFile(ctx context.Context, api sheetsAPI, cfg *Config, connectorID uuid.UUID, charger RateCharger, fileID string) (*FileInfo, error) {
	if charger != nil {
		allowed, retryAfter, err := charger.ChargeRateLimit(ctx, connectorID, 1)
		if err != nil {
			return nil, fmt.Errorf("googlesheets: get file: charge rate limit: %w", err)
		}
		if !allowed {
			return nil, &RateLimitedError{RetryAfter: retryAfter}
		}
	}

	f, err := api.File(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, fileID)
	}

	if !anyAllowed(f.Parents, cfg.Scope.DriveFolderIDs) {
		return nil, fmt.Errorf("%w: %s", ErrFileNotAllowed, fileID)
	}

	return &FileInfo{
		FileID: f.ID, Name: f.Name, MimeType: f.MimeType,
		SizeBytes: f.SizeBytes, ModifiedTime: f.ModifiedTime,
	}, nil
}

// anyAllowed reports whether any of parents is present in allowlist — the
// "direct parents intersect the allowlist" test ErrFileNotAllowed's doc
// comment describes. Built on config.go's own contains rather than a set,
// since both lists are small (a handful of parent folders, a handful of
// allowlisted ones).
func anyAllowed(parents, allowlist []string) bool {
	for _, p := range parents {
		if contains(allowlist, p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Download (used by internal/module/mcp/handler.go's downloadFile route)
// ---------------------------------------------------------------------------

// DownloadFile builds a client and streams fileID's raw bytes. newClient is
// unexported, so — exactly like ListFolder/GetFile above — this thin
// exported wrapper is what a caller outside this package (the MCP gateway's
// download route) actually calls; there is no separate "downloadFile" seam
// function because there is nothing left to test once sheetsAPI.DownloadFile
// itself is covered: this wrapper does no allowlist check, no rate-limit
// charge, and no error translation of its own — the download route performs
// all three itself (via a prior GetFile call, its own ChargeRateLimit call,
// and a plain error path) because streaming is inherently an HTTP-layer
// concern, not something this package's CallToolResult-shaped error mapping
// applies to.
func DownloadFile(ctx context.Context, ts oauth2.TokenSource, fileID string) (io.ReadCloser, string, error) {
	api, err := newClient(ctx, ts, "")
	if err != nil {
		return nil, "", err
	}
	return api.DownloadFile(ctx, fileID)
}

// ---------------------------------------------------------------------------
// Google-native files (Docs/Sheets/Slides/...) have no raw bytes
// ---------------------------------------------------------------------------

// googleNativeMimePrefix identifies a Google Workspace native format (a
// Doc, Sheet, Slide deck, ...) — these exist only as Google's own internal
// representation and have no "raw bytes" a Files.Get(...).Download() call
// can return; they must be exported to a concrete format first (out of
// scope for the read-only MVP). drive_get_file checks this before ever
// minting a download link, per docs/07-sheets-adapter-plan.md step 9.
const googleNativeMimePrefix = "application/vnd.google-apps."

// IsGoogleNativeMimeType reports whether mime names a Google-native format.
func IsGoogleNativeMimeType(mime string) bool {
	return strings.HasPrefix(mime, googleNativeMimePrefix)
}
