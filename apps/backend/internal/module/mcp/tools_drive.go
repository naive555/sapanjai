// The Drive half of the adapter: drive_list_folder and drive_get_file.
// Gated by connector.TypeGoogleSheets and PermissionDriveRead, which
// sheets:read never grants — the matcher's "*" > exact > "resource:*"
// semantics mean sheets:read matches only sheets:read and sheets:*. Config
// is re-decrypted on every invocation, as in tools_sheets.go.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/adapter/googlesheets"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/rbac"
)

// PermissionDriveRead gates every drive_* read tool. Explicitly its own
// action, neither implying nor implied by sheets:read, so an org can let an
// agent read spreadsheets without browsing Drive folders, or the reverse.
const PermissionDriveRead = "drive:read"

var driveListFolderEntry = Entry{
	Name:          "drive_list_folder",
	Permission:    PermissionDriveRead,
	Description:   driveListFolderDescription,
	ConnectorType: connector.TypeGoogleSheets,
	Register:      registerDriveListFolder,
}

var driveGetFileEntry = Entry{
	Name:          "drive_get_file",
	Permission:    PermissionDriveRead,
	Description:   driveGetFileDescription,
	ConnectorType: connector.TypeGoogleSheets,
	Register:      registerDriveGetFile,
}

// ---------------------------------------------------------------------------
// drive_list_folder
// ---------------------------------------------------------------------------

const driveListFolderDescription = "List the files directly inside one of this connector's allowlisted Drive " +
	"folders (config.scope.drive_folder_ids) — folder_id must be one of those, not an arbitrary Drive folder id, " +
	"even one the connector's OAuth account can otherwise reach. Only *direct* children are listed: a subfolder's " +
	"own contents are not included, and a subfolder must itself be allowlisted before its files (or the subfolder " +
	"itself, as an entry here) become visible. Results are capped per call; when has_more is true, call again with " +
	"next_page_token to continue. Use the file_id this returns with drive_get_file to fetch one file's full " +
	"metadata and, for non-Google-native files, a short-lived download link."

type driveListFolderInput struct {
	FolderID  string `json:"folder_id" jsonschema:"one of this connector's allowlisted Drive folder ids (config.scope.drive_folder_ids)"`
	PageToken string `json:"page_token,omitempty" jsonschema:"the next_page_token from a previous call, to continue listing"`
}

type driveFileSummaryOutput struct {
	FileID       string `json:"file_id" jsonschema:"the file's Drive id — pass this to drive_get_file"`
	Name         string `json:"name"`
	MimeType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	ModifiedTime string `json:"modified_time" jsonschema:"RFC3339, as Drive reports it"`
}

type driveListFolderOutput struct {
	FolderID      string                   `json:"folder_id"`
	Files         []driveFileSummaryOutput `json:"files"`
	NextPageToken string                   `json:"next_page_token,omitempty"`
	HasMore       bool                     `json:"has_more" jsonschema:"true if more files remain — call again with next_page_token"`
}

func registerDriveListFolder(s *gomcp.Server, svc *Service, _ *rbac.Principal, conn db.Connector, _ RequestInfo) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "drive_list_folder",
		Description: driveListFolderDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *gomcp.CallToolRequest, in driveListFolderInput) (*gomcp.CallToolResult, driveListFolderOutput, error) {
		// Presence check first, before any config decryption or network
		// call — same discipline every sheets_* tool's required-field check
		// follows.
		if in.FolderID == "" {
			return missingFolderID(), driveListFolderOutput{}, nil
		}

		cfg, err := svc.openGoogleSheetsConfig(ctx, conn)
		if err != nil {
			return ErrorResult(err), driveListFolderOutput{}, nil
		}
		if !cfg.IsFolderAllowed(in.FolderID) {
			return FolderNotAllowed(in.FolderID), driveListFolderOutput{}, nil
		}

		ts := svc.sheetsTokens.Get(ctx, conn.ID, cfg.OAuth)
		result, err := googlesheets.ListFolder(ctx, ts, cfg, conn.ID, svc, googlesheets.ListFolderInput{
			FolderID: in.FolderID, PageToken: in.PageToken,
		})
		if err != nil {
			var rlErr *googlesheets.RateLimitedError
			switch {
			case errors.Is(err, googlesheets.ErrFolderNotAllowed):
				return FolderNotAllowed(in.FolderID), driveListFolderOutput{}, nil
			case errors.As(err, &rlErr):
				return RateLimited(rlErr.RetryAfter), driveListFolderOutput{}, nil
			default:
				return ErrorResult(err), driveListFolderOutput{}, nil
			}
		}

		out := driveListFolderOutput{
			FolderID:      result.FolderID,
			NextPageToken: result.NextPageToken,
			HasMore:       result.HasMore,
			Files:         make([]driveFileSummaryOutput, 0, len(result.Files)),
		}
		for _, f := range result.Files {
			out.Files = append(out.Files, driveFileSummaryOutput{
				FileID: f.FileID, Name: f.Name, MimeType: f.MimeType,
				SizeBytes: f.SizeBytes, ModifiedTime: f.ModifiedTime,
			})
		}
		return buildDriveListFolderResult(out), out, nil
	})
}

