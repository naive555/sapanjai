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

// mcpPrincipalCtxKey keys the resolved principal on the *request* context.
//
// Deliberately not c.Set(): the SDK's server callback takes a plain
// func(*http.Request) and never sees an echo.Context, so RequireMCPKey must
// end with c.SetRequest(c.Request().WithContext(...)) for the value to
// survive into it.
type mcpPrincipalCtxKey struct{}

// mcpKeyNameCtxKey keys the authenticated PAT's display name on the
// *request* context, same rationale as mcpPrincipalCtxKey: the SDK's server
// callback takes a plain func(*http.Request) and never sees an
// echo.Context, so this must ride c.Request().Context() to reach a
// Register closure in internal/module/mcp.
//
// Deliberately not the key id, hash, or token — only the human-readable
// name a Register closure needs to report "this session is using key X".
// Principal's doc comment says Principal must never grow a credential
// field; the same discipline applies to this context value: it carries
// exactly one display string, never a second credential-adjacent field.
type mcpKeyNameCtxKey struct{}

// mcpKeyLookup is the subset of *database.Store RequireMCPKey depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface.
type mcpKeyLookup interface {
	GetMCPKeyByHash(ctx context.Context, keyHash string) (db.McpApiKey, error)
	StampMCPKeyLastUsed(ctx context.Context, id uuid.UUID) error
}

// MCPPrincipalResolver resolves an authenticated PAT into the caller's live
// RBAC grant intersected with the key's own scopes. The concrete type of the
// returned value is always *rbac.Principal.
//
// It returns `any` rather than naming that type because this package cannot
// import internal/module/rbac: every module's handler.go, rbac's included,
// imports middleware for the Guards convention, and Go resolves cycles at
// whole-package granularity. server.go imports both and supplies the real
// implementation; internal/module/mcp asserts the concrete type back.
type MCPPrincipalResolver func(ctx context.Context, userID, organizationID uuid.UUID, scopes []string) (any, error)

// RequireMCPKey authenticates an MCP client's bearer PAT — an mcp_api_keys
// row, not one of the JWTs RequireAuth handles — and resolves it to a
// principal narrowed by the key's own scopes.
//
// Every "this credential doesn't work" case (absent, malformed, unknown,
// revoked, expired) returns an identical 401 so a probing client learns
// nothing about which applied; the reason goes to the log only.
// WWW-Authenticate is set on all of them, which is what makes MCP clients
// offer to re-authenticate rather than simply dying.
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
			ctx = context.WithValue(ctx, mcpKeyNameCtxKey{}, row.Name)
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

// mcpUnauthorized writes the 401 + WWW-Authenticate every rejection uses.
// errParam is set for a token that was read but rejected, empty when none
// was presented at all.
func mcpUnauthorized(c echo.Context, errParam string) error {
	challenge := `Bearer realm="sapanjai"`
	if errParam != "" {
		challenge += `, error="` + errParam + `"`
	}
	c.Response().Header().Set("WWW-Authenticate", challenge)
	return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
}

// MCPPrincipalFromContext returns the principal RequireMCPKey resolved onto
// ctx — dynamic type *rbac.Principal, opaque as `any` here — and whether one
// was present.
func MCPPrincipalFromContext(ctx context.Context) (any, bool) {
	p := ctx.Value(mcpPrincipalCtxKey{})
	return p, p != nil
}

// MCPKeyNameFromContext returns the display name of the mcp_api_keys row
// RequireMCPKey authenticated on ctx, and whether one was present. Absent
// for any request that never went through RequireMCPKey — a unit test
// building a request by hand, or a wiring bug — which callers must treat as
// an empty string, never a panic.
func MCPKeyNameFromContext(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(mcpKeyNameCtxKey{}).(string)
	return name, ok
}
