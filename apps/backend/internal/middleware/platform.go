package middleware

import (
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// ctxPlatformRole is the context key RequirePlatformRole injects, read back
// via PlatformRole.
const ctxPlatformRole = "auth.platformRole"

// RequirePlatformRole guards a route with RequireAuth plus a users.platform_role
// in the allowed set, read fresh from the database on every request.
//
// The role is deliberately NOT a JWT claim: a claim would keep a demoted or
// revoked staff account privileged for a full access-token lifetime, and the
// same reasoning already governs is_verified (see GET /auth/me). Admin traffic
// is a handful of internal users, so the extra indexed primary-key lookup per
// request is not worth a cached second source of truth.
//
// It injects nothing org-scoped: platform staff typically hold no membership
// anywhere, so RequireOrg's x-organization-id contract does not apply.
func (g *Guards) RequirePlatformRole(roles ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := g.verify(c); err != nil {
				return err
			}

			user, err := g.store.GetUserByID(c.Request().Context(), UserID(c))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// A user id with no row (deleted mid-session) is 401,
					// not 500 — the credential no longer names anyone.
					return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
				}
				return err
			}

			if user.PlatformRole == nil || !slices.Contains(roles, *user.PlatformRole) {
				// Reuse apperror.Forbidden's exact message so the console
				// cannot be probed for which platform roles exist.
				return echo.NewHTTPError(http.StatusForbidden, "Insufficient permissions")
			}

			c.Set(ctxPlatformRole, *user.PlatformRole)
			return next(c)
		}
	}
}

// PlatformRole returns the caller's platform role, set by RequirePlatformRole.
// Empty for any request that didn't go through it.
func PlatformRole(c echo.Context) string {
	v, _ := c.Get(ctxPlatformRole).(string)
	return v
}
