package mcp

import (
	"errors"
	"fmt"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/shared/apperror"
)

// This file owns the apperror -> MCP-tool-result mapping — a *second*
// mapping alongside internal/server's apperror -> HTTP-status one
// (newErrorHandler), not a replacement for it. The two exist because MCP
// has two distinct failure channels and only one of them is safe to use for
// an authorization or business-logic failure:
//
//   - a Go error returned from a tools/call handler (or from
//     Service.enforce) is a JSON-RPC protocol error — the client's turn
//     aborts, the model never sees why.
//   - an *mcp.CallToolResult with IsError: true is an ordinary tool result —
//     the model reads the text and can adapt ("I don't have access to
//     that"), the session stays alive.
//
// Denials and business-logic failures always take the second channel. See
// spikes/mcp-gateway/docs/FINDINGS.md §3.5, verified against SDK v1.7.0.

// PermissionDenied builds the CallToolResult a tools/call denial returns.
// The text is byte-identical to the REST 403 body
// (internal/middleware.RequirePermission's "Missing permission: <action>"),
// so gateway audit rows and API logs grep alike — Service.enforce is the
// only caller today.
func PermissionDenied(action string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf("Missing permission: %s", action),
		}},
	}
}

// RateLimited builds the CallToolResult a tools/call returns when the
// connector's upstream-API rate-limit bucket
// (internal/infra/redis.RateLimiter, key "mcp:ratelimit:<connectorId>") is
// exhausted — docs/07-sheets-adapter-plan.md step 4. Like PermissionDenied,
// this takes the IsError channel rather than a Go error: a JSON-RPC
// protocol error would abort the agent's turn with no explanation, while an
// ordinary tool result with a stated retry-after lets it back off and try
// again. apperror.RateLimited carries the code's static (status, message)
// pair for a future REST caller, but the retry-after is per-call and can
// only be expressed here, next to the value that produced it.
func RateLimited(retryAfter time.Duration) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf("RATE_LIMITED: this connector's request budget is exhausted. Retry after %d seconds.",
				int64(retryAfter.Round(time.Second).Seconds())),
		}},
	}
}

// SpreadsheetNotAllowed builds the CallToolResult docs/06-sheets-adapter.md
// §8's SPREADSHEET_NOT_ALLOWED describes: spreadsheetID is real enough that
// the connector's own OAuth token might well be able to reach it, but it is
// absent from this connector's allowlist (config.scope.spreadsheet_ids) —
// checked fresh on every call (googlesheets.Config.IsSpreadsheetAllowed),
// never against a value cached from connector-creation time. The message
// names the recovery path per docs/06 §8's "must state a fix": call
// sheets_list_spreadsheets to see what this connector can actually reach.
func SpreadsheetNotAllowed(spreadsheetID string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf(
				"SPREADSHEET_NOT_ALLOWED: %q is not on this connector's allowlist. "+
					"Call sheets_list_spreadsheets to see which spreadsheets this connector can access.",
				spreadsheetID),
		}},
	}
}

// invalidSampleRowCount builds the CallToolResult
// sheets_describe_spreadsheet returns when include_sample_rows falls
// outside its documented 0-maxSampleRows bound (docs/06-sheets-adapter.md
// §4.1). Checked before any config decryption or network call, so an
// out-of-range value never spends a byte of the connector's OAuth
// credential or rate-limit budget.
func invalidSampleRowCount(n int) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf("include_sample_rows must be between 0 and %d (got %d).", maxSampleRows, n),
		}},
	}
}

// missingSpreadsheetID builds the CallToolResult a sheets_* tool returns
// when the model omitted a required spreadsheet_id — a plain input error,
// checked before config decryption or the allowlist, so it costs nothing.
func missingSpreadsheetID() *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: "spreadsheet_id is required. Call sheets_list_spreadsheets to see which ids this connector can access.",
		}},
	}
}

// missingSheetName builds the CallToolResult sheets_query_rows returns when
// the model omitted a required sheet_name — checked before config
// decryption or the network, same as missingSpreadsheetID.
func missingSheetName() *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: "sheet_name is required. Call sheets_describe_spreadsheet to see this spreadsheet's tab names.",
		}},
	}
}

// invalidQueryRowsInput builds the CallToolResult sheets_query_rows returns
// for any input-shape problem ValidateQueryRowsInput or a Filter's own
// Validate catches: limit out of 1-200, a negative offset, a filter with a
// bad operator or a value shaped wrong for it. Every message these produce
// names only column names, operator names, and numeric bounds — never a
// filter's actual value — so echoing err.Error() straight to the model
// carries no business data.
func invalidQueryRowsInput(err error) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{Text: err.Error()}},
	}
}

// invalidResponseFormat builds the CallToolResult sheets_query_rows returns
// when response_format is set to anything other than "markdown" or "json".
func invalidResponseFormat(got string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf("response_format must be \"markdown\" or \"json\" (got %q).", got),
		}},
	}
}

// missingRange builds the CallToolResult sheets_read_range returns when
// the model omitted a required range — checked before config decryption or
// the network, same discipline as missingSpreadsheetID/missingSheetName.
func missingRange() *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: "range is required, e.g. \"Contracts!A1:D100\". Call sheets_describe_spreadsheet to see this spreadsheet's tab names.",
		}},
	}
}

