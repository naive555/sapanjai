package mcp

import (
	"context"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/rbac"
)

// sapanjai_whoami is the second connector-agnostic diagnostic tool
// (docs/08-gateway-core.md §6 Q1): it touches no upstream API and no
// connector-specific state, just like sapanjai_describe_connector, so it is
// gated on connector:read for the same reason that entry is — inventing a
// dedicated whoami:read action nobody seeds would make it invisible to
// every existing key.
var whoamiEntry = Entry{
	Name:        "sapanjai_whoami",
	Permission:  connector.PermissionRead,
	Description: whoamiDescription,
	Register:    registerWhoami,
}

const whoamiDescription = "Report the caller's own organization, the name of the PAT this MCP session " +
	"authenticated with, and its resolved permission list — the actions actually granted after intersecting the " +
	"key's own scopes with its creator's live role. The natural way to check what a scoped key allows without a " +
	"support ticket. Never returns connection credentials, a key id, or a key hash."

// whoamiOutput is the entire shape this tool can ever return: no
// credential, no config, no key id or hash — same invariant as
// describeConnectorOutput.
type whoamiOutput struct {
	OrganizationID string   `json:"organizationId" jsonschema:"the caller's organization id"`
	KeyName        string   `json:"keyName" jsonschema:"the display name of the PAT this session authenticated with"`
	Permissions    []string `json:"permissions" jsonschema:"the caller's resolved permission list; [\"*\"] when an unscoped key rides an owner's live bypass"`
}

func registerWhoami(s *gomcp.Server, _ *Service, p *rbac.Principal, _ db.Connector, req RequestInfo) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "sapanjai_whoami",
		Description: whoamiDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, whoamiOutput, error) {
		// p and req.KeyName are both closed over from Register, not read
		// from any model-supplied argument — this tool takes no input, so
		// nothing a model sends can influence whose identity it reports.
		return nil, whoamiOutput{
			OrganizationID: p.OrganizationID.String(),
			KeyName:        req.KeyName,
			Permissions:    resolvedPermissions(p),
		}, nil
	})
}

// resolvedPermissions renders p's resolved grant the way a caller actually
// experiences it. Role == "owner" is the unscoped-key case: Narrow left the
// bypass intact (docs/08-gateway-core.md §4), so Allows short-circuits true
// for every action regardless of Actions. That is not the same thing as the
// owner literally holding a "*" grant row, but it is what the principal
// actually permits, so ["*"] is the honest rendering rather than an empty
// or misleadingly partial list. Every other principal renders its own
// Actions, with a nil/empty slice rendered as [] — never JSON null.
func resolvedPermissions(p *rbac.Principal) []string {
	if p.Role == "owner" {
		return []string{"*"}
	}
	if len(p.Actions) == 0 {
		return []string{}
	}
	return p.Actions
}
