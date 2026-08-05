# Phase 3 — Org + Auth Guards — Implementation Plan

> **ARCHIVED — completed 2026-07-20.** All 10 execution steps done and verified live against real Postgres 16 + Redis 7 (docker compose), plus a Step 11 full verification pass:
> - sqlc: 3 new query files (`organizations.sql`, `memberships.sql`, `subscriptions.sql`) → 8 new generated methods, all listed in `db.Querier`.
> - `TokenService.VerifyAccessToken`: HS256 access-token verification (subject + email), mirroring `VerifyRefreshToken`.
> - `internal/middleware/auth.go`: `Guards.RequireAuth()`/`RequireOrg()`, reproducing `plugin.ts`'s `verifyToken` exactly — including the blacklist-checked-*before*-signature ordering. Typed context getters (`UserID`, `UserEmail`, `OrgID`, `MembershipFromContext`). Deviation from source (documented in-code): a `GetMembership` lookup failure that isn't "no matching row" propagates as 500 instead of folding into the 403 "Not a member" response.
> - `internal/module/auditlog`: added `ActionOrgCreated` (`org.created`), `ActionOrgMemberInvited` (`org.member.invited`); `org.member.removed` stays unwritten, matching the contract.
> - `internal/module/subscription`: minimal `Service.GetLimit`/`EnforceLimit` (plan `limits` overlaid by `custom_limits`, `-1`/no-subscription = unlimited) — just enough for the invite flow's `max_members` check. The `/subscription` HTTP endpoints are still Phase 4.
> - `internal/module/organization`: `Create` (slug-check + org + owner-membership in one `WithTx`, then best-effort audit), `ListByUser`, `Invite` (role → limit → user-lookup → already-member, matching `OrgService.invite`'s check order), `RemoveMember` (role → target-lookup → cannot-remove-owner, no audit — matches source). Four routes wired to the guards in `server.New`.
> - Validator: registered a custom `orgslug` rule (`^[a-z0-9-]+$`) via `validator.RegisterValidation`.
> - Tests: 10 guard unit tests + 9 org-service unit tests + 8 subscription unit tests + 6 token unit tests (all hand-mocked, no DB) + 4 integration test functions / 24 subtests (incl. an explicit "empty list serializes as `[]`, not `null`" wire-level check) — run against live Postgres+Redis via `docker compose up db redis`, all green with `-count=1`.
> - No bugs found this phase. One false alarm investigated and dismissed: this environment's local `gofmt` binary has a reproducible quirk that wants to mangle a straight `''` into a curly `”` inside one specific comment pattern (`'Bearer ', ''`) — proved via raw byte inspection to be a pre-existing tool artifact (it hits the untouched Phase 2 file `internal/module/auth/handler.go` identically), not a real formatting issue. Left as-is; `go build`/`go vet` are the authoritative checks and both pass clean.
> - `CLAUDE.md` / `README.md` updated: Phase 3 status, and an organizations curl walkthrough (create → list → invite → remove) added to the README quickstart.
> - Confirmed no regression: all Phase 2 auth integration tests still pass unchanged.
>
> Current layout/commands/status are documented in the root `README.md` and `CLAUDE.md`, not this file. Kept here as a historical record only.

> Execution plan for a Sonnet coding session. Follow the steps in order. Each step
> lists the files to touch, the exact code/SQL to add, and how to verify it. Do not
> re-litigate stack decisions (see `CLAUDE.md`). Source of truth for behavior is
> `docs/02-api-contract.md`; the Node original lives at `../controlplane-api/src/modules/organization`.

## Scope

Phase 3 delivers (per `docs/04-migration-plan.md` "Phase 3 — Org + guards"):

1. **Auth guards** as Echo middleware: `RequireAuth` and `RequireOrg`. (`RequirePermission` is Phase 4 — do **not** build it now.)
2. **Organizations module** — four routes:
   - `POST /organizations` (auth) — create org + owner membership in a tx.
   - `GET /organizations` (auth) — caller's memberships with embedded organization.
   - `POST /organizations/invite` (org) — role check + `max_members` enforcement + add member.
   - `DELETE /organizations/members/:userId` (org) — role check + cannot-remove-owner.
3. **Org audit recording** (best-effort): `org.created`, `org.member.invited`.
4. **Minimal subscription limit-enforcer** — `invite` calls `SubscriptionService.enforceLimit(org, "max_members", count)`. Build only `EnforceLimit`/`GetLimit` + the read query now; the `/subscription` HTTP handlers are Phase 4.

### Non-goals (defer to Phase 4)

- RBAC module, `RequirePermission`, `/rbac/*` routes.
- `/subscription` HTTP endpoints (`GET /subscription`, `POST /subscription/assign`).
- `/audit-logs` query endpoint.
- Swagger annotations.

## Ground truth captured from the codebase

- JSON casing is **camelCase** (`createdAt`, `userId`, `organizationId`, `role`). Confirmed against Drizzle schema `../controlplane-api/src/infrastructure/database/schema/{organizations,memberships}.ts`.
- `POST /organizations` returns the **raw org row** `{ id, name, slug, createdAt, updatedAt }` (not the `orgResponse` model).
- `GET /organizations` returns an array of membership objects each with an embedded `organization`:
  `{ id, userId, organizationId, role, createdAt, organization: { id, name, slug, createdAt, updatedAt } }`.
- `invite` and `removeMember` return `{ "success": true }`.
- Membership roles: `owner` | `admin` | `member`. Guard/role checks use the string in the `role` column.
- `sessions.expires_at` / all `timestamp` columns are `timestamp without time zone` → always compare/marshal in **UTC** (see the extensive note in `internal/module/auth/service.go`). Not directly relevant here but keep the convention.
- Existing patterns to mirror exactly:
  - Services return `*apperror.Error` codes; never touch HTTP. Handlers return errors and let `server.newErrorHandler` map them.
  - Narrow interfaces (`authStore`, `loginLimiter`) per service so unit tests hand-mock without the full `db.Querier`. Add compile-time `var _ Iface = (*database.Store)(nil)` assertions.
  - Body binding via `httpx.BindAndValidate` (→ 400 "Invalid request body" / 422 "Validation failed").
  - Multi-step writes via `store.WithTx(ctx, func(q *db.Queries) error {...})`.
  - Audit writes via `auditSvc.Record(ctx, action, &userID, &orgID, metadataJSON)` — best-effort, returns nothing.

## Guard semantics (from `../controlplane-api/src/modules/auth/plugin.ts` + contract)

`verifyToken` order — reproduce EXACTLY:
1. No/empty bearer token → `401 "Unauthorized"`.
2. **Blacklisted (checked before signature verify)** → `401 "Token revoked"`.
3. Invalid/expired signature, or payload missing `sub` → `401 "Unauthorized"`.

`RequireOrg` additionally, after token OK:
4. Missing `x-organization-id` header → `400 "Missing x-organization-id header"`.
5. `x-organization-id` not a member (includes malformed UUID → treat as not-a-member) → `403 "Not a member of this organization"`.

Guards emit these as `echo.NewHTTPError(status, message)`; the existing error handler passes the string message through. Guards inject into `echo.Context`: user id, user email, and (for `RequireOrg`) org id + the `db.Membership` row.

---

## Step 1 — sqlc queries

Create three new query files under `internal/infra/database/queries/`. Follow the style in `queries/sessions.sql`.

### `queries/organizations.sql`
```sql
-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = $1;

-- name: CreateOrganization :one
INSERT INTO organizations (name, slug)
VALUES ($1, $2)
RETURNING *;
```

### `queries/memberships.sql`
```sql
-- name: GetMembership :one
SELECT * FROM memberships
WHERE user_id = $1 AND organization_id = $2;

-- name: CreateMembership :one
INSERT INTO memberships (user_id, organization_id, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: CountMembershipsByOrg :one
SELECT count(*) FROM memberships WHERE organization_id = $1;

-- name: DeleteMembership :exec
DELETE FROM memberships
WHERE user_id = $1 AND organization_id = $2;

-- name: ListMembershipsByUser :many
SELECT
  m.id, m.user_id, m.organization_id, m.role, m.created_at,
  o.id   AS org_id,
  o.name AS org_name,
  o.slug AS org_slug,
  o.created_at AS org_created_at,
  o.updated_at AS org_updated_at
FROM memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.user_id = $1
ORDER BY m.created_at ASC;
```

### `queries/subscriptions.sql`
```sql
-- name: GetOrgSubscriptionWithPlan :one
SELECT s.custom_limits, p.limits AS plan_limits
FROM org_subscriptions s
JOIN plans p ON p.id = s.plan_id
WHERE s.organization_id = $1;
```

## Step 2 — regenerate sqlc

Run `make sqlc` (or `cd apps/backend && sqlc generate`). Verify:
- `internal/infra/database/db/` gains `organizations.sql.go`, `memberships.sql.go`, `subscriptions.sql.go`.
- `db/querier.go` interface grows: `GetOrganizationBySlug`, `CreateOrganization`, `GetMembership`, `CreateMembership`, `CountMembershipsByOrg`, `DeleteMembership`, `ListMembershipsByUser`, `GetOrgSubscriptionWithPlan`.
- Note the generated `GetMembershipParams{UserID, OrganizationID}`, `CreateMembershipParams{UserID, OrganizationID, Role}`, `ListMembershipsByUserRow{...}`, `GetOrgSubscriptionWithPlanRow{CustomLimits []byte, PlanLimits json.RawMessage}` — use these exact names below.

If the `sqlc` CLI is unavailable, install it: `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`. Building the API does not need sqlc, but this step does.

## Step 3 — access-token verification

Add to `internal/module/auth/token.go`:
```go
// VerifyAccessToken parses and validates an access token's HS256 signature
// with the access secret and returns its subject (user id) and embedded
// email. Any parse/signature/expiry failure is returned as an error; the
// auth guard maps it to 401 "Unauthorized".
func (t *TokenService) VerifyAccessToken(tokenString string) (uuid.UUID, string, error) {
	claims := &accessClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return t.accessSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return uuid.Nil, "", err
	}
	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return uuid.Nil, "", errors.New("access token missing subject")
	}
	userID, err := uuid.Parse(sub)
	if err != nil {
		return uuid.Nil, "", err
	}
	return userID, claims.Email, nil
}
```
(`errors` is already imported in that file.)

## Step 4 — auth guards middleware

Create `internal/middleware/auth.go`. Keep it decoupled: depend on narrow interfaces, not concrete infra.

```go
package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/controlplane/backend/internal/infra/database/db"
)

// Context keys for values guards inject.
const (
	ctxUserID     = "auth.userID"
	ctxUserEmail  = "auth.userEmail"
	ctxOrgID      = "auth.orgID"
	ctxMembership = "auth.membership"
)

const orgHeader = "x-organization-id"

type tokenVerifier interface {
	VerifyAccessToken(token string) (uuid.UUID, string, error)
}
type blacklistChecker interface {
	IsBlacklisted(ctx context.Context, token string) (bool, error)
}
type membershipStore interface {
	GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
}

// Guards builds RequireAuth / RequireOrg middleware from its dependencies.
type Guards struct {
	token     tokenVerifier
	blacklist blacklistChecker
	store     membershipStore
}

func NewGuards(token tokenVerifier, blacklist blacklistChecker, store membershipStore) *Guards {
	return &Guards{token: token, blacklist: blacklist, store: store}
}

// verify reproduces plugin.ts verifyToken: empty -> 401 Unauthorized;
// blacklisted (checked before signature) -> 401 Token revoked; bad/expired
// or missing sub -> 401 Unauthorized. On success sets user id + email.
func (g *Guards) verify(c echo.Context) error {
	token := bearer(c)
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

func (g *Guards) RequireOrg() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := g.verify(c); err != nil {
				return err
			}
			rawOrg := c.Request().Header.Get(orgHeader)
			if rawOrg == "" {
				return echo.NewHTTPError(http.StatusBadRequest, "Missing x-organization-id header")
			}
			orgID, err := uuid.Parse(rawOrg)
			if err != nil {
				// malformed id can never match a membership -> not a member
				return echo.NewHTTPError(http.StatusForbidden, "Not a member of this organization")
			}
			m, err := g.store.GetMembership(c.Request().Context(), db.GetMembershipParams{
				UserID:         UserID(c),
				OrganizationID: orgID,
			})
			if err != nil {
				// pgx.ErrNoRows (not a member) OR any db error -> mirror source 403.
				// If you prefer surfacing real db errors as 500, branch on errors.Is(err, pgx.ErrNoRows).
				return echo.NewHTTPError(http.StatusForbidden, "Not a member of this organization")
			}
			c.Set(ctxOrgID, orgID)
			c.Set(ctxMembership, m)
			return next(c)
		}
	}
}

func bearer(c echo.Context) string {
	h := c.Request().Header.Get(echo.HeaderAuthorization)
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

// Typed getters for handlers.
func UserID(c echo.Context) uuid.UUID     { v, _ := c.Get(ctxUserID).(uuid.UUID); return v }
func UserEmail(c echo.Context) string     { v, _ := c.Get(ctxUserEmail).(string); return v }
func OrgID(c echo.Context) uuid.UUID       { v, _ := c.Get(ctxOrgID).(uuid.UUID); return v }
func Membership(c echo.Context) db.Membership { v, _ := c.Get(ctxMembership).(db.Membership); return v }
```

Add compile-time assertions where the concrete types are wired (or in this file with imports):
`var _ tokenVerifier = (*auth.TokenService)(nil)` etc. Simplest: assert in `server.go` after constructing them, or skip — the wiring will fail to compile if the interface is unsatisfied.

**Decision to confirm:** the `GetMembership` error branch. Source treats any lookup miss as 403. A real DB outage would then read as 403 instead of 500. Recommended: branch on `errors.Is(err, pgx.ErrNoRows)` → 403, else return `err` (→ 500). Note this as an intentional deviation if you take it, otherwise keep bug-for-bug 403.

## Step 5 — auditlog action constants

In `internal/module/auditlog/service.go`, extend the action block:
```go
const (
	ActionUserLogin       = "user.login"
	ActionUserRegister    = "user.register"
	ActionOrgCreated      = "org.created"
	ActionOrgMemberInvited = "org.member.invited"
)
```
(`org.member.removed`, `role.created`, `role.assigned` stay unwritten — matches the contract note that only the first four are recorded.)

## Step 6 — subscription limit-enforcer (minimal)

Create `internal/module/subscription/service.go`. Only what `invite` needs; HTTP handlers are Phase 4.

```go
package subscription

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/controlplane/backend/internal/infra/database"
	"github.com/controlplane/backend/internal/infra/database/db"
	"github.com/controlplane/backend/internal/shared/apperror"
)

var _ subStore = (*database.Store)(nil)

type subStore interface {
	GetOrgSubscriptionWithPlan(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error)
}

type Service struct{ store subStore }

func NewService(store subStore) *Service { return &Service{store: store} }

// GetLimit resolves a numeric limit key, plan limits overlaid by custom_limits.
// Returns (nil, nil) when the org has no subscription (== unlimited).
func (s *Service) GetLimit(ctx context.Context, orgID uuid.UUID, key string) (*float64, error) {
	row, err := s.store.GetOrgSubscriptionWithPlan(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	limits := map[string]float64{}
	if len(row.PlanLimits) > 0 {
		_ = json.Unmarshal(row.PlanLimits, &limits)
	}
	if len(row.CustomLimits) > 0 {
		custom := map[string]float64{}
		if json.Unmarshal(row.CustomLimits, &custom) == nil {
			for k, v := range custom {
				limits[k] = v
			}
		}
	}
	if v, ok := limits[key]; ok {
		return &v, nil
	}
	return nil, nil
}

// EnforceLimit returns apperror.LimitExceeded when currentCount >= limit.
// No subscription (nil) or -1 means unlimited. Mirrors SubscriptionService.enforceLimit.
func (s *Service) EnforceLimit(ctx context.Context, orgID uuid.UUID, key string, currentCount int) error {
	limit, err := s.GetLimit(ctx, orgID, key)
	if err != nil {
		return err
	}
	if limit == nil || *limit == -1 {
		return nil
	}
	if float64(currentCount) >= *limit {
		return apperror.New(apperror.LimitExceeded)
	}
	return nil
}
```

## Step 7 — organization module

### `internal/module/organization/dto.go`
```go
package organization

import "time"

type CreateRequest struct {
	Name string `json:"name" validate:"required,min=1"`
	Slug string `json:"slug" validate:"required,min=2,orgslug"`
}

type InviteRequest struct {
	Email string `json:"email" validate:"required,email"`
	Role  string `json:"role" validate:"required,oneof=admin member"`
}

// OrgResponse is the raw org row returned by POST /organizations.
type OrgResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MembershipResponse is one element of GET /organizations.
type MembershipResponse struct {
	ID             string       `json:"id"`
	UserID         string       `json:"userId"`
	OrganizationID string       `json:"organizationId"`
	Role           string       `json:"role"`
	CreatedAt      time.Time    `json:"createdAt"`
	Organization   OrgResponse  `json:"organization"`
}

type SuccessResponse struct {
	Success bool `json:"success"`
}
```

### `internal/module/organization/service.go`

Narrow store interface + service. Signatures:
```go
type orgStore interface {
	GetOrganizationBySlug(ctx, slug string) (db.Organization, error)
	CreateOrganization(ctx, db.CreateOrganizationParams) (db.Organization, error)
	CreateMembership(ctx, db.CreateMembershipParams) (db.Membership, error)
	GetMembership(ctx, db.GetMembershipParams) (db.Membership, error)
	CountMembershipsByOrg(ctx, organizationID uuid.UUID) (int64, error)
	DeleteMembership(ctx, db.DeleteMembershipParams) error
	ListMembershipsByUser(ctx, userID uuid.UUID) ([]db.ListMembershipsByUserRow, error)
	GetUserByEmail(ctx, email string) (db.User, error)
	WithTx(ctx, func(*db.Queries) error) error
}
type limitEnforcer interface {
	EnforceLimit(ctx context.Context, orgID uuid.UUID, key string, currentCount int) error
}
```
`var _ orgStore = (*database.Store)(nil)`, `var _ limitEnforcer = (*subscription.Service)(nil)`.

`Service` holds `store orgStore`, `audit *auditlog.Service`, `limits limitEnforcer`, `log *slog.Logger`. Methods:

- **Create(ctx, userID uuid.UUID, name, slug string) (db.Organization, error)**
  1. Inside `store.WithTx`: `GetOrganizationBySlug(slug)` — if `err == nil` → `apperror.New(apperror.SlugTaken)`; if not `pgx.ErrNoRows` → return err. Then `CreateOrganization({Name, Slug})`, capture into outer var, then `CreateMembership({UserID: userID, OrganizationID: org.ID, Role: "owner"})`.
  2. After commit: `metadata, _ := json.Marshal(map[string]string{"name": org.Name, "slug": org.Slug})`; `s.audit.Record(ctx, auditlog.ActionOrgCreated, &userID, &org.ID, metadata)`.
  3. Return org.

- **ListByUser(ctx, userID uuid.UUID) ([]db.ListMembershipsByUserRow, error)** — just `store.ListMembershipsByUser(userID)`. (Handler maps to DTO.)

- **Invite(ctx, orgID, inviterID uuid.UUID, inviterRole, email, role string) error**
  1. `if inviterRole == "member" → apperror.Forbidden`.
  2. `count, err := store.CountMembershipsByOrg(orgID)`.
  3. `if err := s.limits.EnforceLimit(ctx, orgID, "max_members", int(count)); err != nil { return err }`.
  4. `user, err := store.GetUserByEmail(email)`; `pgx.ErrNoRows → apperror.UserNotFound`.
  5. `_, err := store.GetMembership({UserID: user.ID, OrganizationID: orgID})`; if `err == nil → apperror.AlreadyMember`; if not `pgx.ErrNoRows → return err`.
  6. `store.CreateMembership({UserID: user.ID, OrganizationID: orgID, Role: role})`.
  7. `metadata, _ := json.Marshal(map[string]string{"email": email, "role": role})`; `s.audit.Record(ctx, auditlog.ActionOrgMemberInvited, &inviterID, &orgID, metadata)`.
  8. Return nil.

- **RemoveMember(ctx, orgID uuid.UUID, requesterRole string, targetUserID uuid.UUID) error**
  1. `if requesterRole == "member" → apperror.Forbidden`.
  2. `target, err := store.GetMembership({UserID: targetUserID, OrganizationID: orgID})`; `pgx.ErrNoRows → apperror.MemberNotFound`.
  3. `if target.Role == "owner" → apperror.CannotRemoveOwner`.
  4. `store.DeleteMembership({UserID: targetUserID, OrganizationID: orgID})`.
  5. Return nil. (No audit — matches source.)

Keep the ordering of checks identical to `../controlplane-api/src/modules/organization/service.ts`.

### `internal/module/organization/handler.go`

```go
type Handler struct{ service *Service }
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(g *echo.Group, guards *middleware.Guards) {
	g.POST("", h.create, guards.RequireAuth())
	g.GET("", h.list, guards.RequireAuth())
	g.POST("/invite", h.invite, guards.RequireOrg())
	g.DELETE("/members/:userId", h.removeMember, guards.RequireOrg())
}
```
Handlers:
- `create`: bind `CreateRequest`; `org, err := service.Create(ctx, middleware.UserID(c), req.Name, req.Slug)`; on success `c.JSON(200, toOrgResponse(org))`.
- `list`: `rows, err := service.ListByUser(ctx, middleware.UserID(c))`; map each row → `MembershipResponse` (embed `OrgResponse` from the `Org*` columns); return `c.JSON(200, out)`. Return `[]MembershipResponse{}` (non-nil) when empty so the body is `[]` not `null`.
- `invite`: bind `InviteRequest`; `m := middleware.Membership(c)`; `err := service.Invite(ctx, middleware.OrgID(c), middleware.UserID(c), m.Role, req.Email, req.Role)`; success → `c.JSON(200, SuccessResponse{true})`.
- `removeMember`: `targetID, err := uuid.Parse(c.Param("userId"))` — on parse failure this can't match a member; call `RemoveMember` with a nil/parsed id. Simplest parity: if parse fails return `apperror.New(apperror.MemberNotFound)`. `m := middleware.Membership(c)`; `err := service.RemoveMember(ctx, middleware.OrgID(c), m.Role, targetID)`; success → `SuccessResponse{true}`.

Add small mappers `toOrgResponse(db.Organization) OrgResponse` and `toMembershipResponse(db.ListMembershipsByUserRow) MembershipResponse`.

Response status: source returns 200 (Elysia default) for all of these — use `http.StatusOK`, not 201, to keep parity. (Confirm no test expects 201.)

## Step 8 — register the slug validator

In `internal/server/validator.go`, register the `orgslug` rule used by `CreateRequest.Slug`:
```go
import "regexp"

var orgSlugRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func newRequestValidator() *requestValidator {
	v := validator.New(validator.WithRequiredStructEnabled())
	_ = v.RegisterValidation("orgslug", func(fl validator.FieldLevel) bool {
		return orgSlugRe.MatchString(fl.Field().String())
	})
	return &requestValidator{v: v}
}
```
A slug failing the pattern → `c.Validate` errors → `httpx.BindAndValidate` returns 422 "Validation failed" (matches contract).

## Step 9 — wire everything in `server.New`

In `internal/server/server.go`, after the existing auth wiring, add:
```go
guards := appmw.NewGuards(tokenSvc, redisAuth, store)
subSvc := subscription.NewService(store)
orgSvc := organization.NewService(store, auditSvc, subSvc, log)
organization.NewHandler(orgSvc).Register(e.Group("/organizations"), guards)
```
Add imports for `organization` and `subscription`. `tokenSvc`, `redisAuth`, `store`, `auditSvc`, `log` already exist in `New`.

Note: `e.Group("/organizations")` + route path `""` yields `/organizations` (no trailing slash). Confirm Echo matches `POST /organizations` (it does; trailing-slash requests are a separate concern the source didn't special-case).

## Step 10 — tests

### Unit: `internal/module/organization/service_test.go`
Mirror `internal/module/auth/service_test.go` (hand-mock `orgStore`, `limitEnforcer`; real `auditlog.NewService` with a discard logger, or a no-op mock). Cover:
- Create: slug taken → `SLUG_TAKEN`; success → org returned, `CreateMembership` called with role `owner`, audit `org.created` recorded.
- Invite: inviter `member` → `FORBIDDEN`; `EnforceLimit` returns `LIMIT_EXCEEDED` → propagated; user not found → `USER_NOT_FOUND`; already member → `ALREADY_MEMBER`; happy path → membership created with requested role + audit `org.member.invited`.
- RemoveMember: requester `member` → `FORBIDDEN`; target missing → `MEMBER_NOT_FOUND`; target `owner` → `CANNOT_REMOVE_OWNER`; happy path → `DeleteMembership` called.

Assert `apperror` codes via `errors.As(err, &appErr); appErr.Code == ...`.

### Unit: `internal/middleware/auth_test.go`
Hand-mock `tokenVerifier`, `blacklistChecker`, `membershipStore`. Drive a trivial `next` handler through `RequireAuth()`/`RequireOrg()` with `httptest`. Cover the five guard outcomes in "Guard semantics" above; assert the `*echo.HTTPError` status+message. Confirm blacklist is checked before signature (a blacklisted-but-invalid token → "Token revoked", not "Unauthorized").

### Integration: `internal/server/organization_integration_test.go`
Reuse the `setupTestServer` harness pattern from `internal/server/auth_integration_test.go` (skips without `DATABASE_URL`/`REDIS_URL`; runs goose migrations). Flow with unique uuid-suffixed emails:
1. Register user A → get access token.
2. `POST /organizations` with `{name, slug}` → 200, body has `id/name/slug/createdAt/updatedAt`; slug unique per run.
3. Duplicate slug → 409 "Organization slug already taken".
4. Invalid slug (`Bad_Slug`) → 422 "Validation failed".
5. `GET /organizations` with A's token → array containing the org, embedded `organization` present.
6. No `Authorization` → 401 "Unauthorized".
7. Register user B; `POST /organizations/invite` from A with `x-organization-id` = org, `{email: B, role: "member"}` → 200 `{success:true}`; invite unknown email → 404 "User not found"; invite B again → 409 "User is already a member".
8. `POST /organizations/invite` without `x-organization-id` → 400 "Missing x-organization-id header".
9. Invite with `x-organization-id` = a random uuid A isn't in → 403 "Not a member of this organization".
10. `DELETE /organizations/members/:B` from A → 200; deleting the owner (A) → 403 "Cannot remove organization owner"; deleting a non-member → 404 "Member not found".

(No subscription is assigned in this flow, so `EnforceLimit` returns unlimited and invites succeed — the `LIMIT_EXCEEDED` path is covered by the unit test.)

## Step 11 — verify

```
cd apps/backend
go build ./...
go vet ./...
go test ./...                       # unit tests always run
# integration (optional, needs infra):
make up            # from repo root, starts db + redis
make migrate
DATABASE_URL=... REDIS_URL=... go test ./internal/server/ -run Organization -v
make lint          # golangci-lint
```

## Definition of done

- [ ] `go build ./...` and `go vet ./...` clean.
- [ ] New sqlc code generated and committed; `db/querier.go` lists the eight new methods.
- [ ] `RequireAuth`/`RequireOrg` enforce the five guard outcomes verbatim (statuses + messages).
- [ ] Four `/organizations` routes match `docs/02-api-contract.md`: paths, guards, bodies, 200/`{success:true}`, and every error code (`SLUG_TAKEN`, `USER_NOT_FOUND`, `ALREADY_MEMBER`, `MEMBER_NOT_FOUND`, `CANNOT_REMOVE_OWNER`, `FORBIDDEN`, `LIMIT_EXCEEDED`).
- [ ] `org.created` + `org.member.invited` audit rows written best-effort (never fail the request); remove writes none.
- [ ] Org create + owner membership run in one transaction.
- [ ] Response JSON is camelCase and `GET /organizations` embeds `organization`; empty list serializes as `[]`.
- [ ] Unit tests (org service, guards) pass; integration test passes against real pg+redis.
- [ ] No HTTP concerns leaked into services; services return only `apperror` codes.

## Open decisions to confirm with the owner before/while coding

1. `RequireOrg` DB-error branch: 403-on-any-error (bug-for-bug) vs. `pgx.ErrNoRows`→403 else 500 (recommended). Document as deviation if you take the split.
2. Should `removeMember` write an `org.member.removed` audit row? Source does not; contract lists it as "defined but not written". Recommend: keep unwritten for parity (constant can wait for Phase 4).
3. Add `GET /organizations/members` list endpoint now (frontend needs it — open question #2 in `docs/03`) or defer? Recommend: defer to when the frontend lands; out of Phase 3 scope.
