package mcp

import (
	"errors"
	"fmt"

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
