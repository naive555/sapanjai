// Package gateway turns an authenticated principal into an MCP server whose
// visible tool surface is exactly that principal's RBAC grant.
//
// Two independent enforcement layers, on purpose:
//
//  1. Construction-time filtering (BuildServer). Tools the principal lacks
//     permission for are never registered, so they never appear in
//     tools/list and calling one returns the SDK's own "unknown tool" error.
//     This is the layer that matters for model behavior: a tool the model
//     cannot see is a tool it will not try to use, which saves tokens and
//     avoids teaching it to expect failures.
//
//  2. Request-time enforcement (EnforcePermissions, an mcp.Middleware on
//     tools/call). Redundant against layer 1 today, but it is what keeps the
//     system correct once permissions can change mid-session, once tools are
//     registered dynamically, and once a client caches a stale tool list.
//     Never rely on the tool list as an authorization boundary.
package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/spikes/mcp-gateway/internal/rbac"
	"github.com/sapanjai/spikes/mcp-gateway/internal/tools"
)

// ServerName and ServerVersion identify this spike to clients during the
// initialize handshake.
const (
	ServerName    = "sapanjai-mcp-spike"
	ServerVersion = "0.1.0"
)

// BuildServer returns an *mcp.Server exposing only the catalog tools that p
// is permitted to use. Constructing a fresh server per session is cheap
// (no I/O, no goroutines) and is what makes per-tenant tool surfaces possible
// at all — see docs/FINDINGS.md, "one server instance per principal".
func BuildServer(p *rbac.Principal, logger *slog.Logger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, &mcp.ServerOptions{
		Instructions: "Read and create invoices in the caller's Sapanjai organization. " +
			"All amounts are THB and include Thai 7% VAT. The organization is fixed by the " +
			"caller's credentials; there is no way to query another organization.",
		Logger: logger,
	})

	var granted, denied []string
	for _, e := range tools.Catalog() {
		if p.HasPermission(e.Permission) {
			e.Register(s, p)
			granted = append(granted, e.Name)
			continue
		}
		denied = append(denied, e.Name)
	}

	s.AddReceivingMiddleware(EnforcePermissions(p, logger))

	if logger != nil {
		logger.Info("built scoped mcp server",
			slog.String("user_id", p.UserID),
			slog.String("org_id", p.OrgID),
			slog.String("role", p.Role),
			slog.Any("tools_granted", granted),
			slog.Any("tools_denied", denied),
		)
	}

	return s
}

// EnforcePermissions re-checks the RBAC action for every tools/call before it
// reaches a handler. It also scrubs tools/list, which is belt-and-braces given
// BuildServer's filtering but becomes load-bearing the moment tools are
// registered on a shared server rather than a per-principal one.
func EnforcePermissions(p *rbac.Principal, logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/call":
				params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
				if !ok {
					return next(ctx, method, req)
				}
				action, known := tools.PermissionFor(params.Name)
				if !known {
					// Not ours to judge; let the SDK produce its own
					// "unknown tool" error.
					return next(ctx, method, req)
				}
				if !p.HasPermission(action) {
					if logger != nil {
						logger.Warn("mcp tool call denied",
							slog.String("user_id", p.UserID),
							slog.String("org_id", p.OrgID),
							slog.String("tool", params.Name),
							slog.String("required_permission", action),
						)
					}
					// Returned as a tool result, not a Go error: the model
					// should learn "I may not do this" and move on, rather
					// than the session dying on a JSON-RPC error. The string
					// matches controlplane's 403 body ("Missing permission:
					// <action>") so gateway logs and API logs grep alike.
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{
							Text: fmt.Sprintf("Missing permission: %s", action),
						}},
					}, nil
				}

			case "tools/list":
				res, err := next(ctx, method, req)
				if err != nil {
					return res, err
				}
				list, ok := res.(*mcp.ListToolsResult)
				if !ok {
					return res, nil
				}
				kept := make([]*mcp.Tool, 0, len(list.Tools))
				for _, t := range list.Tools {
					action, known := tools.PermissionFor(t.Name)
					if known && !p.HasPermission(action) {
						continue
					}
					kept = append(kept, t)
				}
				list.Tools = kept
				return list, nil
			}

			return next(ctx, method, req)
		}
	}
}
