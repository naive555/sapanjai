// Note on coverage: CreateRole and the transactional body of
// UpdatePermissions run entirely inside store.WithTx, which hands the
// closure a concrete *db.Queries — an unexported-field struct this package
// cannot fake without a real DBTX (pgx pool/tx). That mirrors
// organization.Service.Create, which is likewise untested at the unit level
// and covered only by the integration suite; the same split applies here.
// What CAN be hand-mocked — the pre-tx guard checks, and that WithTx is
// reached on the happy path — is covered below. The actual insert/delete
// behavior inside the transaction is exercised by
// internal/server/rbac_subscription_audit_integration_test.go.
package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// ---- hand-mocked rbacStore ----

type mockRBACStore struct {
	createRole                     func(ctx context.Context, arg db.CreateRoleParams) (db.Role, error)
	getRoleByID                    func(ctx context.Context, id uuid.UUID) (db.Role, error)
	listRolesByOrg                 func(ctx context.Context, organizationID uuid.UUID) ([]db.Role, error)
	listPermissionsByRoleIDs       func(ctx context.Context, roleIDs []uuid.UUID) ([]db.Permission, error)
	getMembership                  func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
	assignMemberRole               func(ctx context.Context, arg db.AssignMemberRoleParams) error
	listPermissionActionsByUserOrg func(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error)
	withTx                         func(ctx context.Context, fn func(q db.Querier) error) error
}

func (m *mockRBACStore) CreateRole(ctx context.Context, arg db.CreateRoleParams) (db.Role, error) {
	return m.createRole(ctx, arg)
}
func (m *mockRBACStore) GetRoleByID(ctx context.Context, id uuid.UUID) (db.Role, error) {
	return m.getRoleByID(ctx, id)
}
func (m *mockRBACStore) ListRolesByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.Role, error) {
	return m.listRolesByOrg(ctx, organizationID)
}
func (m *mockRBACStore) ListPermissionsByRoleIDs(ctx context.Context, roleIDs []uuid.UUID) ([]db.Permission, error) {
	return m.listPermissionsByRoleIDs(ctx, roleIDs)
}
func (m *mockRBACStore) GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
	return m.getMembership(ctx, arg)
}
func (m *mockRBACStore) AssignMemberRole(ctx context.Context, arg db.AssignMemberRoleParams) error {
	return m.assignMemberRole(ctx, arg)
}
func (m *mockRBACStore) ListPermissionActionsByUserOrg(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error) {
	return m.listPermissionActionsByUserOrg(ctx, arg)
}
func (m *mockRBACStore) WithTx(ctx context.Context, fn func(q db.Querier) error) error {
	return m.withTx(ctx, fn)
}

var _ rbacStore = (*mockRBACStore)(nil)

func appErrCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperror.Error, got %T (%v)", err, err)
	}
	return appErr.Code
}

// ---- HasPermission ----

func TestHasPermission_OwnerBypassesRolesTables(t *testing.T) {
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{Role: "owner"}, nil
		},
		listPermissionActionsByUserOrg: func(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error) {
			t.Fatal("ListPermissionActionsByUserOrg should not be called for an owner")
			return nil, nil
		},
	}
	svc := NewService(store)

	allowed, err := svc.HasPermission(context.Background(), uuid.New(), uuid.New(), "anything:at-all")
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if !allowed {
		t.Error("expected owner to be allowed")
	}
}

func TestHasPermission_NoMembershipDenied(t *testing.T) {
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store)

	allowed, err := svc.HasPermission(context.Background(), uuid.New(), uuid.New(), "project:read")
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if allowed {
		t.Error("expected a non-member to be denied")
	}
}

func TestHasPermission_Wildcard(t *testing.T) {
	membership := func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
		return db.Membership{Role: "member"}, nil
	}

	cases := []struct {
		name    string
		actions []string
		check   string
		want    bool
	}{
		{"star grants anything", []string{"*"}, "billing:delete", true},
		{"exact match", []string{"project:create"}, "project:create", true},
		{"exact no match", []string{"project:create"}, "project:read", false},
		{"resource wildcard matches same resource", []string{"project:*"}, "project:create", true},
		{"resource wildcard does not match other resource", []string{"project:*"}, "billing:read", false},
		{"empty permission set denies", []string{}, "project:read", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockRBACStore{
				getMembership: membership,
				listPermissionActionsByUserOrg: func(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error) {
					return tc.actions, nil
				},
			}
			svc := NewService(store)

			allowed, err := svc.HasPermission(context.Background(), uuid.New(), uuid.New(), tc.check)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if allowed != tc.want {
				t.Errorf("HasPermission(%q) with actions=%v = %v, want %v", tc.check, tc.actions, allowed, tc.want)
			}
		})
	}
}

