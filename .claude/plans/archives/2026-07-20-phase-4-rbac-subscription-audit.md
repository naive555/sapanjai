# Phase 4 — RBAC + Subscription + Audit Query — Implementation Plan

> **ARCHIVED — completed 2026-07-20.** All 12 execution steps done and verified live against real Postgres 16 + Redis 7 (already-running docker compose containers):
> - sqlc: 1 new query file (`rbac.sql`, 8 queries) + 2 appended queries in `subscriptions.sql` (`GetOrgSubscription`, `UpsertOrgSubscription`) + 1 appended in `auditlog.sql` (`QueryAuditLogs`) → 11 new generated methods, all listed in `db.Querier`.
> - `internal/module/auditlog`: added `ActionOrgMemberRemoved`/`ActionRoleCreated`/`ActionRoleAssigned` constants (defined, deliberately unwritten — matches contract) and `Service.Query` (filterable by `userId`/`action`, newest-first, capped `limit`), plus `fromPgUUID`/`nonEmptyJSON` helpers for serializing nullable DB columns.
> - `internal/module/rbac` (new): `Service` implementing `CreateRole` (role insert + `setPermissions` wrapped in one `WithTx` — an intentional deviation from the source's non-atomic two-awaits, see `docs/03` "Deviations resolved during Phase 4"), `ListRoles`, `UpdatePermissions`, `AssignRole`, and the `HasPermission` engine (`*` / exact `resource:verb` / `resource:*` wildcard, owner bypass). `Handler` wires the four `/rbac/*` routes, all `RequireOrg`-guarded.
> - `internal/middleware/auth.go`: added `RequirePermission(action)`, the third guard — checks the RBAC permission *before* resolving membership, so a non-member gets `403 "Missing permission: <action>"`, never `"Not a member of this organization"` (mirrors `plugin.ts` exactly). `Guards`/`NewGuards` gained a 4th `permissionChecker` dependency; all 7 pre-existing `NewGuards` call sites (tests + `server.go`) updated. No contract route currently uses this guard — built for parity, exercised only by its 7 dedicated unit tests.
> - `internal/module/subscription`: extended the Phase 3 limit-enforcer `Service` with `GetSubscription` (nil on no-subscription) and `AssignPlan` (upsert; a nonexistent `planId` is left to fail at the FK, 500 — no `PLAN_NOT_FOUND` code exists). Added `Handler` for `GET /subscription` (returns JSON `null` when unset) and `POST /subscription/assign` (returns `{success:true}`, a deviation from the source's empty body).
> - Wired all three new handlers (`/rbac`, `/subscription`, `/audit-logs`) into `server.New`, all `RequireOrg`-guarded.
> - Tests: 17 rbac-service unit tests (incl. the full wildcard-permission matrix: `*`, exact, `resource:*`, non-match, empty set, owner bypass, no-membership denial) + 7 new `RequirePermission` middleware unit tests (incl. the non-member-gets-"Missing permission"-not-"Not a member" parity case) + 4 new subscription-service unit tests, all hand-mocked, no DB — plus a new integration file (`rbac_subscription_audit_integration_test.go`) covering RBAC (create/list/update-permissions/assign + every error code), Subscription (null → assign → embedded plan → re-assign upsert), Audit Logs (both recorded actions, action/userId filters, limit capping incl. 422 on out-of-range), and cross-cutting guard checks. Ran the **entire** suite (unit + integration, all phases) live against real Postgres+Redis with `-count=1`: all green, no regressions.
> - `CreateRole`'s transactional body (and the tx branch of `UpdatePermissions`) is deliberately *not* unit-tested with a hand-mock — `WithTx` hands the closure a concrete `*db.Queries` with an unexported backing field this package can't fake without a real DB, the same reason `organization.Service.Create` (Phase 3) has no unit test either. Coverage for that path comes entirely from the integration test.
> - No bugs found this phase. `golangci-lint` is not installed in this environment (same gap as Phase 3); `go build`/`go vet`/`go test` are the authoritative checks and all pass clean. `gofmt -l` flags many files (including untouched Phase 2 files) due to this Windows checkout's `core.autocrlf=true` with no `.gitattributes` pinning line endings — confirmed via byte-diff to be pure CRLF/LF noise, not real formatting drift; left as-is, matching the "gofmt quirk, not a real issue" call made in the Phase 3 archive.
> - `CLAUDE.md` / `README.md` updated: Phase 4 status, and an RBAC/subscription/audit-logs curl walkthrough added to the README quickstart. `docs/03-target-architecture.md` gained a "Deviations resolved during Phase 4" section recording the three intentional deviations (transactional role-create, uuid body validation → 422, `{success:true}` on assign) with rationale.
>
> Current layout/commands/status are documented in the root `README.md` and `CLAUDE.md`, not this file. Kept here as a historical record only.

> Execution plan for a Sonnet coding session. Follow the steps in order. Each step
> lists the files to touch, the exact code/SQL to add, and how to verify it. Do not
> re-litigate stack decisions (see `CLAUDE.md`). Source of truth for behavior is
> `docs/02-api-contract.md`; the Node originals live at
> `../controlplane-api/src/modules/{rbac,subscription,audit-log}`.
> Phase 3 is complete — its guards (`RequireAuth`/`RequireOrg`), the org module, the
> minimal subscription limit-enforcer, and best-effort audit writes are all live.
> Read `.claude/plans/archives/2026-07-20-phase-3-org-guards.md` for the patterns
> this phase extends.

## Scope

Phase 4 delivers (per `docs/04-migration-plan.md` "Phase 4 — RBAC + subscription + audit query"):

1. **RBAC module** — four org-scoped routes plus the permission engine:
   - `GET  /rbac/roles` — list org's roles, each with its `permissions` array embedded.
   - `POST /rbac/roles` — create role + set its permission set; returns the raw role row.
   - `PUT  /rbac/roles/:roleId/permissions` — replace a role's permission set (role must belong to org).
   - `POST /rbac/assign` — assign a role to a member's membership.
   - `HasPermission(userID, orgID, action)` with `*` / exact `resource:verb` / `resource:*` semantics.
2. **`RequirePermission(action)` middleware** — the third guard, composing token verify + org
   resolution + RBAC check. Built for framework/parity completeness; **no contract route uses it**
   (all Phase 4 routes are `org`-guarded), so it is exercised by unit tests only.
3. **Subscription HTTP endpoints** (`org`):
   - `GET  /subscription` — org's subscription incl. embedded plan (nullable when none).
   - `POST /subscription/assign` — upsert the org's subscription to a plan.
   - Extends the existing `subscription.Service` (`GetLimit`/`EnforceLimit` stay untouched).
4. **Audit-log query endpoint** (`org`):
   - `GET /audit-logs?userId=&action=&limit=` — org's logs, newest first, `limit` 1–100 default 50.

### Non-goals (defer)

- Swagger/OpenAPI annotations (Phase 5).
- Any frontend (Phase 6).
- Writing `role.created` / `role.assigned` audit rows — source defines the constants but does **not**
  write them (same as `org.member.removed`). Add the constants; do **not** wire the writes.
- A `revokeRole` / delete-role endpoint — defined in the source repository layer but not routed. Skip.

## Ground truth captured from the codebase

- JSON casing is **camelCase** everywhere (`organizationId`, `roleId`, `customLimits`, `createdAt`).
  The generated `db.*` structs carry snake_case tags, so every response needs a hand-written
  camelCase DTO — mirror `internal/module/organization/dto.go`.
- **`sqlc.yaml` type mappings** (already configured — do not change): non-null `uuid` →
  `uuid.UUID`; nullable `uuid` → `pgtype.UUID`; non-null `jsonb` → `json.RawMessage`; nullable
  `jsonb` → `[]byte`; nullable `text` → `*string` (`emit_pointers_for_null_types: true`).
  So `roles.description` → `*string`, `audit_logs.{organization_id,user_id}` → `pgtype.UUID`,
  `audit_logs.metadata` / `org_subscriptions.custom_limits` → `[]byte`, `plans.limits` →
  `json.RawMessage`. Reuse `auditlog.toPgUUID` for `pgtype.UUID` ↔ `*uuid.UUID`.
- Response shapes verified against the Drizzle originals:
  - `GET /rbac/roles` → array of `{ id, organizationId, name, description, createdAt, permissions: [{ id, roleId, action, createdAt }] }`.
  - `POST /rbac/roles` → the **raw role row only** (`createRole` returns before `setPermissions`); **no `permissions` key**. Use a distinct struct without that field.
  - `PUT .../permissions`, `POST /rbac/assign`, → `{ "success": true }`.
  - `GET /subscription` → `{ id, organizationId, planId, customLimits, createdAt, updatedAt, plan: { id, name, limits, createdAt } }`, or `null` when the org has no subscription.
  - `GET /audit-logs` → array of `{ id, organizationId, userId, action, metadata, createdAt }`.
- Existing patterns to mirror exactly (see Phase 3 plan §"Ground truth"):
  - Services return `*apperror.Error` codes, never touch HTTP; narrow per-service store interfaces
    with `var _ Iface = (*database.Store)(nil)` assertions; hand-mocked unit tests.
  - Body binding via `httpx.BindAndValidate`; multi-step writes via `store.WithTx`.
  - All Phase 4 apperror codes already exist in `internal/shared/apperror/apperror.go`
    (`ROLE_NOT_FOUND`, `FORBIDDEN`, `MEMBER_NOT_FOUND`) — **no new codes needed**.

## Permission semantics (from `../controlplane-api/src/modules/rbac/service.ts` + contract)

`getUserPermissions(userId, orgId)` → `hasPermission(action)`, reproduce EXACTLY:
1. Resolve the caller's membership in the org. **No membership → `[]` (no permissions).**
2. Membership `role == "owner"` → return `["*"]` (owner bypass — never touches the roles tables).
3. Otherwise → the distinct set of `permissions.action` reachable via
   `memberships → member_roles → roles → permissions`.

`hasPermission(action)` returns true iff, over that set:
- it contains `"*"`, **or**
- it contains the exact `action`, **or**
- it contains `"<resource>:*"` where `resource = strings.SplitN(action, ":", 2)[0]`.

`RequirePermission(action)` guard order (from `plugin.ts` `requirePermission`): token verify → require
`x-organization-id` (400 if missing) → `HasPermission` → if false `403 "Missing permission: <action>"`
→ then resolve membership into context. **Parity note:** the source checks the permission *before*
membership, so a **non-member** hitting a perm-guarded route gets `403 "Missing permission: <action>"`,
**not** `403 "Not a member of this organization"`. No contract route is perm-guarded, so this only
shows up in the middleware unit test — encode it there.

---

## Step 1 — sqlc queries

Add to the existing query files under `internal/infra/database/queries/`. Follow the style already
in `memberships.sql` / `subscriptions.sql`.

### New file `queries/rbac.sql`
```sql
-- name: CreateRole :one
INSERT INTO roles (organization_id, name, description)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRoleByID :one
SELECT * FROM roles WHERE id = $1;

-- name: ListRolesByOrg :many
SELECT * FROM roles
WHERE organization_id = $1
ORDER BY created_at ASC;

-- name: DeletePermissionsByRole :exec
DELETE FROM permissions WHERE role_id = $1;

-- name: CreatePermission :exec
INSERT INTO permissions (role_id, action)
VALUES ($1, $2);

-- name: ListPermissionsByRoleIDs :many
SELECT * FROM permissions
WHERE role_id = ANY($1::uuid[])
ORDER BY created_at ASC;

-- name: AssignMemberRole :exec
INSERT INTO member_roles (membership_id, role_id)
VALUES ($1, $2)
ON CONFLICT (membership_id, role_id) DO NOTHING;

-- name: ListPermissionActionsByUserOrg :many
SELECT DISTINCT p.action
FROM memberships m
JOIN member_roles mr ON mr.membership_id = m.id
JOIN roles r         ON r.id = mr.role_id
JOIN permissions p   ON p.role_id = r.id
WHERE m.user_id = $1 AND m.organization_id = $2;
```

### Append to `queries/subscriptions.sql`
```sql
-- name: GetOrgSubscription :one
SELECT
  s.id, s.organization_id, s.plan_id, s.custom_limits, s.created_at, s.updated_at,
  p.id         AS plan_pid,
  p.name       AS plan_name,
  p.limits     AS plan_plimits,
  p.created_at AS plan_created_at
FROM org_subscriptions s
JOIN plans p ON p.id = s.plan_id
WHERE s.organization_id = $1;

-- name: UpsertOrgSubscription :exec
INSERT INTO org_subscriptions (organization_id, plan_id)
VALUES ($1, $2)
ON CONFLICT (organization_id)
DO UPDATE SET plan_id = EXCLUDED.plan_id, updated_at = now();
```
> Aliases avoid colliding with `s.plan_id`/`s.created_at`. `plan_plimits` (non-null jsonb) →
> `json.RawMessage`; `custom_limits` (nullable jsonb) → `[]byte`.

### Append to `queries/auditlog.sql`
```sql
-- name: QueryAuditLogs :many
SELECT * FROM audit_logs
WHERE organization_id = sqlc.arg('organization_id')
  AND (sqlc.narg('user_id')::uuid IS NULL OR user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action'))
ORDER BY created_at DESC
LIMIT sqlc.arg('lim');
```
> Generates `QueryAuditLogsParams{ OrganizationID uuid.UUID, UserID pgtype.UUID, Action *string, Lim int32 }`
> — `narg` fields are nullable so an absent filter is passed as the zero `pgtype.UUID{}` / `nil`.

## Step 2 — regenerate sqlc

Run `make sqlc` (or `cd apps/backend && sqlc generate`). Install if missing:
`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`. Verify:
- `db/rbac.sql.go` created; `subscriptions.sql.go` / `auditlog.sql.go` regenerated.
- `db/querier.go` gains: `CreateRole`, `GetRoleByID`, `ListRolesByOrg`, `DeletePermissionsByRole`,
  `CreatePermission`, `ListPermissionsByRoleIDs`, `AssignMemberRole`, `ListPermissionActionsByUserOrg`,
  `GetOrgSubscription`, `UpsertOrgSubscription`, `QueryAuditLogs`.
- Note the generated param/row struct names — use them verbatim below
  (`CreateRoleParams`, `ListPermissionsByRoleIDsRow` = `db.Permission`, `GetOrgSubscriptionRow`, `QueryAuditLogsParams`).

## Step 3 — auditlog: query method + role action constants

In `internal/module/auditlog/service.go`:

- Extend the action block (constants only — **not written**, matching source):
```go
const (
	ActionUserLogin        = "user.login"
	ActionUserRegister     = "user.register"
	ActionOrgCreated       = "org.created"
	ActionOrgMemberInvited = "org.member.invited"
	ActionOrgMemberRemoved = "org.member.removed" // defined, unwritten
	ActionRoleCreated      = "role.created"        // defined, unwritten
	ActionRoleAssigned     = "role.assigned"       // defined, unwritten
)
```
- Add a query method (reads, so it *does* return an error, unlike `Record`):
```go
// Query returns organizationID's audit logs newest-first, optionally filtered
// by userID and/or action, capped at limit rows. Mirrors AuditLogService.query.
func (s *Service) Query(ctx context.Context, organizationID uuid.UUID, userID *uuid.UUID, action *string, limit int32) ([]db.AuditLog, error) {
	return s.q.QueryAuditLogs(ctx, db.QueryAuditLogsParams{
		OrganizationID: organizationID,
		UserID:         toPgUUID(userID),
		Action:         action,
		Lim:            limit,
	})
}
```
> `s.q` is already `db.Querier`. `toPgUUID` already exists in this file.

## Step 4 — RBAC service

Create `internal/module/rbac/service.go`. Narrow store interface + service, mirroring the org module.

```go
type rbacStore interface {
	CreateRole(ctx context.Context, arg db.CreateRoleParams) (db.Role, error)
	GetRoleByID(ctx context.Context, id uuid.UUID) (db.Role, error)
	ListRolesByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.Role, error)
	ListPermissionsByRoleIDs(ctx context.Context, roleIds []uuid.UUID) ([]db.Permission, error)
	GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
	AssignMemberRole(ctx context.Context, arg db.AssignMemberRoleParams) error
	ListPermissionActionsByUserOrg(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error)
	// setPermissions runs delete+inserts atomically:
	WithTx(ctx context.Context, fn func(q *db.Queries) error) error
}
var _ rbacStore = (*database.Store)(nil)
```

`Service{ store rbacStore }`, `NewService(store) *Service`. Methods (keep check order identical to
`service.ts`):

- **CreateRole(ctx, orgID uuid.UUID, name string, description *string, permissions []string) (db.Role, error)**
  Wrap in `store.WithTx` (deviation from source's non-transactional two-awaits — see decision #1):
  `role, err := q.CreateRole({OrganizationID: orgID, Name: name, Description: description})`, then
  `setPermissions(ctx, q, role.ID, permissions)`. Return `role`.
- **setPermissions(ctx, q *db.Queries, roleID uuid.UUID, actions []string) error** — helper:
  `q.DeletePermissionsByRole(roleID)`; then `for _, a := range actions { q.CreatePermission({RoleID: roleID, Action: a}) }`.
  (No dedupe — a duplicate action violates the `(role_id, action)` unique constraint and surfaces as
  500, matching the source's drizzle insert. See decision #3.)
- **ListRoles(ctx, orgID uuid.UUID) ([]RoleWithPermissions, error)** — `ListRolesByOrg`; collect
  `role.ID`s; `ListPermissionsByRoleIDs(ids)`; group permissions into a `map[uuid.UUID][]db.Permission`;
  return roles each paired with their (possibly empty, non-nil) permission slice. Return an
  empty non-nil slice when there are no roles. (Define a small `RoleWithPermissions{ Role db.Role; Permissions []db.Permission }` carrier, or map straight to the DTO in the handler — either is fine; keep HTTP out of the service if you return the carrier.)
- **UpdatePermissions(ctx, roleID, orgID uuid.UUID, permissions []string) error**
  `role, err := GetRoleByID(roleID)`; `pgx.ErrNoRows → apperror.RoleNotFound`; other err → return.
  `if role.OrganizationID != orgID → apperror.Forbidden`. Then `store.WithTx` → `setPermissions`.
- **AssignRole(ctx, orgID uuid.UUID, userID, roleID uuid.UUID) error**
  `role, err := GetRoleByID(roleID)`; `ErrNoRows → RoleNotFound`. `if role.OrganizationID != orgID → Forbidden`.
  `m, err := GetMembership({UserID: userID, OrganizationID: orgID})`; `ErrNoRows → MemberNotFound`.
  `AssignMemberRole({MembershipID: m.ID, RoleID: roleID})`.
- **HasPermission(ctx, userID, orgID uuid.UUID, action string) (bool, error)** — the engine:
  ```go
  m, err := s.store.GetMembership(ctx, db.GetMembershipParams{UserID: userID, OrganizationID: orgID})
  if errors.Is(err, pgx.ErrNoRows) { return false, nil }
  if err != nil { return false, err }
  if m.Role == "owner" { return true, nil }
  actions, err := s.store.ListPermissionActionsByUserOrg(ctx, db.ListPermissionActionsByUserOrgParams{UserID: userID, OrganizationID: orgID})
  if err != nil { return false, err }
  for _, a := range actions { if a == "*" || a == action { return true, nil } }
  resource := action
  if i := strings.IndexByte(action, ':'); i >= 0 { resource = action[:i] }
  wildcard := resource + ":*"
  for _, a := range actions { if a == wildcard { return true, nil } }
  return false, nil
  ```

## Step 5 — `RequirePermission` middleware

Extend `internal/middleware/auth.go`. Add a narrow checker interface + field on `Guards`:
```go
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, orgID uuid.UUID, action string) (bool, error)
}
```
Change `Guards` to hold `rbac permissionChecker` and update `NewGuards(token, blacklist, store, rbac)`.
(Update the one call site in `server.New` — Step 10.) Add:
```go
// RequirePermission guards a route with RequireAuth + x-organization-id + an
// RBAC permission check. Mirrors plugin.ts requirePermission: the permission is
// checked BEFORE membership resolution, so a non-member is rejected with
// "Missing permission: <action>", not "Not a member of this organization".
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
				return echo.NewHTTPError(http.StatusForbidden, "Missing permission: "+action)
			}
			allowed, err := g.rbac.HasPermission(c.Request().Context(), UserID(c), orgID, action)
			if err != nil {
				return err
			}
			if !allowed {
				return echo.NewHTTPError(http.StatusForbidden, "Missing permission: "+action)
			}
			membership, err := g.store.GetMembership(c.Request().Context(), db.GetMembershipParams{UserID: UserID(c), OrganizationID: orgID})
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
```
> Add `errors` + `pgx` imports if the current file lacks them (it already imports both for `RequireOrg`).

## Step 6 — subscription service extension

Extend `internal/module/subscription/service.go` — **do not touch** `GetLimit`/`EnforceLimit`.
Widen `subStore`:
```go
type subStore interface {
	GetOrgSubscriptionWithPlan(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error)
	GetOrgSubscription(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error)
	UpsertOrgSubscription(ctx context.Context, arg db.UpsertOrgSubscriptionParams) error
}
```
Add:
```go
// GetSubscription returns the org's subscription with its plan, or (zero, nil)
// when the org has no subscription — the handler serializes that as JSON null.
func (s *Service) GetSubscription(ctx context.Context, organizationID uuid.UUID) (*db.GetOrgSubscriptionRow, error) {
	row, err := s.store.GetOrgSubscription(ctx, organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// AssignPlan upserts the org's subscription to planID. Mirrors assignPlan.
func (s *Service) AssignPlan(ctx context.Context, organizationID, planID uuid.UUID) error {
	return s.store.UpsertOrgSubscription(ctx, db.UpsertOrgSubscriptionParams{OrganizationID: organizationID, PlanID: planID})
}
```

## Step 7 — RBAC handler + DTOs

### `internal/module/rbac/dto.go`
```go
type CreateRoleRequest struct {
	Name        string   `json:"name" validate:"required,min=1"`
	Description *string  `json:"description"`
	Permissions []string `json:"permissions" validate:"omitempty,dive,min=1"`
}
type UpdatePermissionsRequest struct {
	Permissions []string `json:"permissions" validate:"omitempty,dive,min=1"`
}
type AssignRoleRequest struct {
	UserID string `json:"userId" validate:"required,uuid"`
	RoleID string `json:"roleId" validate:"required,uuid"`
}

// RoleRowResponse is POST /rbac/roles — the raw role row, NO permissions key.
type RoleRowResponse struct {
	ID             uuid.UUID `json:"id"`
	OrganizationID uuid.UUID `json:"organizationId"`
	Name           string    `json:"name"`
	Description    *string   `json:"description"`
	CreatedAt      time.Time `json:"createdAt"`
}
// RoleResponse is one element of GET /rbac/roles — role + embedded permissions.
type RoleResponse struct {
	ID             uuid.UUID            `json:"id"`
	OrganizationID uuid.UUID            `json:"organizationId"`
	Name           string               `json:"name"`
	Description    *string              `json:"description"`
	CreatedAt      time.Time            `json:"createdAt"`
	Permissions    []PermissionResponse `json:"permissions"`
}
type PermissionResponse struct {
	ID        uuid.UUID `json:"id"`
	RoleID    uuid.UUID `json:"roleId"`
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"createdAt"`
}
type SuccessResponse struct {
	Success bool `json:"success"`
}
```
> `validate:"uuid"` on `userId`/`roleId` means a malformed id → 422 "Validation failed" (clean
> deviation from source, which would 500 on a bad-uuid DB query — see decision #2). Parse the
> validated strings with `uuid.Parse` in the handler (error impossible after validation, but check).

### `internal/module/rbac/handler.go`
```go
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.GET("/roles", h.listRoles, guards.RequireOrg())
	g.POST("/roles", h.createRole, guards.RequireOrg())
	g.PUT("/roles/:roleId/permissions", h.updatePermissions, guards.RequireOrg())
	g.POST("/assign", h.assignRole, guards.RequireOrg())
}
```
- `listRoles`: `service.ListRoles(ctx, appmw.OrgID(c))`; map to `[]RoleResponse` (non-nil, so empty → `[]`); each role's `Permissions` non-nil (`[]PermissionResponse{}` when none). 200.
- `createRole`: bind `CreateRoleRequest`; `role, err := service.CreateRole(ctx, appmw.OrgID(c), req.Name, req.Description, req.Permissions)`; 200 `toRoleRowResponse(role)`.
- `updatePermissions`: `roleID, err := uuid.Parse(c.Param("roleId"))` — parse fail → `apperror.New(apperror.RoleNotFound)` (can't match). Bind `UpdatePermissionsRequest`; `service.UpdatePermissions(ctx, roleID, appmw.OrgID(c), req.Permissions)`; 200 `SuccessResponse{true}`.
- `assignRole`: bind `AssignRoleRequest`; parse both uuids; `service.AssignRole(ctx, appmw.OrgID(c), userID, roleID)`; 200 `SuccessResponse{true}`.

## Step 8 — subscription handler + DTOs

### `internal/module/subscription/dto.go`
```go
type AssignRequest struct {
	PlanID string `json:"planId" validate:"required,uuid"`
}
type PlanResponse struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Limits    json.RawMessage `json:"limits"`
	CreatedAt time.Time       `json:"createdAt"`
}
type SubscriptionResponse struct {
	ID             uuid.UUID       `json:"id"`
	OrganizationID uuid.UUID       `json:"organizationId"`
	PlanID         uuid.UUID       `json:"planId"`
	CustomLimits   json.RawMessage `json:"customLimits"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	Plan           PlanResponse    `json:"plan"`
}
type SuccessResponse struct {
	Success bool `json:"success"`
}
```
> `CustomLimits`: the row's `custom_limits` is `[]byte`; assign directly, but coerce empty/`nil` to
> `nil` so it marshals as JSON `null` (a nil `json.RawMessage` → `null`; a zero-length non-nil slice
> panics `json.Marshal`). i.e. `if len(row.CustomLimits) > 0 { out.CustomLimits = row.CustomLimits }`.

### `internal/module/subscription/handler.go`
```go
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.GET("", h.get, guards.RequireOrg())
	g.POST("/assign", h.assign, guards.RequireOrg())
}
```
- `get`: `sub, err := h.service.GetSubscription(ctx, appmw.OrgID(c))`; if `sub == nil` → `c.JSON(200, nil)` (serializes as `null`, matching contract's "nullable"); else map to `SubscriptionResponse` and 200.
- `assign`: bind `AssignRequest`; `planID, _ := uuid.Parse(req.PlanID)`; `h.service.AssignPlan(ctx, appmw.OrgID(c), planID)`; 200 `SuccessResponse{true}` (see decision #4 — source returns an empty body; we return `{success:true}` for consistency with the rest of the API).
> A `NewHandler(service)` constructor + `NewService` change: `subscription.NewService` already exists;
> it just gains the two methods. `Handler` is new.

## Step 9 — audit-log handler + DTOs

### `internal/module/auditlog/dto.go` (new; keep it out of `service.go`)
```go
type QueryParams struct {
	UserID *string `query:"userId" validate:"omitempty,uuid"`
	Action *string `query:"action"`
	Limit  *int    `query:"limit" validate:"omitempty,min=1,max=100"`
}
type LogResponse struct {
	ID             uuid.UUID       `json:"id"`
	OrganizationID *uuid.UUID      `json:"organizationId"`
	UserID         *uuid.UUID      `json:"userId"`
	Action         string          `json:"action"`
	Metadata       json.RawMessage `json:"metadata"`
	CreatedAt      time.Time       `json:"createdAt"`
}
```
> `OrganizationID`/`UserID` come from `pgtype.UUID`; convert with a helper (`if v.Valid { u := uuid.UUID(v.Bytes); out.UserID = &u }`). `Metadata` from `[]byte`: coerce empty → `nil` (→ `null`).

### `internal/module/auditlog/handler.go` (new)
```go
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.GET("", h.query, guards.RequireOrg())
}
```
- `query`: use `httpx.BindAndValidate(c, &q)` — Echo's default binder reads **query params** for GET
  (verified), so this works. Caveat: an out-of-range `limit` (e.g. `0`/`101`) fails `validate` → 422
  "Validation failed" (correct), but a *non-numeric* `limit=abc` fails `c.Bind`'s type conversion →
  400 "Invalid request body" (not 422). The contract only specifies the 1–100 range, so this is
  acceptable; note it if the owner wants 422 for all bad `limit` values (would need manual parsing).
- Convert: `var userID *uuid.UUID` from `q.UserID` (parse — safe, validated); `limit := int32(50); if q.Limit != nil { limit = int32(*q.Limit) }`.
- `logs, err := h.service.Query(ctx, appmw.OrgID(c), userID, q.Action, limit)`; map to `[]LogResponse` (non-nil → `[]`); 200.
> `auditlog.Service` needs a `Handler` wrapper. The service is currently constructed with a
> `db.Querier` (the `*database.Store`) — that satisfies the new `QueryAuditLogs`. `NewHandler(service *Service)`.

## Step 10 — wire everything in `server.New`

In `internal/server/server.go`, after the org wiring, extend. Because `Guards` now needs the RBAC
service, construct `rbacSvc` **before** `guards`:
```go
rbacSvc := rbac.NewService(store)
guards := appmw.NewGuards(tokenSvc, redisAuth, store, rbacSvc) // <-- new 4th arg

subSvc := subscription.NewService(store)
orgSvc := organization.NewService(store, auditSvc, subSvc)
organization.NewHandler(orgSvc).Register(e.Group("/organizations"), guards)

rbac.NewHandler(rbacSvc).Register(e.Group("/rbac"), guards)
subscription.NewHandler(subSvc).Register(e.Group("/subscription"), guards)
auditlog.NewHandler(auditSvc).Register(e.Group("/audit-logs"), guards)
```
Add imports for `rbac` (and `auditlog`/`subscription` are already imported). Update the existing
`appmw.NewGuards(...)` call to pass `rbacSvc`. Note the updated doc comment on `New` — RBAC is no
longer "lands in Phase 4", it's here.

## Step 11 — tests

### Unit: `internal/module/rbac/service_test.go`
Hand-mock `rbacStore`. Cover:
- `HasPermission`: owner → true without touching permissions; `"*"` grants anything; exact match;
  `"project:*"` matches `"project:create"` but **not** `"billing:read"`; no membership → false;
  empty permission set → false. (This is the highest-value test — the wildcard logic is the crux.)
- `CreateRole`: role returned; `DeletePermissionsByRole` + one `CreatePermission` per action called
  inside the tx (assert via a mock that records calls). Empty permissions → delete called, no inserts.
- `UpdatePermissions`: role missing → `ROLE_NOT_FOUND`; role in another org → `FORBIDDEN`; happy path replaces.
- `AssignRole`: role missing → `ROLE_NOT_FOUND`; wrong org → `FORBIDDEN`; membership missing → `MEMBER_NOT_FOUND`; happy path calls `AssignMemberRole`.
- `ListRoles`: permissions grouped onto the right roles; role with no permissions → non-nil empty slice.

For `WithTx` in the mock, run `fn` against a mock `*db.Queries`... note `WithTx` takes `func(*db.Queries)`,
which is concrete — the org service's test already solves this; **copy that harness** from
`internal/module/organization/service_test.go` (it hand-mocks `WithTx` by invoking the closure against
a stub). Match whatever it does.

### Unit: `internal/module/subscription/service_test.go`
Already exists for `GetLimit`/`EnforceLimit` — extend it (widen the mock store with the two new
methods). Add: `GetSubscription` returns nil on `pgx.ErrNoRows`; returns the row otherwise;
`AssignPlan` forwards to `UpsertOrgSubscription`.

### Unit: `internal/middleware/auth_test.go`
Extend the existing guard tests. Add a `permissionChecker` mock to the harness (`NewGuards` now takes
4 args). Cover `RequirePermission`:
- allowed → `next` runs, membership in context.
- not allowed → `403 "Missing permission: <action>"`.
- **non-member (HasPermission false) → `403 "Missing permission: <action>"`, not "Not a member"** (the parity subtlety).
- missing `x-organization-id` → `400`.
- checker returns error → propagated (500 via handler).

### Integration: `internal/server/rbac_subscription_audit_integration_test.go`
Reuse the `setupTestServer` harness from `internal/server/organization_integration_test.go` (skips
without `DATABASE_URL`/`REDIS_URL`; runs goose migrations; seed plans if the subscription flow needs a
real plan row — insert one directly, or call `make seed`). Flow (unique uuid-suffixed emails/slugs):
1. Register owner A; create org O → capture id; header `x-organization-id: O` for the rest.
2. **RBAC**: `POST /rbac/roles {name:"editor", permissions:["project:create","project:*"]}` → 200 raw role (no `permissions` key). `GET /rbac/roles` → array incl. editor with 2 permissions embedded. `PUT /rbac/roles/:id/permissions {permissions:["doc:read"]}` → 200 `{success:true}`; re-`GET` shows exactly `["doc:read"]`. `PUT` an unknown roleId → 404 "Role not found". Register B, invite B (member); `POST /rbac/assign {userId:B, roleId:editor}` → 200; assign to a non-member uuid → 404 "Member not found"; assign an unknown roleId → 404.
3. **Subscription**: `GET /subscription` before assign → 200 body `null`. Insert/seed a plan P; `POST /subscription/assign {planId:P}` → 200 `{success:true}`; `GET /subscription` → 200 with embedded `plan`, `customLimits: null`. Re-assign a second plan → 200 (upsert, `updatedAt` advances).
4. **Audit**: `GET /audit-logs` → array containing `org.created` (+ `org.member.invited` from step 2). `?action=org.created` filters to just that. `?userId=A` filters. `?limit=1` caps to 1. `?limit=0` → 422 "Validation failed"; `?limit=101` → 422.
5. **Guards**: any `/rbac|/subscription|/audit-logs` call without `x-organization-id` → 400; with a random org uuid A isn't in → 403 "Not a member of this organization".

## Step 12 — verify

```
cd apps/backend
go build ./...
go vet ./...
go test ./...                       # unit tests always run
# integration (needs infra):
make up            # from repo root: db + redis
make migrate
make seed          # default plans (needed by the subscription flow)
DATABASE_URL=... REDIS_URL=... go test ./internal/server/ -run RBAC -v
make lint          # golangci-lint
```

## Definition of done

- [ ] `go build ./...` and `go vet ./...` clean; `make lint` clean.
- [ ] New sqlc code generated + committed; `db/querier.go` lists all 11 new methods.
- [ ] `RequirePermission(action)` enforces `*` / exact / `resource:*`; owner bypass; non-member → "Missing permission".
- [ ] `/rbac/*` (4 routes), `/subscription` (2 routes), `/audit-logs` (1 route) match `docs/02-api-contract.md`: paths, `org` guard, bodies, 200 / `{success:true}`, and every error code (`ROLE_NOT_FOUND`, `FORBIDDEN`, `MEMBER_NOT_FOUND`).
- [ ] `POST /rbac/roles` returns the raw role (no `permissions`); `GET /rbac/roles` embeds `permissions`; `GET /subscription` embeds `plan` and returns `null` when none.
- [ ] `role.created`/`role.assigned`/`org.member.removed` constants defined but **not** written.
- [ ] `setPermissions` (delete + inserts) and `CreateRole`+permissions run in a transaction.
- [ ] Response JSON is camelCase; empty lists serialize as `[]`, absent subscription as `null`.
- [ ] Unit tests (rbac service incl. wildcard matrix, subscription, RequirePermission) + integration test pass against real pg+redis.
- [ ] No HTTP concerns leaked into services; services return only `apperror` codes.
- [ ] The three resolved deviations (#1 role-create tx, #2 uuid→422, #4 assign→`{success:true}`) recorded in `docs/03-target-architecture.md` "Open questions" (marked resolved) + a CHANGELOG note.
- [ ] `CLAUDE.md`/`README.md` updated: Phase 4 status + curl walkthrough; archive this plan to `.claude/plans/archives/`.

## Resolved decisions (owner-approved 2026-07-20 — build to these)

Every deviation below must be recorded in the `docs/03-target-architecture.md` "Open questions"
section (mark resolved) and get a CHANGELOG note, per CLAUDE.md ground rules.

1. **`CreateRole` + `setPermissions` run in one transaction. RESOLVED: yes, wrap in `WithTx`.**
   The source's two separate awaits can leave a role with zero permissions on a mid-write crash;
   CLAUDE.md mandates "multi-step writes run in transactions." `UpdatePermissions`'s delete+insert
   likewise runs in `WithTx`. **Deviation from source — document it.**
2. **Malformed uuids. RESOLVED: `validate:"uuid"` on body fields → 422; path param → 404.**
   `userId`/`roleId`/`planId` in JSON bodies carry `validate:"required,uuid"`, so a malformed value
   returns 422 "Validation failed" (vs. the source's 500 from a bad-uuid DB query). The `:roleId`
   **path** param has no body to validate, so a parse failure is treated as `ROLE_NOT_FOUND` (404) —
   consistent with the existing org handler mapping a bad `:userId` path param to `MEMBER_NOT_FOUND`.
   **Deviation from source — document it.**
3. **Duplicate actions in a `permissions` array. RESOLVED: keep parity — no dedupe.**
   `CreatePermission` is a plain `INSERT` (no `ON CONFLICT`); duplicate actions in one request violate
   the `(role_id, action)` unique constraint → 500, exactly as the source does. Duplicate actions are
   degenerate client input, untested by the contract's parity suite, and not worth a behavioral
   divergence. **No deviation.**
4. **`POST /subscription/assign` response + bad planId. RESOLVED: return `{success:true}`; nonexistent planId → 500.**
   The source returns an empty body; we return `{success:true}` to match invite/remove/assign-role
   (**deviation — document it**). A well-formed but **nonexistent** `planId` hits the plan FK → 500
   "Internal server error" — kept as-is, since no `PLAN_NOT_FOUND` code exists in the contract and
   pre-checking would invent behavior the source lacks. **No deviation on the FK/500 path.**
   (A malformed `planId` is already caught at 422 by decision #2.)
5. **`GET /audit-logs` `limit` error status. RESOLVED: accept the shared helper's behavior.**
   Use `httpx.BindAndValidate`. Out-of-range `limit` (`0`/`101`) → 422 (correct); a non-numeric
   `limit=abc` → 400 "Invalid request body" (bind-time type error). The contract only pins the 1–100
   range, so this is acceptable — do **not** hand-roll query parsing to force 422 on non-numeric input.
   **No deviation** (contract is silent on non-numeric input).
```