// invalidReadRangeInput builds the CallToolResult sheets_read_range returns
// for any range-shape problem googlesheets.ValidateReadRangeInput catches:
// unparseable A1 syntax (including a formula or a second range smuggled in
// — docs/07-sheets-adapter-plan.md step 8's guardrail requirement), an
// unbounded range with no explicit row bounds on both ends, or a range
// spanning more rows/cells than the pre-fetch cap allows. Every message
// these produce names a fix and echoes back only the range string (or a
// column/sheet name) the caller itself supplied — never a cell value — so
// echoing err.Error() straight to the model carries no business data,
// mirroring invalidQueryRowsInput's same reasoning for sheets_query_rows.
func invalidReadRangeInput(err error) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{Text: err.Error()}},
	}
}

// SheetNotFound builds the CallToolResult docs/06-sheets-adapter.md §8's
// SHEET_NOT_FOUND describes: sheetName is not one of this spreadsheet's own
// tabs. Names the recovery path per §8's "must state a fix": call
// sheets_describe_spreadsheet to see the spreadsheet's actual tab names.
func SheetNotFound(sheetName string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf(
				"SHEET_NOT_FOUND: %q is not a tab in this spreadsheet. "+
					"Call sheets_describe_spreadsheet to see its actual tab names.",
				sheetName),
		}},
	}
}

// ColumnNotFound builds the CallToolResult docs/06-sheets-adapter.md §8's
// COLUMN_NOT_FOUND describes: column names neither a filter column nor a
// projected column that exists in the sheet's header row. column is a
// header name (or the caller's attempt at one) — structural, not a
// credential — so it is safe to echo back.
func ColumnNotFound(column string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf(
				"COLUMN_NOT_FOUND: %q is not a column in this sheet. "+
					"Call sheets_describe_spreadsheet to see the sheet's actual header row.",
				column),
		}},
	}
}

// ResultTooLarge builds the CallToolResult docs/06-sheets-adapter.md §8's
// RESULT_TOO_LARGE describes: the rows sheets_query_rows would return
// exceed the response size cap once encoded. Names both recovery paths
// §8 calls for: a narrower columns projection, or a smaller limit/more
// selective filter.
func ResultTooLarge() *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: "RESULT_TOO_LARGE: this query's result is too large to return. " +
				"Use the columns argument to project fewer fields, or narrow your filter / lower limit to return fewer rows.",
		}},
	}
}

// FolderNotAllowed builds the CallToolResult docs/06-sheets-adapter.md §8's
// convention prescribes for a Drive folder id absent from this connector's
// allowlist (config.scope.drive_folder_ids) — checked fresh on every call
// (googlesheets.Config.IsFolderAllowed), the same discipline
// SpreadsheetNotAllowed follows for spreadsheets. folderID is an allowlist
// entry, not a credential, so it is safe to echo back.
func FolderNotAllowed(folderID string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf(
				"FOLDER_NOT_ALLOWED: %q is not on this connector's allowlist. "+
					"Ask the connector's owner to add it to the connector's drive_folder_ids, or use a folder id that is already allowlisted.",
				folderID),
		}},
	}
}

// FileNotAllowed builds the CallToolResult drive_get_file returns when a
// file exists but none of its *direct* parent folders is on this
// connector's allowlist — googlesheets.ErrFileNotAllowed's doc comment
// explains why only direct parents count. fileID is safe to echo back for
// the same reason a spreadsheet or folder id is.
func FileNotAllowed(fileID string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf(
				"FILE_NOT_ALLOWED: %q is not inside any of this connector's allowlisted Drive folders "+
					"(only a file's *direct* parent folder counts — a nested subfolder must be allowlisted itself). "+
					"Call drive_list_folder on an allowlisted folder to see which files this connector can read.",
				fileID),
		}},
	}
}

// FileNotFound builds the CallToolResult drive_get_file/drive_list_folder
// return for googlesheets.ErrFileNotFound — a file id that doesn't resolve
// at all (deleted, never existed, or any other upstream failure fetching
// its metadata; this package never surfaces upstream error detail past its
// own boundary, see ErrFileNotFound's doc comment).
func FileNotFound(fileID string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf(
				"FILE_NOT_FOUND: %q does not exist, or is no longer reachable with this connector's credentials.",
				fileID),
		}},
	}
}

// missingFolderID builds the CallToolResult drive_list_folder returns when
// the model omitted a required folder_id — checked before config
// decryption or the network, same discipline as missingSpreadsheetID.
func missingFolderID() *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: "folder_id is required. It must be one of this connector's allowlisted Drive folders.",
		}},
	}
}

// missingFileID builds the CallToolResult drive_get_file returns when the
// model omitted a required file_id — checked before config decryption or
// the network, same discipline as missingSpreadsheetID.
func missingFileID() *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: "file_id is required. Call drive_list_folder to see which ids this connector can access.",
		}},
	}
}

// ErrorResult converts a service-level error into a CallToolResult with
// IsError set: apperror.Resolve's message for a known *apperror.Error
// (matching the text the REST API would return for the same failure), or a
// generic message for anything else. No tool in step 3 calls this — the one
// tool it ships (sapanjai_describe_connector) cannot fail — but it is the
// mapping later steps' real tool handlers (steps 5+) call so a NOT_FOUND,
// SPREADSHEET_NOT_ALLOWED, etc. reaches the model as a recoverable result
// instead of aborting the turn.
func ErrorResult(err error) *gomcp.CallToolResult {
	var appErr *apperror.Error
	msg := "Internal error"
	if errors.As(err, &appErr) {
		_, msg = apperror.Resolve(appErr.Code)
	}
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{Text: msg}},
	}
}
