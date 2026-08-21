package mcp

import (
	"context"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/connector"
)

// Entry binds one MCP tool to one controlplane RBAC action — the single
// declaration site consumed by both enforcement layers (Service.BuildServer's
// construction-time filtering and Service.enforce's call-time middleware)
// plus, later, the docs table. Mirrors spikes/mcp-gateway/internal/tools's
// Entry shape, verified against SDK v1.7.0.
type Entry struct {
	// Name is the MCP tool name as the model sees it.
	Name string
	// Permission is the controlplane RBAC action required to see and call
	// this tool, in the same "<resource>:<verb>" shape RequirePermission uses.
	Permission string
	// Description is mirrored into the mcp.Tool for the model.
	Description string
	// ConnectorType restricts this entry to connectors of exactly this
	// type — checked by Service.BuildServer alongside Permission before an
	// entry is ever registered. The zero value ("") means "every connector
	// type": sapanjai_describe_connector, step 3's connector-agnostic tool,
	// leaves this unset. Google Sheets tools (tools_sheets.go, step 6) set
	// it to connector.TypeGoogleSheets so they never appear against a
	// "generic" or any other non-Sheets connector — there would be nothing
	// for their handler to decrypt into a valid Config, and advertising a
	// tool that can only ever fail is worse than not advertising it.
	ConnectorType connector.Type
	// Register adds the tool to s. svc is the owning *Service — closed over
	// by tools that need it (Google Sheets tools call back into svc to
	// decrypt conn's config fresh on every invocation and to reach the
	// shared OAuth token-source cache); conn is closed over rather than
	// read from any model-supplied argument, see registerDescribeConnector.
	// req carries the per-request HTTP origin (step 9's drive_get_file needs
	// it to mint an absolute download link; every other registrar ignores
	// it with a `_ RequestInfo` parameter) — see RequestInfo's doc comment
	// for why this is threaded explicitly rather than read off ctx.
	Register func(s *gomcp.Server, svc *Service, conn db.Connector, req RequestInfo)
}

// RequestInfo is the subset of the inbound HTTP request a tool's Register
// closure may need but that isn't otherwise reachable from svc/conn — today,
// exactly the origin drive_get_file (tools_drive.go) must build an absolute
// download URL against. This is deliberately passed as an explicit
// parameter rather than smuggled through the tool handler's ctx: the Go MCP
// SDK's context propagation from the inbound *http.Request through to an
// individual tools/call dispatch works today (verified: the SDK's own
// streamable.go comments "Pass req.Context() here, to allow middleware to
// add context values"), but it is not a documented contract this gateway
// should bet a security-relevant URL's correctness on across an SDK
// upgrade. Handler.getServer builds one instance of this per HTTP request
// (via baseURLFromRequest) and BuildServer forwards it to every Entry it
// registers, permitted or not — cheap to compute, so there is no reason to
// build it lazily only for the one tool that uses it.
type RequestInfo struct {
	// BaseURL is the scheme+host the gateway itself was reached on for this
	// request (e.g. "https://api.example.com"), with no trailing slash. See
	// baseURLFromRequest (handler.go) for how it's derived from
	// X-Forwarded-Proto/X-Forwarded-Host/r.Host. Empty in every unit test
	// that builds a *gomcp.Server directly (never through a real HTTP
	// request) — SignFileLink then produces a root-relative
	// "/mcp/files/..." URL, which is never served to a real client outside
	// tests.
	BaseURL string
}

// appliesTo reports whether e should even be considered for conn, before
// any RBAC check: an entry whose ConnectorType is set only ever targets
// that one connector type.
func (e Entry) appliesTo(conn db.Connector) bool {
	return e.ConnectorType == "" || string(e.ConnectorType) == conn.Type
}

// Catalog returns every tool the gateway knows how to expose, permitted or
// not for any given principal. Order is stable so tools/list output is
// deterministic. Step 3 ships exactly one trivial tool, chosen because it
// exercises connector resolution and tenant isolation while being
// structurally incapable of leaking config (see registerDescribeConnector);
// real adapters land in steps 5+.
func Catalog() []Entry {
	return []Entry{
		{
			Name:        "sapanjai_describe_connector",
			Permission:  connector.PermissionRead,
			Description: "Describe the connector this MCP session is bound to: its name, type, and current health status. Never returns connection credentials.",
			Register:    registerDescribeConnector,
		},
		sheetsListSpreadsheetsEntry,
		sheetsDescribeSpreadsheetEntry,
		sheetsQueryRowsEntry,
		sheetsReadRangeEntry,
		driveListFolderEntry,
		driveGetFileEntry,
	}
}

// PermissionFor returns the RBAC action gating tool name, and whether name
// is in the catalog at all.
func PermissionFor(name string) (string, bool) {
	for _, e := range Catalog() {
		if e.Name == name {
			return e.Permission, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// sapanjai_describe_connector
// ---------------------------------------------------------------------------

// describeConnectorOutput is intentionally the *entire* shape a caller can
// ever see through this tool: no config field exists on it, so there is no
// code path — typo, future edit, or otherwise — through which the connector's
// decrypted config could reach the model. Mirrors connector.ConnectorResponse's
// own "must never grow a config field" invariant.
type describeConnectorOutput struct {
	Name   string `json:"name" jsonschema:"the connector's display name"`
	Type   string `json:"type" jsonschema:"the connector's type identifier, e.g. \"generic\""`
	Status string `json:"status" jsonschema:"the connector's last known health status: active, inactive, or error"`
}

func registerDescribeConnector(s *gomcp.Server, _ *Service, conn db.Connector, _ RequestInfo) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "sapanjai_describe_connector",
		Description: "Describe the connector this MCP session is bound to: its name, type, and current health status. Never returns connection credentials.",
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, describeConnectorOutput, error) {
		// conn is closed over from Register's argument, not read from any
		// model-supplied input — the tool takes no arguments at all, so
		// there is no field a model could set to widen or redirect this
		// beyond the connector the session is bound to.
		return nil, describeConnectorOutput{Name: conn.Name, Type: conn.Type, Status: conn.Status}, nil
	})
}