// buildDriveListFolderResult renders out as a markdown table, escaped per
// cell so a file name containing "|" or a newline cannot corrupt it.
func buildDriveListFolderResult(out driveListFolderOutput) *gomcp.CallToolResult {
	var b strings.Builder
	if len(out.Files) == 0 {
		b.WriteString("No files in this folder.\n\n")
	} else {
		b.WriteString("| file_id | name | mime_type | size_bytes | modified_time |\n")
		b.WriteString("| --- | --- | --- | --- | --- |\n")
		for _, f := range out.Files {
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %s |\n",
				escapeMarkdownCell(f.FileID), escapeMarkdownCell(f.Name), escapeMarkdownCell(f.MimeType),
				f.SizeBytes, escapeMarkdownCell(f.ModifiedTime))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "folder_id=%s has_more=%v", out.FolderID, out.HasMore)
	if out.NextPageToken != "" {
		fmt.Fprintf(&b, " next_page_token=%s", out.NextPageToken)
	}
	b.WriteString("\n")
	return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: b.String()}}}
}

// ---------------------------------------------------------------------------
// drive_get_file
// ---------------------------------------------------------------------------

// driveGetFileDescription is deliberate prompt surface. It says download_url
// is short-lived *and replayable*: there is no nonce and no consumption
// tracking, so a leaked link stays live for the rest of its TTL. Calling it
// "single-use" would understate that risk to the model.
const driveGetFileDescription = "Get one Drive file's metadata by id, plus — for files with actual bytes to " +
	"download — a short-lived download link good for 15 minutes. file_id typically comes from drive_list_folder; " +
	"this tool re-checks that the file's own parent folder is still on this connector's allowlist every time it's " +
	"called, so a file that was reachable a minute ago can become unreachable if the connector's scope narrowed. " +
	"Google-native files (Docs, Sheets, Slides, and similar — anything with no raw bytes of its own) never get a " +
	"download_url; for a Google Sheet specifically, use the sheets_* tools instead of trying to download it. When " +
	"download_url is present, treat it as sensitive and short-lived, not single-use: it works without any further " +
	"authentication and stays valid for anyone holding it until download_url_expires_at (at most 15 minutes from " +
	"when this tool ran) — it is not consumed or invalidated by using it, so it should still be discarded rather " +
	"than saved or repeated back verbatim once you're done with it."

type driveGetFileInput struct {
	FileID string `json:"file_id" jsonschema:"the Drive file id to fetch, typically from drive_list_folder"`
}

type driveGetFileOutput struct {
	FileID               string `json:"file_id"`
	Name                 string `json:"name"`
	MimeType             string `json:"mime_type"`
	SizeBytes            int64  `json:"size_bytes"`
	ModifiedTime         string `json:"modified_time" jsonschema:"RFC3339, as Drive reports it"`
	DownloadURL          string `json:"download_url,omitempty" jsonschema:"a short-lived, replayable link (not single-use — no consumption tracking) to download this file's raw bytes, valid until download_url_expires_at; omitted for a Google-native file (no raw bytes) or when link minting is unavailable"`
	DownloadURLExpiresAt string `json:"download_url_expires_at,omitempty" jsonschema:"RFC3339 timestamp download_url stops working at, at most 15 minutes after this call"`
}

