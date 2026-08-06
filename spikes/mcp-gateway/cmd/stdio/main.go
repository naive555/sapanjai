// Command stdio runs the spike as a local stdio MCP server, the transport
// Claude Desktop and Claude Code use for locally-spawned servers.
//
// Because stdio has no request headers and no per-request identity, the
// principal is fixed for the lifetime of the process and comes from the
// environment (SAPANJAI_TOKEN). That is a genuine protocol constraint, not a
// shortcut — see docs/FINDINGS.md, "stdio cannot be multi-tenant".
//
// Register with Claude Code:
//
//	claude mcp add sapanjai-spike \
//	  --env SAPANJAI_TOKEN=tok_reader_siam \
//	  -- /abs/path/to/sapanjai-mcp-stdio
//
// CRITICAL: nothing may be written to stdout except JSON-RPC frames. All
// logging goes to stderr.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/spikes/mcp-gateway/internal/gateway"
	"github.com/sapanjai/spikes/mcp-gateway/internal/principal"
)

func main() {
	// stderr, never stdout: a stray stdout byte corrupts the JSON-RPC stream
	// and the client drops the connection with an opaque parse error.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	token := os.Getenv("SAPANJAI_TOKEN")
	if token == "" {
		token = "tok_reader_siam"
		logger.Warn("SAPANJAI_TOKEN unset, defaulting", slog.String("token", token))
	}

	p, err := principal.Resolve(token)
	if err != nil {
		logger.Error("cannot resolve principal", slog.String("error", err.Error()))
		os.Exit(1)
	}

	server := gateway.BuildServer(p, logger)

	logger.Info("sapanjai mcp spike starting on stdio",
		slog.String("user", p.Email),
		slog.String("org_id", p.OrgID),
		slog.String("role", p.Role),
	)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Error("server exited", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
