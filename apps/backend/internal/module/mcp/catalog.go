package mcp

import (
	"context"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/connector"
)

// Entry binds one MCP tool to one controlplane RBAC action — the single
// declaration site consumed by both enforcement layers: BuildServer's
// construction-time filtering and Service.enforce's call-time middleware.
type Entry struct {
	// Name is the MCP tool name as the model sees it.
	Name string
	// Permission is the RBAC action required to see and call this tool, in
	// the same "<resource>:<verb>" shape RequirePermission uses.
	Permission string
	// Description is mirrored into the mcp.Tool for the model.
	Description string
	// ConnectorType restricts this entry to one connector type; "" means
	// every type. A tool whose handler could only ever fail against the
	// wrong type is better left unadvertised than advertised and broken.
	ConnectorType connector.Type
	// Register adds the tool to s. conn is closed over rather than read from
	// any model-supplied argument, so no tool input can redirect a call to
	// another connector.
	Register func(s *gomcp.Server, svc *Service, conn db.Connector, req RequestInfo)
}

// RequestInfo carries the parts of the inbound HTTP request a Register
// closure needs but cannot reach through svc/conn — today just the origin
// drive_get_file builds absolute download URLs against.
//
// Passed explicitly rather than read off the handler's ctx: SDK context
// propagation from the inbound request through to a tools/call dispatch
// works today, but it is not a documented contract worth betting a
// security-relevant URL on across an SDK upgrade.
type RequestInfo struct {
	// BaseURL is the scheme+host the gateway was reached on, no trailing
	// slash — see baseURLFromRequest (handler.go). Empty in unit tests that
	// build a server directly, which makes SignFileLink emit a root-relative
	// URL never served to a real client.
	BaseURL string
}

// appliesTo reports whether e should even be considered for conn, before
// any RBAC check: an entry whose ConnectorType is set only ever targets
// that one connector type.
func (e Entry) appliesTo(conn db.Connector) bool {
	return e.ConnectorType == "" || string(e.ConnectorType) == conn.Type
}

// catalog is the tool set, built once: it is static for the process
// lifetime, and both BuildServer and PermissionFor sit on the per-request
// path. Order is stable so tools/list is deterministic.
var catalog = []Entry{
	{
		Name:        "sapanjai_describe_connector",
		Permission:  connector.PermissionRead,
		Description: describeConnectorDescription,
		Register:    registerDescribeConnector,
	},
	sheetsListSpreadsheetsEntry,
	sheetsDescribeSpreadsheetEntry,
	sheetsQueryRowsEntry,
	sheetsReadRangeEntry,
	driveListFolderEntry,
	driveGetFileEntry,
}

// permissionByTool indexes catalog by tool name, so PermissionFor is a map
// lookup rather than a linear scan per tool of every tools/list response.
var permissionByTool = func() map[string]string {
	m := make(map[string]string, len(catalog))
	for _, e := range catalog {
		m[e.Name] = e.Permission
	}
	return m
}()

// Catalog returns every tool the gateway knows how to expose, permitted or
// not for any given principal.
func Catalog() []Entry {
	return catalog
}

// PermissionFor returns the RBAC action gating tool name, and whether name
// is in the catalog at all.
func PermissionFor(name string) (string, bool) {
	action, ok := permissionByTool[name]
	return action, ok
}

// ---------------------------------------------------------------------------
// sapanjai_describe_connector
// ---------------------------------------------------------------------------

// describeConnectorDescription is shared by the catalog Entry and the
// registered tool, which must advertise the same text.
const describeConnectorDescription = "Describe the connector this MCP session is bound to: its name, type, and current health status. Never returns connection credentials."

// describeConnectorOutput is the entire shape this tool can ever return: it
// has no config field, so no edit short of adding one can leak decrypted
// config to the model. Same invariant as connector.ConnectorResponse.
type describeConnectorOutput struct {
	Name   string `json:"name" jsonschema:"the connector's display name"`
	Type   string `json:"type" jsonschema:"the connector's type identifier, e.g. \"generic\""`
	Status string `json:"status" jsonschema:"the connector's last known health status: active, inactive, or error"`
}

func registerDescribeConnector(s *gomcp.Server, _ *Service, conn db.Connector, _ RequestInfo) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "sapanjai_describe_connector",
		Description: describeConnectorDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, describeConnectorOutput, error) {
		// conn comes from Register, not from input — the tool takes no
		// arguments, so nothing a model sends can redirect it.
		return nil, describeConnectorOutput{Name: conn.Name, Type: conn.Type, Status: conn.Status}, nil
	})
}
