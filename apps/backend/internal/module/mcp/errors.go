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
