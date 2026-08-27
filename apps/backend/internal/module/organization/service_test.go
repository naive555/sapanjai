package organization

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// ---- hand-mocked orgStore ----

type mockOrgStore struct {
	getOrganizationBySlug   func(ctx context.Context, slug string) (db.Organization, error)
	createOrganization      func(ctx context.Context, arg db.CreateOrganizationParams) (db.Organization, error)
	createMembership        func(ctx context.Context, arg db.CreateMembershipParams) (db.Membership, error)
	getMembership           func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
	countMembershipsByOrg   func(ctx context.Context, organizationID uuid.UUID) (int64, error)
	deleteMembership        func(ctx context.Context, arg db.DeleteMembershipParams) error
	listMembershipsByUser   func(ctx context.Context, userID uuid.UUID) ([]db.ListMembershipsByUserRow, error)
	listOrganizationMembers func(ctx context.Context, organizationID uuid.UUID) ([]db.ListOrganizationMembersRow, error)
	getUserByEmail          func(ctx context.Context, email string) (db.User, error)
	withTx                  func(ctx context.Context, fn func(q db.Querier) error) error
}

func (m *mockOrgStore) GetOrganizationBySlug(ctx context.Context, slug string) (db.Organization, error) {
	return m.getOrganizationBySlug(ctx, slug)
}
func (m *mockOrgStore) CreateOrganization(ctx context.Context, arg db.CreateOrganizationParams) (db.Organization, error) {
	return m.createOrganization(ctx, arg)
}
func (m *mockOrgStore) CreateMembership(ctx context.Context, arg db.CreateMembershipParams) (db.Membership, error) {
	return m.createMembership(ctx, arg)
}
func (m *mockOrgStore) GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
	return m.getMembership(ctx, arg)
}
func (m *mockOrgStore) CountMembershipsByOrg(ctx context.Context, organizationID uuid.UUID) (int64, error) {
	return m.countMembershipsByOrg(ctx, organizationID)
}
func (m *mockOrgStore) DeleteMembership(ctx context.Context, arg db.DeleteMembershipParams) error {
	return m.deleteMembership(ctx, arg)
}
func (m *mockOrgStore) ListMembershipsByUser(ctx context.Context, userID uuid.UUID) ([]db.ListMembershipsByUserRow, error) {
	return m.listMembershipsByUser(ctx, userID)
}
func (m *mockOrgStore) ListOrganizationMembers(ctx context.Context, organizationID uuid.UUID) ([]db.ListOrganizationMembersRow, error) {
	return m.listOrganizationMembers(ctx, organizationID)
}
func (m *mockOrgStore) GetUserByEmail(ctx context.Context, email string) (db.User, error) {
	return m.getUserByEmail(ctx, email)
}
func (m *mockOrgStore) WithTx(ctx context.Context, fn func(q db.Querier) error) error {
	return m.withTx(ctx, fn)
}

var _ orgStore = (*mockOrgStore)(nil)

// ---- hand-mocked limitEnforcer ----

type mockLimitEnforcer struct {
	enforceLimit func(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error
}

func (m *mockLimitEnforcer) EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
	return m.enforceLimit(ctx, organizationID, key, currentCount)
}

var _ limitEnforcer = (*mockLimitEnforcer)(nil)

// ---- spyQuerier: records CreateAuditLog calls for assertion ----

type spyQuerier struct {
	db.Querier
	auditCalls []db.CreateAuditLogParams
}

func (s *spyQuerier) CreateAuditLog(ctx context.Context, arg db.CreateAuditLogParams) error {
	s.auditCalls = append(s.auditCalls, arg)
	return nil
}

func newTestAudit(spy *spyQuerier) *auditlog.Service {
	return auditlog.NewService(spy, slog.New(slog.NewTextHandler(os.Stdout, nil)))
}

func appErrorCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperror.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

func allowAllLimiter() *mockLimitEnforcer {
	return &mockLimitEnforcer{
		enforceLimit: func(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
			return nil
		},
	}
}

// ---- ListMembers ----

