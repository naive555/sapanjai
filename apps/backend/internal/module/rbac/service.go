// Package rbac implements the /rbac module: role CRUD-lite, role
// assignment, and the HasPermission engine used by RequirePermission.
// Mirrors RBACService/RBACRepository in the source app
// (src/modules/rbac/{service,repository}.ts).
package rbac

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

var _ rbacStore = (*database.Store)(nil)

// rbacStore is the subset of *database.Store the rbac service depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface.
type rbacStore interface {
	CreateRole(ctx context.Context, arg db.CreateRoleParams) (db.Role, error)
	GetRoleByID(ctx context.Context, id uuid.UUID) (db.Role, error)
	ListRolesByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.Role, error)
	ListPermissionsByRoleIDs(ctx context.Context, roleIDs []uuid.UUID) ([]db.Permission, error)
	GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
	AssignMemberRole(ctx context.Context, arg db.AssignMemberRoleParams) error
	ListPermissionActionsByUserOrg(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error)
	WithTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// RoleWithPermissions pairs a role row with its (possibly empty, non-nil)
// permission set, as returned by ListRoles.
type RoleWithPermissions struct {
	Role        db.Role
	Permissions []db.Permission
}

// Service implements role create/list/update-permissions/assign and the
// HasPermission check, mirroring RBACService in the source app.
type Service struct {
	store rbacStore
}

// NewService builds an rbac Service.
func NewService(store rbacStore) *Service {
	return &Service{store: store}
}

// CreateRole creates a role and sets its permission set in a single
// transaction, then returns the raw role row. Mirrors RBACService.createRole,
// with one intentional deviation: the source runs the create and the
// setPermissions as two separate, non-atomic awaits; this wraps both in one
// WithTx per CLAUDE.md ("multi-step writes run in transactions"), so a
// mid-write failure can't leave a role with zero permissions.
func (s *Service) CreateRole(ctx context.Context, organizationID uuid.UUID, name string, description *string, permissions []string) (db.Role, error) {
	var role db.Role

	err := s.store.WithTx(ctx, func(q *db.Queries) error {
		var err error
		role, err = q.CreateRole(ctx, db.CreateRoleParams{
			OrganizationID: organizationID,
			Name:           name,
			Description:    description,
		})
		if err != nil {
			return err
		}
		return setPermissions(ctx, q, role.ID, permissions)
	})
	if err != nil {
		return db.Role{}, err
	}

	return role, nil
}

// ListRoles returns organizationID's roles, each with its permission set
// embedded. Roles with no permissions get a non-nil empty slice; the
// returned slice itself is non-nil (empty when the org has no roles) so the
// handler serializes it as [], not null.
func (s *Service) ListRoles(ctx context.Context, organizationID uuid.UUID) ([]RoleWithPermissions, error) {
	roles, err := s.store.ListRolesByOrg(ctx, organizationID)
	if err != nil {
		return nil, err
	}

	out := make([]RoleWithPermissions, len(roles))
	if len(roles) == 0 {
		return out, nil
	}

	roleIDs := make([]uuid.UUID, len(roles))
	for i, role := range roles {
		roleIDs[i] = role.ID
	}

	perms, err := s.store.ListPermissionsByRoleIDs(ctx, roleIDs)
	if err != nil {
		return nil, err
	}

	byRole := make(map[uuid.UUID][]db.Permission, len(roles))
	for _, p := range perms {
		byRole[p.RoleID] = append(byRole[p.RoleID], p)
	}

	for i, role := range roles {
		rolePerms := byRole[role.ID]
		if rolePerms == nil {
			rolePerms = []db.Permission{}
		}
		out[i] = RoleWithPermissions{Role: role, Permissions: rolePerms}
	}

	return out, nil
}

// UpdatePermissions replaces roleID's permission set. Mirrors
// RBACService.updatePermissions: role must exist and belong to
// organizationID.
func (s *Service) UpdatePermissions(ctx context.Context, roleID, organizationID uuid.UUID, permissions []string) error {
	role, err := s.store.GetRoleByID(ctx, roleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.RoleNotFound)
		}
		return err
	}
	if role.OrganizationID != organizationID {
		return apperror.New(apperror.Forbidden)
	}

	return s.store.WithTx(ctx, func(q *db.Queries) error {
		return setPermissions(ctx, q, roleID, permissions)
	})
}

