// Package organization implements the /organizations module: create, list,
// invite, remove-member. Mirrors src/modules/organization in the source
// app.
package organization

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// Compile-time checks that the concrete infra/module types satisfy the
// narrow interfaces this service depends on.
var (
	_ orgStore      = (*database.Store)(nil)
	_ limitEnforcer = (*subscription.Service)(nil)
)

// orgStore is the subset of *database.Store the org service depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface.
type orgStore interface {
	GetOrganizationBySlug(ctx context.Context, slug string) (db.Organization, error)
	CreateOrganization(ctx context.Context, arg db.CreateOrganizationParams) (db.Organization, error)
	CreateMembership(ctx context.Context, arg db.CreateMembershipParams) (db.Membership, error)
	GetMembership(ctx context.Context, arg db.GetMembershipParams) (db.Membership, error)
	CountMembershipsByOrg(ctx context.Context, organizationID uuid.UUID) (int64, error)
	DeleteMembership(ctx context.Context, arg db.DeleteMembershipParams) error
	ListMembershipsByUser(ctx context.Context, userID uuid.UUID) ([]db.ListMembershipsByUserRow, error)
	ListOrganizationMembers(ctx context.Context, organizationID uuid.UUID) ([]db.ListOrganizationMembersRow, error)
	GetUserByEmail(ctx context.Context, email string) (db.User, error)
	WithTx(ctx context.Context, fn func(q *db.Queries) error) error
}

// limitEnforcer is the subset of *subscription.Service the invite flow
// depends on, narrowed for the same reason as orgStore.
type limitEnforcer interface {
	EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error
}

// Service implements org create/list/invite/remove-member, mirroring
// OrgService in the source app's src/modules/organization/service.ts.
type Service struct {
	store  orgStore
	audit  *auditlog.Service
	limits limitEnforcer
}

// NewService builds an organization Service.
func NewService(store orgStore, audit *auditlog.Service, limits limitEnforcer) *Service {
	return &Service{store: store, audit: audit, limits: limits}
}

// Create creates a new organization and an "owner" membership for userID in
// a single transaction, then records the org.created audit entry. Returns
// apperror.SlugTaken if the slug is already in use.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, name, slug string) (db.Organization, error) {
	var org db.Organization

	err := s.store.WithTx(ctx, func(q *db.Queries) error {
		_, err := q.GetOrganizationBySlug(ctx, slug)
		if err == nil {
			return apperror.New(apperror.SlugTaken)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		org, err = q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: name, Slug: slug})
		if err != nil {
			return err
		}

		_, err = q.CreateMembership(ctx, db.CreateMembershipParams{
			UserID:         userID,
			OrganizationID: org.ID,
			Role:           "owner",
		})
		return err
	})
	if err != nil {
		return db.Organization{}, err
	}

	metadata, _ := json.Marshal(map[string]string{"name": org.Name, "slug": org.Slug})
	s.audit.Record(ctx, auditlog.ActionOrgCreated, &userID, &org.ID, metadata)

	return org, nil
}

// ListByUser returns userID's memberships with each organization embedded.
func (s *Service) ListByUser(ctx context.Context, userID uuid.UUID) ([]db.ListMembershipsByUserRow, error) {
	return s.store.ListMembershipsByUser(ctx, userID)
}

// ListMembers returns organizationID's member roster (user + role), ordered
// by membership creation time. Guarded by RequireOrg, so the caller is
// already verified as a member.
func (s *Service) ListMembers(ctx context.Context, organizationID uuid.UUID) ([]db.ListOrganizationMembersRow, error) {
	return s.store.ListOrganizationMembers(ctx, organizationID)
}

// Invite adds a new member to organizationID, called with the inviter's own
// role (already resolved by the RequireOrg guard) rather than re-fetching
// it. Mirrors OrgService.invite, including check order: role, member-limit,
// user-lookup, already-member.
func (s *Service) Invite(ctx context.Context, organizationID, inviterID uuid.UUID, inviterRole, email, role string) error {
	if inviterRole == "member" {
		return apperror.New(apperror.Forbidden)
	}

	count, err := s.store.CountMembershipsByOrg(ctx, organizationID)
	if err != nil {
		return err
	}
	if err := s.limits.EnforceLimit(ctx, organizationID, "max_members", int(count)); err != nil {
		return err
	}

	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.UserNotFound)
		}
		return err
	}

	_, err = s.store.GetMembership(ctx, db.GetMembershipParams{UserID: user.ID, OrganizationID: organizationID})
	if err == nil {
		return apperror.New(apperror.AlreadyMember)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	if _, err := s.store.CreateMembership(ctx, db.CreateMembershipParams{
		UserID:         user.ID,
		OrganizationID: organizationID,
		Role:           role,
	}); err != nil {
		return err
	}

	metadata, _ := json.Marshal(map[string]string{"email": email, "role": role})
	s.audit.Record(ctx, auditlog.ActionOrgMemberInvited, &inviterID, &organizationID, metadata)

	return nil
}

// RemoveMember removes targetUserID from organizationID, called with the
// requester's own role (already resolved by the RequireOrg guard). Mirrors
// OrgService.removeMember, including check order: role, target-lookup,
// cannot-remove-owner. No audit entry — matches source (org.member.removed
// is defined but not written).
func (s *Service) RemoveMember(ctx context.Context, organizationID uuid.UUID, requesterRole string, targetUserID uuid.UUID) error {
	if requesterRole == "member" {
		return apperror.New(apperror.Forbidden)
	}

	target, err := s.store.GetMembership(ctx, db.GetMembershipParams{UserID: targetUserID, OrganizationID: organizationID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.MemberNotFound)
		}
		return err
	}
	if target.Role == "owner" {
		return apperror.New(apperror.CannotRemoveOwner)
	}

	return s.store.DeleteMembership(ctx, db.DeleteMembershipParams{UserID: targetUserID, OrganizationID: organizationID})
}
