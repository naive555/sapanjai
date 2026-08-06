package middleware

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// Context keys for values the guards inject, read back via the typed
// getters below (UserID, UserEmail, OrgID, MembershipFromContext).
const (
	ctxUserID     = "auth.userID"
	ctxUserEmail  = "auth.userEmail"
	ctxOrgID      = "auth.orgID"
	ctxMembership = "auth.membership"
)

// OrgHeader is the request header carrying the active organization for
// org-scoped routes, per docs/02-api-contract.md.
const OrgHeader = "x-organization-id"

// tokenVerifier is the subset of *auth.TokenService the guards depend on.
type tokenVerifier interface {
	VerifyAccessToken(token string) (uuid.UUID, string, error)
}

// blacklistChecker is the subset of *redis.Auth the guards depend on.
type blacklistChecker interface {
	IsBlacklisted(ctx context.Context, token string) (bool, error)
}

// membershipStore is the subset of *database.Store the org guard depends on.
type membershipStore interface {
	GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
}

// permissionChecker is the subset of *rbac.Service the RequirePermission
// guard depends on.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error)
}

// Guards builds the RequireAuth/RequireOrg/RequirePermission middleware,
// replacing the requireAuth/requireOrg/requirePermission Elysia macros in
// the source app's src/modules/auth/plugin.ts.
type Guards struct {
	token     tokenVerifier
	blacklist blacklistChecker
	store     membershipStore
	rbac      permissionChecker
}

// NewGuards builds a Guards from its narrow dependencies.
func NewGuards(token tokenVerifier, blacklist blacklistChecker, store membershipStore, rbac permissionChecker) *Guards {
	return &Guards{token: token, blacklist: blacklist, store: store, rbac: rbac}
}

// verify reproduces plugin.ts's verifyToken exactly, including check order:
// missing/empty bearer token -> 401 "Unauthorized"; blacklisted (checked
// BEFORE signature verification) -> 401 "Token revoked"; invalid/expired
// signature or missing subject -> 401 "Unauthorized". On success it stores
// the caller's user id and email on the echo.Context.
func (g *Guards) verify(c echo.Context) error {
	token := bearerToken(c)
	if token == "" {
		return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
	}

	blacklisted, err := g.blacklist.IsBlacklisted(c.Request().Context(), token)
	if err != nil {
		return err
	}
	if blacklisted {
		return echo.NewHTTPError(http.StatusUnauthorized, "Token revoked")
	}

	userID, email, err := g.token.VerifyAccessToken(token)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
	}

	c.Set(ctxUserID, userID)
	c.Set(ctxUserEmail, email)
	return nil
}

// RequireAuth guards a route with a valid, non-blacklisted access token.
func (g *Guards) RequireAuth() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := g.verify(c); err != nil {
				return err
			}
			return next(c)
		}
	}
}

// RequireOrg guards a route with RequireAuth plus a valid x-organization-id
// header naming an org the caller belongs to. Mirrors plugin.ts requireOrg.
//
// Deviation from source: a lookup failure that isn't "no matching row" (a
// real DB error) is propagated as a 500 rather than folded into the 403
// "Not a member" response — the source's Drizzle findFirst has no such
// distinction (a query error there throws, which its own error handler
// would turn into a 500 anyway), so this preserves the same effective
// behavior rather than masking infra failures as an auth decision.
func (g *Guards) RequireOrg() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := g.verify(c); err != nil {
				return err
			}

			raw := c.Request().Header.Get(OrgHeader)
			if raw == "" {
				return echo.NewHTTPError(http.StatusBadRequest, "Missing x-organization-id header")
			}

			orgID, err := uuid.Parse(raw)
			if err != nil {
				// A malformed id can never match a membership row.
				return echo.NewHTTPError(http.StatusForbidden, "Not a member of this organization")
			}

			membership, err := g.store.GetMembership(c.Request().Context(), db.GetMembershipParams{
				UserID:         UserID(c),
				OrganizationID: orgID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return echo.NewHTTPError(http.StatusForbidden, "Not a member of this organization")
				}
				return err
			}

			c.Set(ctxOrgID, orgID)
			c.Set(ctxMembership, membership)
			return next(c)
		}
	}
}

// RequirePermission guards a route with RequireAuth plus an x-organization-id
// header naming an org the caller has the given RBAC action in. Mirrors
// plugin.ts requirePermission, including check order: the permission is
// checked BEFORE membership is resolved, so a caller who fails the
// permission check (including a non-member, since HasPermission returns
// false for callers with no membership) gets 403 "Missing permission:
// <action>", never "Not a member of this organization". First used in
// production by the /connectors routes (internal/module/connector).
func (g *Guards) RequirePermission(action string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := g.verify(c); err != nil {
				return err
			}

			raw := c.Request().Header.Get(OrgHeader)
			if raw == "" {
				return echo.NewHTTPError(http.StatusBadRequest, "Missing x-organization-id header")
			}

			orgID, err := uuid.Parse(raw)
			if err != nil {
				// A malformed id can never grant a permission.
				return echo.NewHTTPError(http.StatusForbidden, "Missing permission: "+action)
			}

			allowed, err := g.rbac.HasPermission(c.Request().Context(), UserID(c), orgID, action)
			if err != nil {
				return err
			}
			if !allowed {
				return echo.NewHTTPError(http.StatusForbidden, "Missing permission: "+action)
			}

			membership, err := g.store.GetMembership(c.Request().Context(), db.GetMembershipParams{
				UserID:         UserID(c),
				OrganizationID: orgID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return echo.NewHTTPError(http.StatusForbidden, "Not a member of this organization")
				}
				return err
			}

			c.Set(ctxOrgID, orgID)
			c.Set(ctxMembership, membership)
			return next(c)
		}
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, or "" if absent — mirrors
// headers.authorization?.replace("Bearer ", "") in the source app.
func bearerToken(c echo.Context) string {
	const prefix = "Bearer "
	h := c.Request().Header.Get(echo.HeaderAuthorization)
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}

// UserID returns the caller's user id, set by RequireAuth/RequireOrg.
func UserID(c echo.Context) uuid.UUID {
	v, _ := c.Get(ctxUserID).(uuid.UUID)
	return v
}

// UserEmail returns the caller's email from the access token claims, set by
// RequireAuth/RequireOrg. Empty for tokens issued with sub-only claims
// (POST /auth/refresh).
func UserEmail(c echo.Context) string {
	v, _ := c.Get(ctxUserEmail).(string)
	return v
}

// OrgID returns the active organization id, set by RequireOrg.
func OrgID(c echo.Context) uuid.UUID {
	v, _ := c.Get(ctxOrgID).(uuid.UUID)
	return v
}

// MembershipFromContext returns the caller's membership row in the active
// organization, set by RequireOrg.
func MembershipFromContext(c echo.Context) db.Membership {
	v, _ := c.Get(ctxMembership).(db.Membership)
	return v
}