// AssignRole assigns roleID to userID's membership in organizationID.
// Mirrors RBACService.assignRole: role must exist and belong to
// organizationID; userID must already be a member.
func (s *Service) AssignRole(ctx context.Context, organizationID, userID, roleID uuid.UUID) error {
	role, err := s.store.GetRoleByID(ctx, roleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.RoleNotFound)
		}
		return err
	}
	if role.OrganizationID != organizationID {
		return apperror.New(apperror.Forbidden)
	}

	membership, err := s.store.GetMembership(ctx, db.GetMembershipParams{UserID: userID, OrganizationID: organizationID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.MemberNotFound)
		}
		return err
	}

	return s.store.AssignMemberRole(ctx, db.AssignMemberRoleParams{MembershipID: membership.ID, RoleID: roleID})
}

// Authorize resolves userID's membership and granted action set in
// organizationID once, returning a *Principal a caller can run any number
// of Allows(action) checks against. This is what makes a bulk caller (e.g.
// filtering an MCP tool catalog against ~10 actions) cost one membership
// lookup plus one permission query instead of one pair per action.
//
// Preserves HasPermission's existing no-membership behavior: pgx.ErrNoRows
// is not an error here either, it yields a Principal with a nil Actions set
// that Allows() will always deny (mirroring the old "no membership ->
// false"). An owner's Actions is left unfetched — ListPermissionActionsByUserOrg
// is never called for an owner, matching HasPermission's owner bypass, so
// Authorize costs owners the same single query it always did.
func (s *Service) Authorize(ctx context.Context, userID, organizationID uuid.UUID) (*Principal, error) {
	membership, err := s.store.GetMembership(ctx, db.GetMembershipParams{UserID: userID, OrganizationID: organizationID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &Principal{UserID: userID, OrganizationID: organizationID}, nil
		}
		return nil, err
	}
	if membership.Role == "owner" {
		return &Principal{UserID: userID, OrganizationID: organizationID, Role: membership.Role}, nil
	}

	actions, err := s.store.ListPermissionActionsByUserOrg(ctx, db.ListPermissionActionsByUserOrgParams{
		UserID:         userID,
		OrganizationID: organizationID,
	})
	if err != nil {
		return nil, err
	}

	return &Principal{UserID: userID, OrganizationID: organizationID, Role: membership.Role, Actions: actions}, nil
}

// HasPermission reports whether userID can perform action in
// organizationID. Mirrors RBACRepository.getUserPermissions +
// RBACService.hasPermission: no membership -> false; owner role bypasses
// the roles tables entirely ("*"); otherwise the caller's roles' permission
// actions are checked for "*", an exact match, or a "<resource>:*"
// wildcard. A thin wrapper over Authorize(...).Allows(action) since
// step 1 of docs/07-sheets-adapter-plan.md — the single-action call site
// (RequirePermission) is unchanged, the semantics now live in one place.
func (s *Service) HasPermission(ctx context.Context, userID, organizationID uuid.UUID, action string) (bool, error) {
	principal, err := s.Authorize(ctx, userID, organizationID)
	if err != nil {
		return false, err
	}
	return principal.Allows(action), nil
}

// setPermissions replaces roleID's permission set: delete then re-insert,
// run inside the caller's transaction. No dedup of actions — a duplicate
// action in the input violates the permissions table's (role_id, action)
// unique constraint and surfaces as a 500, matching the source's plain
// insert (intentional bug-for-bug parity; see Phase 4 plan decision #3).
func setPermissions(ctx context.Context, q *db.Queries, roleID uuid.UUID, actions []string) error {
	if err := q.DeletePermissionsByRole(ctx, roleID); err != nil {
		return err
	}
	for _, action := range actions {
		if err := q.CreatePermission(ctx, db.CreatePermissionParams{RoleID: roleID, Action: action}); err != nil {
			return err
		}
	}
	return nil
}