func TestHasPermission_DatabaseErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, dbErr
		},
	}
	svc := NewService(store)

	_, err := svc.HasPermission(context.Background(), uuid.New(), uuid.New(), "x:y")
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

// ---- Authorize ----

func TestAuthorize_OwnerSkipsPermissionsQuery(t *testing.T) {
	userID, orgID := uuid.New(), uuid.New()
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{Role: "owner"}, nil
		},
		listPermissionActionsByUserOrg: func(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error) {
			t.Fatal("ListPermissionActionsByUserOrg should not be called for an owner")
			return nil, nil
		},
	}
	svc := NewService(store)

	principal, err := svc.Authorize(context.Background(), userID, orgID)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if principal.Role != "owner" {
		t.Errorf("Role = %q, want %q", principal.Role, "owner")
	}
	if !principal.Allows("anything:at-all") {
		t.Error("expected owner principal to allow anything")
	}
}

func TestAuthorize_MemberWithGrants(t *testing.T) {
	userID, orgID := uuid.New(), uuid.New()
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{Role: "member"}, nil
		},
		listPermissionActionsByUserOrg: func(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error) {
			return []string{"project:*"}, nil
		},
	}
	svc := NewService(store)

	principal, err := svc.Authorize(context.Background(), userID, orgID)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if !principal.Allows("project:create") {
		t.Error("expected project:* to allow project:create")
	}
	if principal.Allows("billing:read") {
		t.Error("expected project:* to not allow billing:read")
	}
}

func TestAuthorize_MemberWithNoGrants(t *testing.T) {
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{Role: "member"}, nil
		},
		listPermissionActionsByUserOrg: func(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error) {
			return []string{}, nil
		},
	}
	svc := NewService(store)

	principal, err := svc.Authorize(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if principal.Allows("project:read") {
		t.Error("expected a member with no grants to be denied")
	}
}

func TestAuthorize_NonMemberAllowsNothing(t *testing.T) {
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store)

	principal, err := svc.Authorize(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if principal.Allows("project:read") {
		t.Error("expected a non-member principal to allow nothing")
	}
}

func TestAuthorize_MembershipStoreErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, dbErr
		},
	}
	svc := NewService(store)

	_, err := svc.Authorize(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

func TestAuthorize_PermissionsStoreErrorPropagates(t *testing.T) {
	dbErr := errors.New("connection reset")
	store := &mockRBACStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{Role: "member"}, nil
		},
		listPermissionActionsByUserOrg: func(ctx context.Context, arg db.ListPermissionActionsByUserOrgParams) ([]string, error) {
			return nil, dbErr
		},
	}
	svc := NewService(store)

	_, err := svc.Authorize(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, dbErr) {
		t.Fatalf("expected the raw db error to propagate, got %v", err)
	}
}

// ---- ListRoles ----

func TestListRoles_GroupsPermissionsByRole(t *testing.T) {
	roleA := db.Role{ID: uuid.New(), Name: "editor"}
	roleB := db.Role{ID: uuid.New(), Name: "viewer"}
	permA1 := db.Permission{ID: uuid.New(), RoleID: roleA.ID, Action: "project:create"}
	permA2 := db.Permission{ID: uuid.New(), RoleID: roleA.ID, Action: "project:delete"}

	store := &mockRBACStore{
		listRolesByOrg: func(ctx context.Context, organizationID uuid.UUID) ([]db.Role, error) {
			return []db.Role{roleA, roleB}, nil
		},
		listPermissionsByRoleIDs: func(ctx context.Context, roleIDs []uuid.UUID) ([]db.Permission, error) {
			return []db.Permission{permA1, permA2}, nil
		},
	}
	svc := NewService(store)

	roles, err := svc.ListRoles(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(roles))
	}
	if len(roles[0].Permissions) != 2 {
		t.Errorf("roleA permissions = %d, want 2", len(roles[0].Permissions))
	}
	if roles[1].Permissions == nil || len(roles[1].Permissions) != 0 {
		t.Errorf("roleB permissions = %+v, want a non-nil empty slice", roles[1].Permissions)
	}
}