func registerDriveGetFile(s *gomcp.Server, svc *Service, _ *rbac.Principal, conn db.Connector, req RequestInfo) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "drive_get_file",
		Description: driveGetFileDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *gomcp.CallToolRequest, in driveGetFileInput) (*gomcp.CallToolResult, driveGetFileOutput, error) {
		if in.FileID == "" {
			return missingFileID(), driveGetFileOutput{}, nil
		}

		cfg, err := svc.openGoogleSheetsConfig(ctx, conn)
		if err != nil {
			return ErrorResult(err), driveGetFileOutput{}, nil
		}

		// No allowlist pre-check here, unlike every other tool: a bare file
		// id carries no folder context, so there is nothing to check until
		// GetFile has fetched the file's own parents.
		ts := svc.sheetsTokens.Get(ctx, conn.ID, cfg.OAuth)
		result, err := googlesheets.GetFile(ctx, ts, cfg, conn.ID, svc, in.FileID)
		if err != nil {
			var rlErr *googlesheets.RateLimitedError
			switch {
			case errors.Is(err, googlesheets.ErrFileNotAllowed):
				return FileNotAllowed(in.FileID), driveGetFileOutput{}, nil
			case errors.Is(err, googlesheets.ErrFileNotFound):
				return FileNotFound(in.FileID), driveGetFileOutput{}, nil
			case errors.As(err, &rlErr):
				return RateLimited(rlErr.RetryAfter), driveGetFileOutput{}, nil
			default:
				return ErrorResult(err), driveGetFileOutput{}, nil
			}
		}

		out := driveGetFileOutput{
			FileID: result.FileID, Name: result.Name, MimeType: result.MimeType,
			SizeBytes: result.SizeBytes, ModifiedTime: result.ModifiedTime,
		}

		if googlesheets.IsGoogleNativeMimeType(result.MimeType) {
			return buildDriveGetFileResult(out, "This is a Google-native file (e.g. a Doc, Sheet, or Slide deck) "+
				"and has no raw bytes to download — no download_url is provided. For a Google Sheet, use the "+
				"sheets_* tools instead."), out, nil
		}

		if len(svc.fileLinkKey) == 0 {
			// An empty key disables minting: still return metadata, and say
			// links are unavailable. Unreachable in production, since
			// config.Load requires a non-empty master key, but unit tests
			// build a Service without one and must not sign with nil.
			return buildDriveGetFileResult(out, "A download link is unavailable for this connector right now."), out, nil
		}

		p, ok := principalFromContext(ctx)
		if !ok {
			// A wiring bug, not a client error: this tool is only registered
			// on a server built from a real principal, so no principal here
			// means SDK context propagation broke. Fail safe — metadata with
			// no link, rather than a link bound to no one.
			return buildDriveGetFileResult(out, "A download link could not be minted for this request."), out, nil
		}

		expiresAt := time.Now().Add(fileLinkTTL)
		out.DownloadURL = SignFileLink(svc.fileLinkKey, req.BaseURL, conn.OrganizationID, conn.ID, p.UserID, result.FileID, expiresAt)
		out.DownloadURLExpiresAt = expiresAt.UTC().Format(time.RFC3339)

		return buildDriveGetFileResult(out, fmt.Sprintf(
			"download_url is short-lived (expires at %s, at most 15 minutes from now) but replayable, not single-use — "+
				"it works for anyone holding it until it expires, so don't save it or repeat it back once you're done.",
			out.DownloadURLExpiresAt)), out, nil
	})
}

// buildDriveGetFileResult renders the file's metadata plus a note on the
// download_url situation — expiring, Google-native, or unavailable. The note
// is always present so the model never infers *why* a URL is missing.
func buildDriveGetFileResult(out driveGetFileOutput, note string) *gomcp.CallToolResult {
	text := fmt.Sprintf("file_id=%s name=%q mime_type=%s size_bytes=%d modified_time=%s\n%s",
		out.FileID, out.Name, out.MimeType, out.SizeBytes, out.ModifiedTime, note)
	return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: text}}}
}

// ---------------------------------------------------------------------------
// drive_* error results
// ---------------------------------------------------------------------------
//
// Folder and file ids are allowlist entries, not credentials, so each is
// safe to echo back alongside the fix.

func missingFolderID() *gomcp.CallToolResult {
	return errResult("folder_id is required. It must be one of this connector's allowlisted Drive folders.")
}

func missingFileID() *gomcp.CallToolResult {
	return errResult("file_id is required. Call drive_list_folder to see which ids this connector can access.")
}

func FolderNotAllowed(folderID string) *gomcp.CallToolResult {
	return errResult("FOLDER_NOT_ALLOWED: %q is not on this connector's allowlist. "+
		"Ask the connector's owner to add it to the connector's drive_folder_ids, or use a folder id that is already allowlisted.", folderID)
}

// FileNotAllowed reports a file none of whose *direct* parents is
// allowlisted — see googlesheets.ErrFileNotAllowed on why nesting doesn't
// count.
func FileNotAllowed(fileID string) *gomcp.CallToolResult {
	return errResult("FILE_NOT_ALLOWED: %q is not inside any of this connector's allowlisted Drive folders "+
		"(only a file's *direct* parent folder counts — a nested subfolder must be allowlisted itself). "+
		"Call drive_list_folder on an allowlisted folder to see which files this connector can read.", fileID)
}

func FileNotFound(fileID string) *gomcp.CallToolResult {
	return errResult("FILE_NOT_FOUND: %q does not exist, or is no longer reachable with this connector's credentials.", fileID)
}