func TestService_ListMembers_HappyPath(t *testing.T) {
	orgID := uuid.New()
	want := []db.ListOrganizationMembersRow{
		{UserID: uuid.New(), Email: "owner@example.com", Role: "owner"},
		{UserID: uuid.New(), Email: "member@example.com", Role: "member"},
	}
	var gotOrgID uuid.UUID
	store := &mockOrgStore{
		listOrganizationMembers: func(ctx context.Context, organizationID uuid.UUID) ([]db.ListOrganizationMembersRow, error) {
			gotOrgID = organizationID
			return want, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	got, err := svc.ListMembers(context.Background(), orgID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if gotOrgID != orgID {
		t.Errorf("ListOrganizationMembers called with org=%v, want %v", gotOrgID, orgID)
	}
	if len(got) != len(want) {
		t.Fatalf("ListMembers returned %d rows, want %d", len(got), len(want))
	}
}

func TestService_ListMembers_StoreError(t *testing.T) {
	wantErr := errors.New("boom")
	store := &mockOrgStore{
		listOrganizationMembers: func(ctx context.Context, organizationID uuid.UUID) ([]db.ListOrganizationMembersRow, error) {
			return nil, wantErr
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	_, err := svc.ListMembers(context.Background(), uuid.New())
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// ---- Invite ----

func TestService_Invite_InviterIsMember_Forbidden(t *testing.T) {
	store := &mockOrgStore{}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	err := svc.Invite(context.Background(), uuid.New(), uuid.New(), "member", "target@example.com", "member")
	if code := appErrorCode(t, err); code != apperror.Forbidden {
		t.Fatalf("code = %q, want %q", code, apperror.Forbidden)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record when invite is forbidden")
	}
}

func TestService_Invite_LimitExceeded(t *testing.T) {
	store := &mockOrgStore{
		countMembershipsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) {
			return 5, nil
		},
	}
	limiter := &mockLimitEnforcer{
		enforceLimit: func(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
			if key != "max_members" || currentCount != 5 {
				t.Fatalf("EnforceLimit called with key=%q count=%d, want max_members/5", key, currentCount)
			}
			return apperror.New(apperror.LimitExceeded)
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), limiter)

	err := svc.Invite(context.Background(), uuid.New(), uuid.New(), "admin", "target@example.com", "member")
	if code := appErrorCode(t, err); code != apperror.LimitExceeded {
		t.Fatalf("code = %q, want %q", code, apperror.LimitExceeded)
	}
}

func TestService_Invite_UserNotFound(t *testing.T) {
	store := &mockOrgStore{
		countMembershipsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) { return 0, nil },
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	err := svc.Invite(context.Background(), uuid.New(), uuid.New(), "owner", "nobody@example.com", "member")
	if code := appErrorCode(t, err); code != apperror.UserNotFound {
		t.Fatalf("code = %q, want %q", code, apperror.UserNotFound)
	}
}

func TestService_Invite_AlreadyMember(t *testing.T) {
	existingUser := db.User{ID: uuid.New(), Email: "target@example.com"}
	store := &mockOrgStore{
		countMembershipsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) { return 0, nil },
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return existingUser, nil
		},
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{ID: uuid.New(), UserID: existingUser.ID}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	err := svc.Invite(context.Background(), uuid.New(), uuid.New(), "admin", "target@example.com", "member")
	if code := appErrorCode(t, err); code != apperror.AlreadyMember {
		t.Fatalf("code = %q, want %q", code, apperror.AlreadyMember)
	}
}

func TestService_Invite_HappyPath(t *testing.T) {
	orgID := uuid.New()
	inviterID := uuid.New()
	existingUser := db.User{ID: uuid.New(), Email: "target@example.com"}

	var createdArg db.CreateMembershipParams
	store := &mockOrgStore{
		countMembershipsByOrg: func(ctx context.Context, organizationID uuid.UUID) (int64, error) { return 1, nil },
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return existingUser, nil
		},
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, pgx.ErrNoRows
		},
		createMembership: func(ctx context.Context, arg db.CreateMembershipParams) (db.Membership, error) {
			createdArg = arg
			return db.Membership{ID: uuid.New(), UserID: arg.UserID, OrganizationID: arg.OrganizationID, Role: arg.Role}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	err := svc.Invite(context.Background(), orgID, inviterID, "admin", "target@example.com", "admin")
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if createdArg.UserID != existingUser.ID || createdArg.OrganizationID != orgID || createdArg.Role != "admin" {
		t.Errorf("CreateMembership called with %+v, want user=%v org=%v role=admin", createdArg, existingUser.ID, orgID)
	}
	if len(spy.auditCalls) != 1 {
		t.Fatalf("expected 1 audit call, got %d", len(spy.auditCalls))
	}
	if spy.auditCalls[0].Action != auditlog.ActionOrgMemberInvited {
		t.Errorf("audit action = %q, want %q", spy.auditCalls[0].Action, auditlog.ActionOrgMemberInvited)
	}
}

// ---- RemoveMember ----

func TestService_RemoveMember_RequesterIsMember_Forbidden(t *testing.T) {
	store := &mockOrgStore{}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	err := svc.RemoveMember(context.Background(), uuid.New(), "member", uuid.New())
	if code := appErrorCode(t, err); code != apperror.Forbidden {
		t.Fatalf("code = %q, want %q", code, apperror.Forbidden)
	}
}

func TestService_RemoveMember_TargetNotFound(t *testing.T) {
	store := &mockOrgStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{}, pgx.ErrNoRows
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	err := svc.RemoveMember(context.Background(), uuid.New(), "owner", uuid.New())
	if code := appErrorCode(t, err); code != apperror.MemberNotFound {
		t.Fatalf("code = %q, want %q", code, apperror.MemberNotFound)
	}
}

func TestService_RemoveMember_CannotRemoveOwner(t *testing.T) {
	store := &mockOrgStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{Role: "owner"}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	err := svc.RemoveMember(context.Background(), uuid.New(), "admin", uuid.New())
	if code := appErrorCode(t, err); code != apperror.CannotRemoveOwner {
		t.Fatalf("code = %q, want %q", code, apperror.CannotRemoveOwner)
	}
}

func TestService_RemoveMember_HappyPath(t *testing.T) {
	orgID := uuid.New()
	targetID := uuid.New()

	var deletedArg db.DeleteMembershipParams
	deleteCalled := false
	store := &mockOrgStore{
		getMembership: func(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error) {
			return db.Membership{Role: "member"}, nil
		},
		deleteMembership: func(ctx context.Context, arg db.DeleteMembershipParams) error {
			deleteCalled = true
			deletedArg = arg
			return nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, newTestAudit(spy), allowAllLimiter())

	if err := svc.RemoveMember(context.Background(), orgID, "owner", targetID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if !deleteCalled {
		t.Fatal("expected DeleteMembership to be called")
	}
	if deletedArg.UserID != targetID || deletedArg.OrganizationID != orgID {
		t.Errorf("DeleteMembership called with %+v, want user=%v org=%v", deletedArg, targetID, orgID)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record on remove-member, matching source")
	}
}
