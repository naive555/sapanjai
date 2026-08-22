package mcp

import (
	"errors"
	"fmt"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/shared/apperror"
)

// The apperror -> MCP-tool-result mapping — a second mapping alongside
// internal/server's apperror -> HTTP-status one, not a replacement.
//
// MCP has two failure channels and only one is safe for a denial or a
// business-logic failure. A Go error returned from a tools/call handler is a
// JSON-RPC protocol error: the client's turn aborts and the model never
// learns why. A CallToolResult with IsError is an ordinary result: the model
// reads the text, can adapt, and the session survives. Denials always take
// the second channel.
//
// Tool-specific messages live next to the tools that raise them
// (tools_sheets.go, tools_drive.go); this file holds only the shared shape
// and the connector-type-agnostic results.

// errResult builds the IsError tool result every failure message here uses.
// One constructor, so the shape can gain a field (a machine-readable code,
// a second content block) in one place rather than nineteen.
func errResult(format string, args ...any) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		IsError: true,
		Content: []gomcp.Content{&gomcp.TextContent{
			Text: fmt.Sprintf(format, args...),
		}},
	}
}

// PermissionDenied builds the result a tools/call denial returns. The text is
// byte-identical to the REST 403 body (middleware.RequirePermission's
// "Missing permission: <action>") so gateway audit rows and API logs grep
// alike.
func PermissionDenied(action string) *gomcp.CallToolResult {
	return errResult("Missing permission: %s", action)
}

// RateLimited builds the result a tools/call returns when the connector's
// bucket is exhausted. Like PermissionDenied this takes the IsError channel:
// a protocol error would abort the turn with no explanation, while a stated
// retry-after lets the agent back off and try again.
func RateLimited(retryAfter time.Duration) *gomcp.CallToolResult {
	return errResult("RATE_LIMITED: this connector's request budget is exhausted. Retry after %d seconds.",
		retrySeconds(retryAfter))
}

// retrySeconds renders a retry-after as whole seconds, shared with the REST
// side's rateLimitedMessage (handler.go) so both state it identically.
func retrySeconds(retryAfter time.Duration) int64 {
	return int64(retryAfter.Round(time.Second).Seconds())
}

// ErrorResult converts a service-level error into an IsError result:
// apperror.Resolve's message for a known *apperror.Error, matching what the
// REST API would return for the same failure, or a generic message.
func ErrorResult(err error) *gomcp.CallToolResult {
	var appErr *apperror.Error
	msg := "Internal error"
	if errors.As(err, &appErr) {
		_, msg = apperror.Resolve(appErr.Code)
	}
	return errResult("%s", msg)
}
