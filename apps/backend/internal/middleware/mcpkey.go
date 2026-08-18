package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// mcpPrincipalCtxKey is the *request context* key RequireMCPKey stores the
// resolved principal under.
//
// This is NOT c.Set(): the MCP SDK's mcp.NewStreamableHTTPHandler takes a
// plain func(r *http.Request) *mcp.Server, which only ever sees
// r.Context() — it has no echo.Context and cannot see anything stashed
// there. RequireMCPKey must therefore end with
// c.SetRequest(c.Request().WithContext(...)) so the value survives into
// that callback. Getting this backwards is the single most likely way to
// lose an hour on this step (docs/07-sheets-adapter-plan.md, step 3).
type mcpPrincipalCtxKey struct{}

// mcpKeyLookup is the subset of *database.Store RequireMCPKey depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface.
type mcpKeyLookup interface {
	GetMCPKeyByHash(ctx context.Context, keyHash string) (db.McpApiKey, error)
	StampMCPKeyLastUsed(ctx context.Context, id uuid.UUID) error
}

// MCPPrincipalResolver resolves an authenticated PAT's (userID,
// organizationID, scopes) into the value that governs the rest of the
// request: the caller's live RBAC grant intersected with the key's own
// scopes (docs/07-sheets-adapter-plan.md Decision 1). The returned value's
// concrete type is always *rbac.Principal.
//
// Declared as a plain function type returning `any` — not a method-set
// interface naming *rbac.Principal or *rbac.Service — so this package need
// not import internal/module/rbac. That import would cycle: every module's
// handler.go (including rbac's own) imports this package for the Guards
// convention (Register(g *echo.Group, guards *appmw.Guards)), so rbac
// already imports middleware, and Go resolves import cycles at
// whole-package granularity, not per file. server.go — which already
// imports both packages — supplies the real implementation by composing
// rbac.Service.Authorize and its Principal.Narrow:
//
//	func(ctx context.Context, userID, organizationID uuid.UUID, scopes []string) (any, error) {
//		p, err := rbacSvc.Authorize(ctx, userID, organizationID)
//		if err != nil {
//			return nil, err
//		}
//		return p.Narrow(scopes), nil
//	}
//
// internal/module/mcp — which safely imports rbac, since rbac does not
// import mcp — recovers the concrete type with a type assertion via
// MCPPrincipalFromContext.
type MCPPrincipalResolver func(ctx context.Context, userID, organizationID uuid.UUID, scopes []string) (any, error)

// RequireMCPKey authenticates an MCP client's bearer PAT (a mcp_api_keys
// row, distinct from the JWT access tokens RequireAuth/RequireOrg/
// RequirePermission handle) and resolves it to a principal via resolve,
// narrowed by the key's own (nullable) scopes.
//
// Rejections are 401 "Unauthorized" for every case a client should treat as
// "this credential doesn't work" — absent/malformed header, unknown hash,
// revoked, expired — deliberately not distinguished from each other in the
// response body (only in the server log) so a probing client learns nothing
// about which reason applied. WWW-Authenticate is set on every 401: this is
// what makes MCP clients (Claude Code, Inspector, ...) offer to
// re-authenticate instead of just dying — see
// spikes/mcp-gateway/cmd/httpsrv/main.go's withAuth, verified against the
// same SDK version this ports.
func RequireMCPKey(store mcpKeyLookup, resolve MCPPrincipalResolver, log *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			token := bearerToken(c)
			if token == "" {
				return mcpUnauthorized(c, "")
			}

			row, err := store.GetMCPKeyByHash(c.Request().Context(), hashMCPToken(token))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return mcpUnauthorized(c, "invalid_token")
				}
				return err
			}
			if row.RevokedAt.Valid {
				return mcpUnauthorized(c, "invalid_token")
			}
			if row.ExpiresAt.Valid && row.ExpiresAt.Time.Before(time.Now()) {
				return mcpUnauthorized(c, "invalid_token")
			}

			principal, err := resolve(c.Request().Context(), row.UserID, row.OrganizationID, row.Scopes)
			if err != nil {
				return err
			}

			// Best-effort bookkeeping, same spirit as auditlog.Service.Record:
			// log and swallow, never fail the request over a stamp that only
			// feeds a dashboard "last used" column.
			if err := store.StampMCPKeyLastUsed(c.Request().Context(), row.ID); err != nil && log != nil {
				log.Warn("failed to stamp mcp key last_used_at", "error", err, "key_id", row.ID)
			}

			ctx := context.WithValue(c.Request().Context(), mcpPrincipalCtxKey{}, principal)
			c.SetRequest(c.Request().WithContext(ctx))
			return next(c)
		}
	}
}

// hashMCPToken must produce byte-identical output to mcpkey.HashToken (hex
// SHA-256), since it hashes the same presented-token value that
// mcpkey.Service.Create hashed at mint time and the two hashes are compared
// via a unique-index lookup. Duplicated here rather than imported: package
// mcpkey's handler.go imports this package (for appmw.OrgID/UserID), so the
// reverse import would be a cycle, exactly as for internal/module/rbac —
// see MCPPrincipalResolver's doc comment. Keep this in sync with
// internal/module/mcpkey/service.go's HashToken if that ever changes.
func hashMCPToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mcpUnauthorized writes the 401 + WWW-Authenticate response RequireMCPKey
// uses for every rejection reason. errParam, when non-empty, is appended as
// the WWW-Authenticate "error" parameter (e.g. "invalid_token") — present
// for a token that was read but rejected, absent when no token was
// presented at all, mirroring the spike's withAuth.
func mcpUnauthorized(c echo.Context, errParam string) error {
	challenge := `Bearer realm="sapanjai"`
	if errParam != "" {
		challenge += `, error="` + errParam + `"`
	}
	c.Response().Header().Set("WWW-Authenticate", challenge)
	return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
}

// MCPPrincipalFromContext returns the principal RequireMCPKey resolved onto
// ctx (dynamic type *rbac.Principal, opaque as `any` at this layer — see
// MCPPrincipalResolver), and whether one was present. Read by
// internal/module/mcp's per-request mcp.Server-construction callback, which
// receives only a *http.Request (never an echo.Context) — see
// mcpPrincipalCtxKey.
func MCPPrincipalFromContext(ctx context.Context) (any, bool) {
	p := ctx.Value(mcpPrincipalCtxKey{})
	return p, p != nil
}
