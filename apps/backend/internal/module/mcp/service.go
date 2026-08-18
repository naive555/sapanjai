// Package mcp implements the MCP gateway: one Streamable HTTP endpoint per
// connector (POST /mcp/:connectorId) exposing an RBAC-filtered tool catalog
// to an authenticated PAT holder. This is docs/07-sheets-adapter-plan.md's
// step 3, the risk-retirement step — it proves PAT -> org -> connector ->
// RBAC-filtered tool list -> tools/call -> audit end to end against one
// trivial tool before any Google API code exists.
//
// The pattern is ported from spikes/mcp-gateway (internal/gateway +
// internal/tools), verified there against SDK v1.7.0: never import that
// module, never add it to this build.
package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/rbac"
)

// ServerName and ServerVersion identify this gateway to clients during the
// initialize handshake.
const (
	ServerName    = "sapanjai-mcp-gateway"
	ServerVersion = "0.1.0"
)

// connectorGetter is the subset of *connector.Service this depends on,
// narrowed so unit tests can hand-mock it. Get already does everything this
// package needs for tenant isolation: it scopes by organization_id and
// returns apperror.NotFound for a connector belonging to another org,
// byte-indistinguishable from one that does not exist at all.
type connectorGetter interface {
	Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error)
}

var _ connectorGetter = (*connector.Service)(nil)

// Service builds per-request *mcp.Server instances scoped to one
// authenticated principal and one resolved connector, wiring both
// enforcement layers plus best-effort audit writes.
type Service struct {
	connectors connectorGetter
	audit      *auditlog.Service
	log        *slog.Logger
}

// NewService builds an mcp Service.
func NewService(connectors connectorGetter, audit *auditlog.Service, log *slog.Logger) *Service {
	return &Service{connectors: connectors, audit: audit, log: log}
}

// ResolveConnector fetches connectorID scoped to organizationID — always the
// authenticated principal's own organization (from RequireMCPKey), never a
// value read from the request path or body beyond the id itself. A
// connector belonging to another org is apperror.NotFound, the same code a
// nonexistent id produces.
func (s *Service) ResolveConnector(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	return s.connectors.Get(ctx, organizationID, connectorID)
}

// BuildServer returns an *mcp.Server exposing only the catalog tools p is
// permitted to use against conn. Constructing a fresh server per request is
// cheap (no I/O, no goroutines) and is what makes a per-tenant, per-connector
// tool surface possible in Stateless mode at all.
//
// Two independent enforcement layers, on purpose (see Service.enforce's
// doc comment for why layer 2 is not redundant once permissions can change
// mid-session or a client caches a stale tools/list):
//
//  1. Construction-time filtering, right here: a tool the principal lacks
//     permission for is never registered, so it never appears in
//     tools/list and the SDK reports its own "unknown tool" error if a
//     client calls it anyway.
//  2. Request-time enforcement, Service.enforce, an mcp.Middleware on
//     tools/call and tools/list.
func (s *Service) BuildServer(p *rbac.Principal, conn db.Connector) *gomcp.Server {
	srv := gomcp.NewServer(&gomcp.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, &gomcp.ServerOptions{
		Instructions: "Read-only tools scoped to one Sapanjai connector. The organization and " +
			"connector are fixed by the caller's credentials; there is no tool to select another.",
		Logger: s.log,
	})

	var granted, denied []string
	for _, e := range Catalog() {
		if p.Allows(e.Permission) {
			e.Register(srv, conn)
			granted = append(granted, e.Name)
			continue
		}
		denied = append(denied, e.Name)
	}

	srv.AddReceivingMiddleware(s.enforce(p, conn))

	if s.log != nil {
		s.log.Info("built scoped mcp server",
			"user_id", p.UserID,
			"organization_id", p.OrganizationID,
			"connector_id", conn.ID,
			"tools_granted", granted,
			"tools_denied", denied,
		)
	}

	return srv
}