func TestListRoles_EmptyOrgReturnsNonNilEmptySlice(t *testing.T) {
	store := &mockRBACStore{
		listRolesByOrg: func(ctx context.Context, organizationID uuid.UUID) ([]db.Role, error) {
			return nil, nil
		},
	}
	svc := NewService(store)

	roles, err := svc.ListRoles(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if roles == nil {
		t.Fatal("expected a non-nil empty slice")
	}
	if len(roles) != 0 {
		t.Errorf("expected 0 roles, got %d", len(roles))
	}
}

// ---- UpdatePermissions ----

func TestUpdatePermissions_RoleNotFound(t *testing.T) {
	store := &mockRBACStore{
		getRoleByID: func(ctx context.Context, id uuid.UUID) (db.Role, error) {
			return db.Role{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store)

	err := svc.UpdatePermissions(context.Background(), uuid.New(), uuid.New(), []string{"a:b"})
	if code := appErrCode(t, err); code != apperror.RoleNotFound {
		t.Fatalf("code = %q, want %q", code, apperror.RoleNotFound)
	}
}

func TestUpdatePermissions_WrongOrgForbidden(t *testing.T) {
	store := &mockRBACStore{
		getRoleByID: func(ctx context.Context, id uuid.UUID) (db.Role, error) {
			return db.Role{ID: id, OrganizationID: uuid.New()}, nil
		},
	}
	svc := NewService(store)

	err := svc.UpdatePermissions(context.Background(), uuid.New(), uuid.New(), []string{"a:b"})
	if code := appErrCode(t, err); code != apperror.Forbidden {
		t.Fatalf("code = %q, want %q", code, apperror.Forbidden)
	}
}

func TestUpdatePermissions_HappyPathDelegatesToWithTx(t *testing.T) {
	orgID := uuid.New()
	roleID := uuid.New()
	withTxCalled := false
	store := &mockRBACStore{
		getRoleByID: func(ctx context.Context, id uuid.UUID) (db.Role, error) {
			return db.Role{ID: roleID, OrganizationID: orgID}, nil
		},
		withTx: func(ctx context.Context, fn func(q db.Querier) error) error {
			withTxCalled = true
			return nil
		},
	}
	svc := NewService(store)

	if err := svc.UpdatePermissions(context.Background(), roleID, orgID, []string{"a:b"}); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if !withTxCalled {
		t.Error("expected WithTx to be called")
	}
}

// ---- AssignRole ----

func TestAssignRole_RoleNotFound(t *testing.T) {
	store := &mockRBACStore{
		getRoleByID: func(ctx context.Context, id uuid.UUID) (db.Role, error) {
			return db.Role{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store)

	err := svc.AssignRole(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if code := appErrCode(t, err); code != apperror.RoleNotFound {
		t.Fatalf("code = %q, want %q", code, apperror.RoleNotFound)
	}
}

func TestAssignRole_WrongOrgForbidden(t *testing.T) {
	store := &mockRBACStore{
		getRoleByID: func(ctx context.Context, id uuid.UUID) (db.Role, error) {
			return db.Role{ID: id, OrganizationID: uuid.New()}, nil
		},
	}
	svc := NewService(store)

	err := svc.AssignRole(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if code := appErrCode(t, err); code != apperror.Forbidden {
		t.Fatalf("code = %q, want %q", code, apperror.Forbidden)
	}
}

func TestAssignRole_MemberNotFound(t *testing.T) {
	orgID := uuid.New()
	roleID := uuid.New()
	store := &mockRBACStore{
		getRoleByID: func(ctx context.Context, id uuid.UUID) (db.Role, error) {
			return db.Role{ID: roleID, OrganizationID: orgID}, nil
		},
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store)

	err := svc.AssignRole(context.Background(), orgID, uuid.New(), roleID)
	if code := appErrCode(t, err); code != apperror.MemberNotFound {
		t.Fatalf("code = %q, want %q", code, apperror.MemberNotFound)
	}
}

func TestAssignRole_HappyPath(t *testing.T) {
	orgID := uuid.New()
	roleID := uuid.New()
	userID := uuid.New()
	membershipID := uuid.New()

	var gotArg db.AssignMemberRoleParams
	store := &mockRBACStore{
		getRoleByID: func(ctx context.Context, id uuid.UUID) (db.Role, error) {
			return db.Role{ID: roleID, OrganizationID: orgID}, nil
		},
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{ID: membershipID, UserID: userID, OrganizationID: orgID}, nil
		},
		assignMemberRole: func(ctx context.Context, arg db.AssignMemberRoleParams) error {
			gotArg = arg
			return nil
		},
	}
	svc := NewService(store)

	if err := svc.AssignRole(context.Background(), orgID, userID, roleID); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if gotArg.MembershipID != membershipID || gotArg.RoleID != roleID {
		t.Errorf("AssignMemberRole called with %+v, want membership=%v role=%v", gotArg, membershipID, roleID)
	}
}
