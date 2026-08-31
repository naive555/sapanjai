package middleware

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/authtoken"
)

// Context keys for values the guards inject, read back via the typed
// getters below (UserID, UserEmail, OrgID, MembershipFromContext).
const (
	ctxUserID       = "auth.userID"
	ctxUserEmail    = "auth.userEmail"
	ctxOrgID        = "auth.orgID"
	ctxMembership   = "auth.membership"
	ctxActorID      = "auth.actorID"
	ctxImpersonated = "auth.impersonated"
)

// OrgHeader is the request header carrying the active organization for
// org-scoped routes, per docs/02-api-contract.md.
const OrgHeader = "x-organization-id"

// tokenVerifier is the subset of *auth.TokenService the guards depend on.
type tokenVerifier interface {
	VerifyAccessToken(token string) (authtoken.AccessToken, error)
}

// safeMethods are the HTTP methods an impersonation token may use. Anything
// outside this set is refused in verify() — see its doc comment.
var safeMethods = []string{http.MethodGet, http.MethodHead, http.MethodOptions}

// blacklistChecker is the subset of *redis.Auth the guards depend on: the
// access-token blacklist, the ban cache behind Guards.verify's durable ban
// check (see its doc comment), and the step-up 2FA cache RequirePlatformRole
// consults (internal/middleware/platform.go, execution plan Task 6.3).
type blacklistChecker interface {
	IsBlacklisted(ctx context.Context, token string) (bool, error)
	IsBanned(ctx context.Context, userID uuid.UUID) (bool, error)
	IsTwoFactorVerified(ctx context.Context, userID uuid.UUID) (bool, error)
}

// userStore is the subset of *database.Store the guards depend on: org
// membership lookups (RequireOrg/RequirePermission) plus a fresh per-request
// user row (RequirePlatformRole, see platform.go — platform_role is
// deliberately not a JWT claim, so it is re-read from the database on every
// admin request). Named for what it now covers; it started as
// membershipStore before RequirePlatformRole needed GetUserByID.
type userStore interface {
	GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)
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
	store     userStore
	rbac      permissionChecker

	// adminRequire2FA gates RequirePlatformRole's step-up check (execution
	// plan Task 6.3). Set via SetAdminRequire2FA rather than a NewGuards
	// parameter so the five existing call sites across this package's and
	// other modules' tests don't all need updating for a single boolean
	// only server.go's real wiring cares about; it defaults false, which is
	// the safe "2FA not enforced" behavior every test that never calls the
	// setter already assumes.
	adminRequire2FA bool
}

// NewGuards builds a Guards from its narrow dependencies.
func NewGuards(token tokenVerifier, blacklist blacklistChecker, store userStore, rbac permissionChecker) *Guards {
	return &Guards{token: token, blacklist: blacklist, store: store, rbac: rbac}
}

// SetAdminRequire2FA sets whether RequirePlatformRole enforces step-up TOTP
// (ADMIN_REQUIRE_2FA, execution plan Task 6.3). Called once from server.New
// after construction.
func (g *Guards) SetAdminRequire2FA(require bool) {
	g.adminRequire2FA = require
}

// verify reproduces plugin.ts's verifyToken exactly, including check order:
// missing/empty bearer token -> 401 "Unauthorized"; blacklisted (checked
// BEFORE signature verification) -> 401 "Token revoked"; invalid/expired
// signature or missing subject -> 401 "Unauthorized". On success it stores
// the caller's user id and email on the echo.Context.
//
// A banned user is rejected last, with 401 "Account suspended" — 401 rather
// than 403 because the credential is no longer usable and the frontend's
// 401 path already clears the session cleanly (contrast POST /auth/login,
// which returns 403 ACCOUNT_SUSPENDED for the same condition; see
// docs/11-admin-panel.md §4, and do not unify the two). The ban check runs
// AFTER signature verification because it is keyed by user id, which is
// only trustworthy once the signature is verified — checking an unverified
// claim would let a forged-but-unsigned token probe the ban cache. That
// ordering costs a second Redis round trip on every request rather than
// pipelining it with the blacklist read above: the blacklist key is keyed
// by token and can be read before verification, but the ban key needs the
// verified subject, so the two reads cannot be issued in the same
// pipeline without first decoding the subject from an unverified token —
// exactly the kind of contortion this function is documented not to take.
//
// Durability caveat: users.banned_at is the source of truth and
// banned:<userId> in Redis (internal/infra/redis.Auth) is a fast-path
// cache with no TTL. Nothing re-primes the cache from the database on a
// Redis flush except a login attempt (auth.Service.Login). Worst case
// after a flush is that a banned user's already-issued access token keeps
// working until it expires — bounded by JWT_ACCESS_EXPIRES_IN (<=15 min),
// because banning also revokes every session so the refresh path is
// already dead. That bound is why no reaper job exists.
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

	claims, err := g.token.VerifyAccessToken(token)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
	}

	// An impersonation token is read-only, enforced here rather than per
	// route (docs/11-admin-panel.md §5): a route added next year is covered
	// automatically, where an allowlist would silently not cover it. The
	// check sits before the ban lookup only because it needs no I/O — the
	// ordering has no security significance either way.
	if claims.Impersonated && !slices.Contains(safeMethods, c.Request().Method) {
		status, message := apperror.Resolve(apperror.ImpersonationReadOnly)
		return echo.NewHTTPError(status, message)
	}

	banned, err := g.blacklist.IsBanned(c.Request().Context(), claims.UserID)
	if err != nil {
		return err
	}
	if banned {
		return echo.NewHTTPError(http.StatusUnauthorized, "Account suspended")
	}

	c.Set(ctxUserID, claims.UserID)
	c.Set(ctxUserEmail, claims.Email)
	c.Set(ctxImpersonated, claims.Impersonated)
	c.Set(ctxActorID, claims.ActorID)
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

// IsImpersonated reports whether the caller's token was minted by
// POST /admin/users/:userId/impersonate, set by verify().
func IsImpersonated(c echo.Context) bool {
	v, _ := c.Get(ctxImpersonated).(bool)
	return v
}

// ActorID returns the platform-staff user who started an impersonation, set
// by verify(). uuid.Nil on an ordinary token — always pair it with
// IsImpersonated rather than treating a non-nil value as the only signal.
func ActorID(c echo.Context) uuid.UUID {
	v, _ := c.Get(ctxActorID).(uuid.UUID)
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
