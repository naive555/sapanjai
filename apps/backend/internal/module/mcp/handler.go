package mcp

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/module/rbac"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// connectorCtxKey is the *request context* key Handler.resolveConnector
// stores the resolved connector row under, read back by Handler.getServer.
// Same reasoning as appmw's principal key: the SDK's per-request server
// callback only ever sees r.Context(), never an echo.Context.
type connectorCtxKey struct{}

// Handler mounts POST /mcp/:connectorId: one Streamable HTTP MCP endpoint,
// shared by every connector, disambiguated per request by the path segment.
type Handler struct {
	service *Service
	mcpHTTP http.Handler
}

// NewHandler builds an mcp Handler, constructing the SDK's
// StreamableHTTPHandler once at server-boot time. Building it once (rather
// than per request) is required for Stateless mode — the handler itself is
// stateless and cheap to reuse; only the *mcp.Server built inside getServer
// is per-request.
func NewHandler(service *Service, log *slog.Logger) *Handler {
	h := &Handler{service: service}
	h.mcpHTTP = gomcp.NewStreamableHTTPHandler(h.getServer, &gomcp.StreamableHTTPOptions{
		// Stateless: no Mcp-Session-Id, no server-side session table, every
		// POST self-contained — auth, build the authorized surface, serve,
		// discard. This is what lets a permission or scope change take
		// effect on the next call instead of the next reconnect, and what
		// keeps the deployment horizontally scalable with no sticky
		// routing. See docs/05-mcp-gateway.md and
		// spikes/mcp-gateway/docs/FINDINGS.md §3.2.
		Stateless: true,
		// Plain application/json responses instead of SSE framing —
		// simpler to curl and to put behind an ordinary load balancer.
		JSONResponse: true,
		Logger:       log,
	})
	return h
}

// getServer is the SDK's per-request server-construction callback
// (func(*http.Request) *mcp.Server). It runs once per HTTP POST in
// Stateless mode, after Register's middleware chain (RequireMCPKey then
// resolveConnector) has already put a principal and a connector on the
// request's context. Both are guaranteed present by the time this runs; a
// missing one is a wiring bug (a route calling this without that chain
// ahead of it), and returning a server with no tools is the safe failure
// rather than a panic that would take the whole process down.
func (h *Handler) getServer(r *http.Request) *gomcp.Server {
	p, ok := principalFromContext(r.Context())
	if !ok {
		return h.service.BuildServer(&rbac.Principal{}, db.Connector{})
	}
	conn, ok := connectorFromContext(r.Context())
	if !ok {
		return h.service.BuildServer(&rbac.Principal{}, db.Connector{})
	}
	return h.service.BuildServer(p, conn)
}

// Register mounts POST /mcp/:connectorId behind mcpKeyGuard (RequireMCPKey)
// and connector resolution, per docs/07-sheets-adapter-plan.md step 3:
// "Mount into Echo with echo.WrapHandler(mcpHandler)." Echo applies
// middleware in the order listed here — mcpKeyGuard runs first (resolves
// and authenticates the PAT), then resolveConnector (reads the principal it
// set, resolves :connectorId scoped to the principal's org), then finally
// the wrapped SDK handler.
func (h *Handler) Register(g *echo.Group, mcpKeyGuard echo.MiddlewareFunc) {
	g.POST("/:connectorId", echo.WrapHandler(h.mcpHTTP), mcpKeyGuard, h.resolveConnector)
}

// resolveConnector is ordinary Echo middleware, not the SDK handler itself.
// It parses :connectorId and resolves it scoped to the authenticated
// principal's organization — never a client-supplied org — before a single
// byte of MCP protocol is processed. A connector belonging to another
// organization is apperror.NotFound, the same code a nonexistent id
// produces, so the id cannot be used to probe for another tenant's
// connectors.
func (h *Handler) resolveConnector(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		connectorID, err := connectorIDParam(c)
		if err != nil {
			return err
		}

		p, ok := principalFromContext(c.Request().Context())
		if !ok {
			// mcpKeyGuard must run ahead of this middleware; reaching here
			// without a principal is a wiring bug, not a client error.
			return apperror.New(apperror.NotFound)
		}

		conn, err := h.service.ResolveConnector(c.Request().Context(), p.OrganizationID, connectorID)
		if err != nil {
			return err
		}

		ctx := context.WithValue(c.Request().Context(), connectorCtxKey{}, conn)
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
}

func connectorFromContext(ctx context.Context) (db.Connector, bool) {
	conn, ok := ctx.Value(connectorCtxKey{}).(db.Connector)
	return conn, ok
}

// principalFromContext recovers the concrete *rbac.Principal that
// appmw.RequireMCPKey put on the request context. appmw.MCPPrincipalFromContext
// hands back `any` deliberately — the middleware package cannot import
// internal/module/rbac without an import cycle (see
// appmw.MCPPrincipalResolver's doc comment) — so this package, which safely
// imports rbac, is where the type assertion happens.
func principalFromContext(ctx context.Context) (*rbac.Principal, bool) {
	v, ok := appmw.MCPPrincipalFromContext(ctx)
	if !ok {
		return nil, false
	}
	p, ok := v.(*rbac.Principal)
	return p, ok
}

// connectorIDParam parses the :connectorId path param. A malformed id can
// never match a connector row, so it resolves to the same NotFound a
// well-formed-but-missing id would.
func connectorIDParam(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("connectorId"))
	if err != nil {
		return uuid.Nil, apperror.New(apperror.NotFound)
	}
	return id, nil
}
