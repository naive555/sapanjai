package middleware

import (
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/shared/apperror"
)

// ctxPlatformRole is the context key RequirePlatformRole injects, read back
// via PlatformRole.
const ctxPlatformRole = "auth.platformRole"

// RequirePlatformRole guards a route with RequireAuth plus a users.platform_role
// in the allowed set, read fresh from the database on every request, PLUS
// (when ADMIN_REQUIRE_2FA is true, execution plan Task 6.3) a live
// admin:2fa:<userId> Redis key from a completed POST /admin/2fa/verify.
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
	return g.requirePlatformRole(roles, true)
}

// RequirePlatformRoleNo2FA is RequirePlatformRole with the ADMIN_REQUIRE_2FA
// check skipped. Used ONLY by POST /admin/2fa/{enroll,confirm,verify}: the
// chicken-and-egg is that a staff member cannot complete step-up through a
// route step-up itself gates. Every other /admin route goes through
// RequirePlatformRole. The role check and the impersonation refusal are
// identical either way — this is not a weaker guard, just one clause
// narrower.
func (g *Guards) RequirePlatformRoleNo2FA(roles ...string) echo.MiddlewareFunc {
	return g.requirePlatformRole(roles, false)
}

func (g *Guards) requirePlatformRole(roles []string, enforce2FA bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := g.verify(c); err != nil {
				return err
			}

			// An impersonation token never reaches the admin console, and
			// this check is NOT redundant with "the target had no
			// platform_role at issuance". That was true when the token was
			// minted; platform_role is a mutable row, so promoting the
			// impersonated user during the token's 10-minute life would
			// otherwise hand the impersonator a staff session. Refusing on
			// the token's own immutable imp claim closes that window
			// without depending on the target's current role at all.
			if IsImpersonated(c) {
				return echo.NewHTTPError(http.StatusForbidden, "Insufficient permissions")
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
				// cannot be probed for which platform roles exist. This
				// check runs, and can reject, BEFORE the 2FA check below —
				// deliberately: a demoted staff member's platform_role is
				// gone immediately (D1), so a stale-but-unexpired
				// admin:2fa:<userId> key from before the demotion is never
				// even reached. See platform_test.go's demotion test.
				return echo.NewHTTPError(http.StatusForbidden, "Insufficient permissions")
			}

			if enforce2FA && g.adminRequire2FA {
				verified, err := g.blacklist.IsTwoFactorVerified(c.Request().Context(), UserID(c))
				if err != nil {
					return err
				}
				if !verified {
					status, message := apperror.Resolve(apperror.TwoFactorRequired)
					return echo.NewHTTPError(status, message)
				}
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