// enforce is the request-time enforcement layer and the audit hook.
// Redundant against BuildServer's construction-time filtering today, it
// becomes load-bearing the moment permissions can change mid-session, tools
// are registered dynamically, or a client caches a stale tool list — never
// treat tools/list as an authorization boundary (mirrors
// spikes/mcp-gateway/internal/gateway.EnforcePermissions, verified there
// against SDK v1.7.0).
func (s *Service) enforce(p *rbac.Principal, conn db.Connector) gomcp.Middleware {
	return func(next gomcp.MethodHandler) gomcp.MethodHandler {
		return func(ctx context.Context, method string, req gomcp.Request) (gomcp.Result, error) {
			switch method {
			case "initialize", "server/discover":
				// One HTTP POST per JSON-RPC call in Stateless mode, so
				// "session" here means one handshake, not a long-lived
				// connection — the closest analogue this transport has to
				// session-start.
				//
				// Two methods, not one: SDK v1.7.0 clients negotiate up to
				// protocol version 2026-07-28 (SEP-2575) by default, which
				// replaces the classic "initialize" call with
				// "server/discover" whenever the server also advertises
				// that range — true for every server built here, since
				// Stateless mode is required for the new protocol and this
				// gateway is Stateless unconditionally. Verified against
				// this exact SDK build: a plain `gomcp.NewClient` talking
				// to this handler sends server/discover, not initialize —
				// confirmed via internal/server/mcp_integration_test.go's
				// audit assertion, which only passed once this case listed
				// both methods. A pre-2026-07-28 client (older Claude
				// Code/Desktop builds, or a hand-rolled curl request that
				// only names "initialize") still sends the classic method,
				// so both must be handled for "mcp.session.started" to be
				// reliable across client vintages.
				s.auditSessionStarted(ctx, p, conn)

			case "tools/call":
				params, ok := req.GetParams().(*gomcp.CallToolParamsRaw)
				if !ok {
					return next(ctx, method, req)
				}
				action, known := PermissionFor(params.Name)
				if !known {
					// Not ours to judge — let the SDK produce its own
					// "unknown tool" protocol error.
					return next(ctx, method, req)
				}
				if !p.Allows(action) {
					s.auditToolDenied(ctx, p, conn, params.Name, action)
					return PermissionDenied(action), nil
				}
				// Audited before dispatch: this records that a permitted
				// call was attempted, independent of whatever the tool's
				// own handler later returns (success or its own IsError).
				s.auditToolCalled(ctx, p, conn, params.Name)
				return next(ctx, method, req)

			case "tools/list":
				res, err := next(ctx, method, req)
				if err != nil {
					return res, err
				}
				list, ok := res.(*gomcp.ListToolsResult)
				if !ok {
					return res, nil
				}
				kept := make([]*gomcp.Tool, 0, len(list.Tools))
				for _, t := range list.Tools {
					action, known := PermissionFor(t.Name)
					if known && !p.Allows(action) {
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

// auditSessionStarted, auditToolCalled, and auditToolDenied all funnel
// through recordAudit. Every metadata map here is deliberately tiny and
// explicit — connector_id, tool, missing_permission — never the request
// envelope, never a tool argument (which, once real adapters land, could be
// business data such as a spreadsheet cell value or a partner name; see
// docs/07-sheets-adapter-plan.md step 7's "audit metadata carries
// filter_columns[] but never filter values" ground rule, which starts here).

func (s *Service) auditSessionStarted(ctx context.Context, p *rbac.Principal, conn db.Connector) {
	s.recordAudit(ctx, auditlog.ActionMCPSessionStarted, p, map[string]string{
		"connector_id": conn.ID.String(),
	})
}

func (s *Service) auditToolCalled(ctx context.Context, p *rbac.Principal, conn db.Connector, tool string) {
	s.recordAudit(ctx, auditlog.ActionMCPToolCalled, p, map[string]string{
		"connector_id": conn.ID.String(),
		"tool":         tool,
	})
}

func (s *Service) auditToolDenied(ctx context.Context, p *rbac.Principal, conn db.Connector, tool, action string) {
	s.recordAudit(ctx, auditlog.ActionMCPToolDenied, p, map[string]string{
		"connector_id":       conn.ID.String(),
		"tool":               tool,
		"missing_permission": action,
	})
}

// recordAudit is best-effort, exactly like auditlog.Service.Record itself:
// a marshal or write failure is logged and swallowed, never propagated —
// an MCP call must never fail because its own audit trail couldn't be
// written (CLAUDE.md, "Audit-log writes are best-effort").
func (s *Service) recordAudit(ctx context.Context, action string, p *rbac.Principal, metadata map[string]string) {
	if s.audit == nil {
		return
	}
	metaBytes, err := json.Marshal(metadata)
	if err != nil {
		if s.log != nil {
			s.log.Error("failed to marshal mcp audit metadata", "error", err, "action", action)
		}
		return
	}
	userID := p.UserID
	orgID := p.OrganizationID
	s.audit.Record(ctx, action, &userID, &orgID, metaBytes)
}
