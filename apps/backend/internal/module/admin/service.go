// Package admin implements the /admin module: cross-org READ surfaces and
// superadmin-only mutations for the platform staff console
// (docs/11-admin-panel.md). Every route is guarded by
// internal/middleware.Guards.RequirePlatformRole rather than
// RequireOrg/RequirePermission — platform staff typically hold no
// membership anywhere — and every query in
// internal/infra/database/queries/admin.sql deliberately carries no
// organization_id predicate the way tenant-facing queries do. That is the
// whole point of this module, not an oversight.
//
// This file implements the read surfaces (execution plan
// (.claude/plans/2026-08-28-admin-panel.md) Phase 2, Tasks 2.1-2.10) and
// the mutation routes (Phase 3, Tasks 3.1-3.5): plan/limit changes,
// org delete, platform-role grant/revoke, ban/unban, plan CRUD, and
// impersonation (Phase 4).
//
// Decrypted connector config never appears anywhere in this package —
// ConnectorItem is metadata only (id, organizationId, organizationName,
// name, type, status, lastHealthCheckAt, createdAt), and no query here
// ever selects connectors.encrypted_config or mcp_api_keys.key_hash.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	appredis "github.com/sapanjai/backend/internal/infra/redis"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/password"
)

var (
	_ adminStore           = (*database.Store)(nil)
	_ subscriptionResolver = (*subscription.Service)(nil)
	_ adminAuth            = (*appredis.Auth)(nil)
)

// nestedListLimit bounds the connector/MCP-key lists nested inside
// GET /admin/organizations/:orgId (Task 2.4). Unlike the top-level
// GET /admin/connectors and GET /admin/mcp-keys these are not paginated —
// a single org's own connectors/keys are expected to stay well under this
// ceiling for the life of the product. Reuses the exact same
// AdminListConnectors/AdminListMCPKeys queries and item shapes as those
// two cross-org endpoints.
const nestedListLimit = 500

// recentOrgAuditLimit is how many of an org's most recent audit entries
// GET /admin/organizations/:orgId embeds (Task 2.4).
const recentOrgAuditLimit = 20

// statsLookbackWindow is the "last 7d" window GET /admin/system/stats
// reports growth over (Task 2.8).
const statsLookbackWindow = 7 * 24 * time.Hour

// adminStore is the subset of *database.Store this service depends on,
// narrowed so unit tests can hand-mock it without the full db.Querier
// surface.
type adminStore interface {
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)

	AdminGetOrganizationByID(ctx context.Context, id uuid.UUID) (db.Organization, error)
	AdminListOrganizations(ctx context.Context, arg db.AdminListOrganizationsParams) ([]db.AdminListOrganizationsRow, error)
	AdminCountOrganizations(ctx context.Context, search *string) (int64, error)

	AdminListUsers(ctx context.Context, arg db.AdminListUsersParams) ([]db.AdminListUsersRow, error)
	AdminCountUsers(ctx context.Context, arg db.AdminCountUsersParams) (int64, error)
	AdminCountActiveSessionsByUser(ctx context.Context, userID uuid.UUID) (int64, error)

	AdminListConnectors(ctx context.Context, arg db.AdminListConnectorsParams) ([]db.AdminListConnectorsRow, error)
	AdminCountConnectors(ctx context.Context, arg db.AdminCountConnectorsParams) (int64, error)

	AdminListMCPKeys(ctx context.Context, arg db.AdminListMCPKeysParams) ([]db.AdminListMCPKeysRow, error)
	AdminCountMCPKeys(ctx context.Context, arg db.AdminCountMCPKeysParams) (int64, error)

	AdminQueryAuditLogs(ctx context.Context, arg db.AdminQueryAuditLogsParams) ([]db.AdminQueryAuditLogsRow, error)
	AdminCountAuditLogs(ctx context.Context, arg db.AdminCountAuditLogsParams) (int64, error)

	AdminCountAllOrganizations(ctx context.Context) (int64, error)
	AdminCountAllUsers(ctx context.Context) (int64, error)
	AdminCountAllConnectors(ctx context.Context) (int64, error)
	AdminCountAllMCPKeys(ctx context.Context) (int64, error)
	AdminCountActiveMCPKeys(ctx context.Context) (int64, error)
	AdminCountActiveSessions(ctx context.Context) (int64, error)
	AdminCountAllAuditLogs(ctx context.Context) (int64, error)
	AdminCountEmailOutboxByStatus(ctx context.Context) ([]db.AdminCountEmailOutboxByStatusRow, error)
	AdminCountUsersSince(ctx context.Context, since time.Time) (int64, error)
	AdminCountOrganizationsSince(ctx context.Context, since time.Time) (int64, error)
	AdminPlanBreakdown(ctx context.Context) ([]db.AdminPlanBreakdownRow, error)

	ListOrganizationMembers(ctx context.Context, organizationID uuid.UUID) ([]db.ListOrganizationMembersRow, error)
	ListMembershipsByUser(ctx context.Context, userID uuid.UUID) ([]db.ListMembershipsByUserRow, error)

	// ---- Phase 3: mutations ----

	AdminSetOrgCustomLimits(ctx context.Context, arg db.AdminSetOrgCustomLimitsParams) (int64, error)
	AdminDeleteOrganization(ctx context.Context, id uuid.UUID) error

	// CountSuperadmins is called directly (not through WithTx below) since
	// it only ever reads. SetUserPlatformRole/SetUserBan/
	// RevokeAllUserSessions are NOT listed here even though the mutations
	// below use all three — they're issued against the db.Querier WithTx's
	// fn callback receives, which already carries the full generated
	// surface, so declaring them again on this narrower interface would be
	// dead surface no Service method actually calls through it.
	CountSuperadmins(ctx context.Context) (int64, error)

	// WithTx backs the platform-role-change and ban/unban mutations, both
	// of which pair a users-table write with a session revocation
	// (CLAUDE.md: "Multi-step writes ... run in transactions", the same
	// reasoning already applied to org-create+owner-membership and session
	// rotation). fn receives the generated db.Querier so a unit test can
	// substitute a mock transaction body without a real pgx tx underneath —
	// see database.Store.WithTx's own doc comment.
	WithTx(ctx context.Context, fn func(q db.Querier) error) error

	// AdminListPlans, not subscriptionResolver.ListPlans: the tenant
	// catalogue now filters on is_public (queries/plans.sql's
	// ListPublicPlans, billing plan step 8), and the console that SETS
	// is_public must still see the plans it hid. Two callers, two
	// different questions, two queries.
	AdminListPlans(ctx context.Context) ([]db.Plan, error)
	AdminGetPlanByID(ctx context.Context, id uuid.UUID) (db.Plan, error)
	AdminCreatePlan(ctx context.Context, arg db.AdminCreatePlanParams) (db.Plan, error)
	AdminUpdatePlan(ctx context.Context, arg db.AdminUpdatePlanParams) (db.Plan, error)
	AdminDeletePlan(ctx context.Context, id uuid.UUID) error
	AdminCountSubscriptionsByPlan(ctx context.Context, planID uuid.UUID) (int64, error)

	// plan_prices (migration 00013, billing plan step 8). No update-amount
	// and no delete member here, deliberately -- queries/admin.sql's
	// plan_prices block comment has the reasoning, and the absence is
	// enforced by this interface not naming them.
	AdminListPlanPrices(ctx context.Context, planID uuid.UUID) ([]db.PlanPrice, error)
	AdminCreatePlanPrice(ctx context.Context, arg db.AdminCreatePlanPriceParams) (db.PlanPrice, error)
	AdminGetPlanPrice(ctx context.Context, arg db.AdminGetPlanPriceParams) (db.PlanPrice, error)
	AdminSetPlanPriceActive(ctx context.Context, arg db.AdminSetPlanPriceActiveParams) (db.PlanPrice, error)
	AdminCountActivePlanPricesFor(ctx context.Context, arg db.AdminCountActivePlanPricesForParams) (int64, error)

	// ---- Phase 6: TOTP step-up (Task 6.3) ----
	UpsertUserTOTPSecret(ctx context.Context, arg db.UpsertUserTOTPSecretParams) error
	GetUserTOTP(ctx context.Context, userID uuid.UUID) (db.UserTotp, error)
	ConfirmUserTOTP(ctx context.Context, arg db.ConfirmUserTOTPParams) error
	UpdateUserTOTPRecoveryCodes(ctx context.Context, arg db.UpdateUserTOTPRecoveryCodesParams) error
}

// adminAuth is the subset of *redis.Auth (internal/infra/redis) this
// service depends on: the re-auth attempt limiter (Task 3.2) and the
// durable-ban Redis cache (D3, internal/middleware.Guards.verify's
// consumer). Narrowed for the same hand-mockable-without-a-real-Redis-client
// reason as adminStore and countCache.
type adminAuth interface {
	GetReauthAttempts(ctx context.Context, userID uuid.UUID) (int, error)
	IncrementReauthAttempts(ctx context.Context, userID uuid.UUID) (int64, error)
	ResetReauthAttempts(ctx context.Context, userID uuid.UUID) error
	Ban(ctx context.Context, userID uuid.UUID) error
	Unban(ctx context.Context, userID uuid.UUID) error

	// ---- Phase 6: TOTP step-up (Task 6.3) ----
	SetTwoFactorVerified(ctx context.Context, userID uuid.UUID) error
	GetTwoFactorAttempts(ctx context.Context, userID uuid.UUID) (int, error)
	IncrementTwoFactorAttempts(ctx context.Context, userID uuid.UUID) (int64, error)
	ResetTwoFactorAttempts(ctx context.Context, userID uuid.UUID) error
}

// subscriptionResolver is the subset of *subscription.Service the
// organization-detail view depends on. GetSubscription/EffectiveLimits
// already implement the custom-over-plan merge — Task 2.4 calls into them
// rather than reimplementing it here.
type subscriptionResolver interface {
	GetSubscription(ctx context.Context, organizationID uuid.UUID) (*db.GetOrgSubscriptionRow, error)
	EffectiveLimits(ctx context.Context, organizationID uuid.UUID) (map[string]float64, error)

	// AssignPlan backs POST /admin/organizations/:orgId/plan (Task 3.3) —
	// delegates to subscription.Service's existing upsert rather than
	// reimplementing it here.
	AssignPlan(ctx context.Context, organizationID, planID uuid.UUID) error
}

// impersonationSigner is the subset of *auth.TokenService the impersonation
// endpoint depends on (Phase 4). Narrowed to the one method so the admin
// module cannot accidentally reach for SignAccessToken/SignRefreshToken and
// mint an ordinary, refreshable session for a tenant user.
type impersonationSigner interface {
	SignImpersonationToken(targetID uuid.UUID, targetEmail string, actorID uuid.UUID, ttl time.Duration) (string, error)
}

// totpSealer is the subset of *envelope.Encryptor the TOTP step-up routes
// depend on (Phase 6, Task 6.3) — the exact same shape connector.sealer
// narrows to, deliberately: user_totp.secret_encrypted is sealed under
// CONNECTOR_MASTER_KEY, the same envelope machinery and the same KeyProvider
// instance connectors use (server.go constructs one Encryptor and hands it
// to both services), so rotation is already solved and no new secret is
// introduced. See CLAUDE.md's envelope-encryption ground rule.
type totpSealer interface {
	Seal(ctx context.Context, plaintext, aad []byte) (json.RawMessage, error)
	Open(ctx context.Context, raw json.RawMessage, aad []byte) ([]byte, error)
}

// Service implements the admin module's read surfaces (Phase 2), mutation
// routes (Phase 3), impersonation (Phase 4), and TOTP step-up (Phase 6).
type Service struct {
	store     adminStore
	cache     countCache
	audit     *auditlog.Service
	sub       subscriptionResolver
	redisAuth adminAuth
	signer    impersonationSigner
	crypto    totpSealer
	log       *slog.Logger
}

// NewService builds an admin Service from its explicit dependencies — no
// service locator (execution plan Task 2.9).
func NewService(store adminStore, cache countCache, audit *auditlog.Service, sub subscriptionResolver, redisAuth adminAuth, signer impersonationSigner, crypto totpSealer, log *slog.Logger) *Service {
	return &Service{store: store, cache: cache, audit: audit, sub: sub, redisAuth: redisAuth, signer: signer, crypto: crypto, log: log}
}

// AdminContext carries the request-scoped values a Phase 3 mutation's audit
// write needs — the calling admin's own id, and the ip/userAgent captured
// in the handler via c.RealIP()/c.Request().UserAgent() — without letting
// the service read echo.Context directly (execution plan Task 3.1;
// CLAUDE.md's handler->service->sqlc convention: no HTTP concerns inside a
// service, and echo.Context is an HTTP concern).
//
// Caveat inherited from the handler, updated now that Phase 6 Task 6.1 is
// done: server.New sets e.IPExtractor, and apps/frontend's proxy
// (app/api/[...path]/route.ts) strips any inbound X-Forwarded-For/X-Real-IP
// before forwarding, so a caller can no longer inject an arbitrary chain.
// That closes the spoofing hole, but it does not make this field a
// per-staff-member location: every admin request arrives through that same
// frontend proxy, so IP resolves to the frontend's own private-network
// address for all of them, not the individual browser's address. Treat it
// as "which of our own services relayed this," not "where the staff member
// physically was" — see server.go's e.IPExtractor comment and
// docs/09-railway-deploy.md for the full trust chain this depends on.
type AdminContext struct {
	AdminID   uuid.UUID
	IP        string
	UserAgent string
}

// maxReauthAttempts mirrors auth.maxLoginAttempts (internal/module/auth):
// same cap, same 15-minute window, independent counter. Without this, the
// password field on every destructive admin endpoint would be an online
// password oracle against the highest-value accounts in the system
// (execution plan Task 3.2).
const maxReauthAttempts = 5

// impersonationTTL is how long a token from
// POST /admin/users/:userId/impersonate stays valid (docs/11-admin-panel.md
// §5). Deliberately far shorter than JWT_ACCESS_EXPIRES_IN and not
// configurable: the containment argument for impersonation rests on this
// number, so it belongs in code beside that argument rather than in an env
// var an operator could quietly raise to eight hours.
const impersonationTTL = 10 * time.Minute

// superadminCap bounds how many accounts may simultaneously hold
// platform_role = 'superadmin' (execution plan Task 3.4). Staff is 2-5
// people; 10 is headroom, not a real ceiling anyone should approach — its
// job is to fail loudly on a scripting mistake (a loop that promotes every
// row in a CSV), not to constrain legitimate growth.
const superadminCap = 10

// reauth re-verifies the calling admin's own password before a destructive
// operation. It exists so that a session left open on an unlocked laptop,
// or a stolen access token, is not by itself sufficient to delete an
// organization, ban a user, or grant platform_role (docs/11-admin-panel.md
// D4: per-operation, not a sudo window — the friction is deliberate).
//
// Rate-limited independently of login (admin:reauth:attempts:<userId>,
// internal/infra/redis/auth.go) — reusing the login counter would let an
// attacker who already burned an account's login attempts get a fresh
// budget here, and vice versa. Structure mirrors auth.Service.Login's own
// rate-limit dance exactly: check-before-compare, increment-and-propagate
// on failure, reset on success.
func (s *Service) reauth(ctx context.Context, adminID uuid.UUID, pw string) error {
	attempts, err := s.redisAuth.GetReauthAttempts(ctx, adminID)
	if err != nil {
		return err
	}
	if attempts >= maxReauthAttempts {
		return apperror.New(apperror.TooManyAttempts)
	}

	admin, err := s.store.GetUserByID(ctx, adminID)
	if err != nil {
		return err
	}

	if _, err := password.Verify(ctx, admin.PasswordHash, pw); err != nil {
		if ctx.Err() != nil {
			return err
		}
		if _, incErr := s.redisAuth.IncrementReauthAttempts(ctx, adminID); incErr != nil {
			return incErr
		}
		return apperror.New(apperror.ReauthFailed)
	}

	return s.redisAuth.ResetReauthAttempts(ctx, adminID)
}

// adminAuditMetadata marshals a mutation's action-specific fields plus the
// ip/userAgent every admin audit entry carries (execution plan Task 3.1)
// into one JSON metadata blob. A marshal failure (extra should only ever
// hold JSON-safe values the caller constructs, so this is defensive, not
// expected) degrades to ip/userAgent alone rather than losing the audit
// entry's ip/userAgent, since Record itself is best-effort and swallows
// its own errors.
func adminAuditMetadata(actx AdminContext, extra map[string]any) json.RawMessage {
	m := map[string]any{"ip": actx.IP, "userAgent": actx.UserAgent}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"ip": actx.IP, "userAgent": actx.UserAgent})
	}
	return b
}

// Me returns the calling staff account's own admin-facing profile
// (Task 2.3). The role is read fresh off the same row RequirePlatformRole
// just fetched, not passed through from the guard's context value — one
// extra field off a lookup this handler already needs, not a second
// source of truth.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (MeResponse, error) {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return MeResponse{}, err
	}
	var role string
	if user.PlatformRole != nil {
		role = *user.PlatformRole
	}
	return MeResponse{ID: user.ID, Email: user.Email, DisplayName: user.DisplayName, PlatformRole: role}, nil
}

// ListOrganizations returns a page of organizations, search matching name
// or slug (Task 2.4).
func (s *Service) ListOrganizations(ctx context.Context, search *string, limit, offset int32) (OrganizationsListResponse, error) {
	rows, err := s.store.AdminListOrganizations(ctx, db.AdminListOrganizationsParams{Search: search, Lim: limit, Off: offset})
	if err != nil {
		return OrganizationsListResponse{}, err
	}

	total, err := s.cachedCount(ctx, organizationsFilterKey(search), func() (int64, error) {
		return s.store.AdminCountOrganizations(ctx, search)
	})
	if err != nil {
		return OrganizationsListResponse{}, err
	}

	items := make([]OrganizationListItem, len(rows))
	for i, r := range rows {
		items[i] = OrganizationListItem{
			ID:             r.ID,
			Name:           r.Name,
			Slug:           r.Slug,
			CreatedAt:      r.CreatedAt,
			MemberCount:    r.MemberCount,
			ConnectorCount: r.ConnectorCount,
			MCPKeyCount:    r.McpKeyCount,
			PlanName:       r.PlanName,
		}
	}
	return OrganizationsListResponse{Items: items, Total: total}, nil
}

// GetOrganization returns orgID's detail view: the org row, its plan and
// effective limits (via subscription.Service — Task 2.4), its member
// roster, its connectors and MCP keys (metadata only, both capped at
// nestedListLimit), and its 20 most recent audit entries.
func (s *Service) GetOrganization(ctx context.Context, orgID uuid.UUID) (OrganizationDetailResponse, error) {
	org, err := s.store.AdminGetOrganizationByID(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OrganizationDetailResponse{}, apperror.New(apperror.NotFound)
		}
		return OrganizationDetailResponse{}, err
	}

	var planName *string
	sub, err := s.sub.GetSubscription(ctx, orgID)
	if err != nil {
		return OrganizationDetailResponse{}, err
	}
	if sub != nil {
		planName = &sub.PlanName
	}

	limits, err := s.sub.EffectiveLimits(ctx, orgID)
	if err != nil {
		return OrganizationDetailResponse{}, err
	}

	memberRows, err := s.store.ListOrganizationMembers(ctx, orgID)
	if err != nil {
		return OrganizationDetailResponse{}, err
	}
	members := make([]OrgMemberItem, len(memberRows))
	for i, m := range memberRows {
		members[i] = OrgMemberItem{UserID: m.UserID, Email: m.Email, DisplayName: m.DisplayName, Role: m.Role, JoinedAt: m.JoinedAt}
	}

	connectorRows, err := s.store.AdminListConnectors(ctx, db.AdminListConnectorsParams{
		OrganizationID: toPgUUID(&orgID),
		Lim:            nestedListLimit,
	})
	if err != nil {
		return OrganizationDetailResponse{}, err
	}
	connectors := make([]ConnectorItem, len(connectorRows))
	for i, c := range connectorRows {
		connectors[i] = toConnectorItem(c)
	}

	keyRows, err := s.store.AdminListMCPKeys(ctx, db.AdminListMCPKeysParams{
		OrganizationID: toPgUUID(&orgID),
		Lim:            nestedListLimit,
	})
	if err != nil {
		return OrganizationDetailResponse{}, err
	}
	keys := make([]MCPKeyItem, len(keyRows))
	for i, k := range keyRows {
		keys[i] = toMCPKeyItem(k)
	}

	auditRows, err := s.audit.Query(ctx, orgID, nil, nil, nil, recentOrgAuditLimit)
	if err != nil {
		return OrganizationDetailResponse{}, err
	}
	audits := make([]OrgAuditLogItem, len(auditRows))
	for i, a := range auditRows {
		audits[i] = OrgAuditLogItem{ID: a.ID, UserID: fromPgUUID(a.UserID), Action: a.Action, Metadata: nonEmptyJSON(a.Metadata), CreatedAt: a.CreatedAt}
	}

	return OrganizationDetailResponse{
		ID:              org.ID,
		Name:            org.Name,
		Slug:            org.Slug,
		CreatedAt:       org.CreatedAt,
		UpdatedAt:       org.UpdatedAt,
		PlanName:        planName,
		EffectiveLimits: limits,
		Members:         members,
		Connectors:      connectors,
		MCPKeys:         keys,
		RecentAuditLogs: audits,
	}, nil
}

// ListUsers returns a page of users, search matching email or display
// name, optionally filtered by platform role and/or ban status (Task 2.5).
func (s *Service) ListUsers(ctx context.Context, search, role *string, banned *bool, limit, offset int32) (UsersListResponse, error) {
	rows, err := s.store.AdminListUsers(ctx, db.AdminListUsersParams{Search: search, Role: role, Banned: banned, Lim: limit, Off: offset})
	if err != nil {
		return UsersListResponse{}, err
	}

	total, err := s.cachedCount(ctx, usersFilterKey(search, role, banned), func() (int64, error) {
		return s.store.AdminCountUsers(ctx, db.AdminCountUsersParams{Search: search, Role: role, Banned: banned})
	})
	if err != nil {
		return UsersListResponse{}, err
	}

	items := make([]UserListItem, len(rows))
	for i, r := range rows {
		items[i] = UserListItem{
			ID:           r.ID,
			Email:        r.Email,
			DisplayName:  r.DisplayName,
			IsVerified:   r.IsVerified,
			PlatformRole: r.PlatformRole,
			BannedAt:     fromPgTimestamp(r.BannedAt),
			CreatedAt:    r.CreatedAt,
			OrgCount:     r.OrgCount,
		}
	}
	return UsersListResponse{Items: items, Total: total}, nil
}

// GetUser returns userID's detail view: the user row (never PasswordHash —
// mapped explicitly field-by-field below, never a struct embed), every
// membership with org name/slug/role, and the count of currently active
// (non-revoked, unexpired) sessions.
func (s *Service) GetUser(ctx context.Context, userID uuid.UUID) (UserDetailResponse, error) {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UserDetailResponse{}, apperror.New(apperror.UserNotFound)
		}
		return UserDetailResponse{}, err
	}

	membershipRows, err := s.store.ListMembershipsByUser(ctx, userID)
	if err != nil {
		return UserDetailResponse{}, err
	}
	memberships := make([]UserMembershipItem, len(membershipRows))
	for i, m := range membershipRows {
		memberships[i] = UserMembershipItem{
			OrganizationID:   m.OrgID,
			OrganizationName: m.OrgName,
			OrganizationSlug: m.OrgSlug,
			Role:             m.Role,
			JoinedAt:         m.CreatedAt,
		}
	}

	activeSessions, err := s.store.AdminCountActiveSessionsByUser(ctx, userID)
	if err != nil {
		return UserDetailResponse{}, err
	}

	return UserDetailResponse{
		ID:             user.ID,
		Email:          user.Email,
		DisplayName:    user.DisplayName,
		IsVerified:     user.IsVerified,
		PlatformRole:   user.PlatformRole,
		BannedAt:       fromPgTimestamp(user.BannedAt),
		BanReason:      user.BanReason,
		CreatedAt:      user.CreatedAt,
		Memberships:    memberships,
		ActiveSessions: activeSessions,
	}, nil
}

// ListConnectors returns a page of connectors across every organization,
// metadata only (Task 2.6).
func (s *Service) ListConnectors(ctx context.Context, organizationID *uuid.UUID, connType, status, search *string, limit, offset int32) (ConnectorsListResponse, error) {
	orgFilter := toPgUUID(organizationID)
	rows, err := s.store.AdminListConnectors(ctx, db.AdminListConnectorsParams{
		OrganizationID: orgFilter, Type: connType, Status: status, Search: search, Lim: limit, Off: offset,
	})
	if err != nil {
		return ConnectorsListResponse{}, err
	}

	total, err := s.cachedCount(ctx, connectorsFilterKey(organizationID, connType, status, search), func() (int64, error) {
		return s.store.AdminCountConnectors(ctx, db.AdminCountConnectorsParams{OrganizationID: orgFilter, Type: connType, Status: status, Search: search})
	})
	if err != nil {
		return ConnectorsListResponse{}, err
	}

	items := make([]ConnectorItem, len(rows))
	for i, r := range rows {
		items[i] = toConnectorItem(r)
	}
	return ConnectorsListResponse{Items: items, Total: total}, nil
}

// ListMCPKeys returns a page of MCP keys across every organization,
// metadata only — never KeyHash (Task 2.6).
func (s *Service) ListMCPKeys(ctx context.Context, organizationID, userID *uuid.UUID, search *string, limit, offset int32) (MCPKeysListResponse, error) {
	orgFilter := toPgUUID(organizationID)
	userFilter := toPgUUID(userID)
	rows, err := s.store.AdminListMCPKeys(ctx, db.AdminListMCPKeysParams{
		OrganizationID: orgFilter, UserID: userFilter, Search: search, Lim: limit, Off: offset,
	})
	if err != nil {
		return MCPKeysListResponse{}, err
	}

	total, err := s.cachedCount(ctx, mcpKeysFilterKey(organizationID, userID, search), func() (int64, error) {
		return s.store.AdminCountMCPKeys(ctx, db.AdminCountMCPKeysParams{OrganizationID: orgFilter, UserID: userFilter, Search: search})
	})
	if err != nil {
		return MCPKeysListResponse{}, err
	}

	items := make([]MCPKeyItem, len(rows))
	for i, r := range rows {
		items[i] = toMCPKeyItem(r)
	}
	return MCPKeysListResponse{Items: items, Total: total}, nil
}

// QueryAuditLogs returns a page of cross-org audit log entries (Task 2.7).
// actionPatterns is already-converted LIKE patterns (buildActionPatterns);
// from/to are expected already normalized to UTC by the caller, mirroring
// auditlog.Service.Query's own contract for the same naive-timestamp
// column.
func (s *Service) QueryAuditLogs(ctx context.Context, organizationID, userID *uuid.UUID, actionPatterns []string, from, to *time.Time, limit, offset int32) (AuditLogsListResponse, error) {
	orgFilter := toPgUUID(organizationID)
	userFilter := toPgUUID(userID)
	fromFilter := toPgTimestamp(from)
	toFilter := toPgTimestamp(to)

	rows, err := s.store.AdminQueryAuditLogs(ctx, db.AdminQueryAuditLogsParams{
		OrganizationID: orgFilter, UserID: userFilter, ActionPatterns: actionPatterns,
		From: fromFilter, To: toFilter, Lim: limit, Off: offset,
	})
	if err != nil {
		return AuditLogsListResponse{}, err
	}

	total, err := s.cachedCount(ctx, auditLogsFilterKey(organizationID, userID, actionPatterns, from, to), func() (int64, error) {
		return s.store.AdminCountAuditLogs(ctx, db.AdminCountAuditLogsParams{
			OrganizationID: orgFilter, UserID: userFilter, ActionPatterns: actionPatterns, From: fromFilter, To: toFilter,
		})
	})
	if err != nil {
		return AuditLogsListResponse{}, err
	}

	items := make([]AuditLogItem, len(rows))
	for i, r := range rows {
		items[i] = AuditLogItem{
			ID:               r.ID,
			OrganizationID:   fromPgUUID(r.OrganizationID),
			OrganizationName: r.OrganizationName,
			UserID:           fromPgUUID(r.UserID),
			UserEmail:        r.UserEmail,
			Action:           r.Action,
			Metadata:         nonEmptyJSON(r.Metadata),
			CreatedAt:        r.CreatedAt,
		}
	}
	return AuditLogsListResponse{Items: items, Total: total}, nil
}

// SystemStats returns the counts behind GET /admin/system/stats (Task 2.8).
// Every count is individually cached under its own fixed key — there is no
// user-supplied filter for this route, but a full stats gather is exactly
// the kind of repeated-COUNT(*) load cachedCount exists to absorb.
func (s *Service) SystemStats(ctx context.Context) (StatsResponse, error) {
	organizations, err := s.cachedCount(ctx, "stats:organizations", func() (int64, error) { return s.store.AdminCountAllOrganizations(ctx) })
	if err != nil {
		return StatsResponse{}, err
	}
	users, err := s.cachedCount(ctx, "stats:users", func() (int64, error) { return s.store.AdminCountAllUsers(ctx) })
	if err != nil {
		return StatsResponse{}, err
	}
	connectors, err := s.cachedCount(ctx, "stats:connectors", func() (int64, error) { return s.store.AdminCountAllConnectors(ctx) })
	if err != nil {
		return StatsResponse{}, err
	}
	mcpKeysTotal, err := s.cachedCount(ctx, "stats:mcpKeysTotal", func() (int64, error) { return s.store.AdminCountAllMCPKeys(ctx) })
	if err != nil {
		return StatsResponse{}, err
	}
	mcpKeysActive, err := s.cachedCount(ctx, "stats:mcpKeysActive", func() (int64, error) { return s.store.AdminCountActiveMCPKeys(ctx) })
	if err != nil {
		return StatsResponse{}, err
	}
	sessionsActive, err := s.cachedCount(ctx, "stats:sessionsActive", func() (int64, error) { return s.store.AdminCountActiveSessions(ctx) })
	if err != nil {
		return StatsResponse{}, err
	}
	auditLogs, err := s.cachedCount(ctx, "stats:auditLogs", func() (int64, error) { return s.store.AdminCountAllAuditLogs(ctx) })
	if err != nil {
		return StatsResponse{}, err
	}

	since := time.Now().Add(-statsLookbackWindow)
	usersLast7d, err := s.cachedCount(ctx, "stats:usersLast7d", func() (int64, error) { return s.store.AdminCountUsersSince(ctx, since) })
	if err != nil {
		return StatsResponse{}, err
	}
	organizationsLast7d, err := s.cachedCount(ctx, "stats:organizationsLast7d", func() (int64, error) {
		return s.store.AdminCountOrganizationsSince(ctx, since)
	})
	if err != nil {
		return StatsResponse{}, err
	}

	emailRows, err := s.store.AdminCountEmailOutboxByStatus(ctx)
	if err != nil {
		return StatsResponse{}, err
	}
	var emailStats EmailOutboxStats
	for _, r := range emailRows {
		switch r.Status {
		case "pending":
			emailStats.Pending = r.StatusCount
		case "sent":
			emailStats.Sent = r.StatusCount
		case "failed":
			emailStats.Failed = r.StatusCount
		}
	}

	planRows, err := s.store.AdminPlanBreakdown(ctx)
	if err != nil {
		return StatsResponse{}, err
	}
	planBreakdown := make([]PlanBreakdownItem, len(planRows))
	for i, r := range planRows {
		planBreakdown[i] = PlanBreakdownItem{PlanName: r.PlanName, OrgCount: r.OrgCount}
	}

	// Redis INFO is a nice-to-have quick health signal, not a reason to
	// fail the whole stats page.
	memHuman, err := s.cache.UsedMemoryHuman(ctx)
	if err != nil {
		s.log.Warn("admin system stats: redis INFO memory failed", "error", err)
		memHuman = ""
	}

	return StatsResponse{
		Organizations:        organizations,
		Users:                users,
		Connectors:           connectors,
		MCPKeysTotal:         mcpKeysTotal,
		MCPKeysActive:        mcpKeysActive,
		SessionsActive:       sessionsActive,
		AuditLogs:            auditLogs,
		EmailOutbox:          emailStats,
		UsersLast7d:          usersLast7d,
		OrganizationsLast7d:  organizationsLast7d,
		PlanBreakdown:        planBreakdown,
		RedisUsedMemoryHuman: memHuman,
	}, nil
}

// ListPlans returns every subscription plan, public and hidden alike.
// Plans is a tiny table with no staff-facing filters, so this bypasses
// cachedCount entirely.
//
// Reads AdminListPlans rather than the tenant catalogue: GET /plans now
// hides is_public = false rows (queries/plans.sql's ListPublicPlans), and
// a console that could not see a plan it had hidden could not un-hide it.
func (s *Service) ListPlans(ctx context.Context) (PlansListResponse, error) {
	plans, err := s.store.AdminListPlans(ctx)
	if err != nil {
		return PlansListResponse{}, err
	}
	items := make([]PlanItem, len(plans))
	for i, p := range plans {
		items[i] = toPlanItem(p)
	}
	return PlansListResponse{Items: items, Total: int64(len(items))}, nil
}

// toPlanItem maps a db.Plan row to its response DTO field by field --
// CLAUDE.md's admin ground rule, and the reason migration 00013's three
// new columns did not appear in a staff response until this function was
// edited to name them.
func toPlanItem(p db.Plan) PlanItem {
	return PlanItem{
		ID:              p.ID,
		Name:            p.Name,
		Limits:          p.Limits,
		StripeProductID: p.StripeProductID,
		IsPublic:        p.IsPublic,
		SortOrder:       p.SortOrder,
		CreatedAt:       p.CreatedAt,
	}
}

// toPlanPriceItem is toPlanItem's plan_prices twin, field by field for the
// same reason.
func toPlanPriceItem(p db.PlanPrice) PlanPriceItem {
	return PlanPriceItem{
		ID:            p.ID,
		PlanID:        p.PlanID,
		StripePriceID: p.StripePriceID,
		UnitAmount:    p.UnitAmount,
		Currency:      p.Currency,
		Interval:      p.Interval,
		Active:        p.Active,
		CreatedAt:     p.CreatedAt,
	}
}

// ==== Mutations (execution plan Phase 3) ====
//
// Every route in this section is superadmin-only (docs/11-admin-panel.md
// §4: support reads everything, mutates nothing) — enforced by the
// handler's separate RequirePlatformRole("superadmin") guard instance, not
// re-checked here; a service trusting its own middleware is the same
// pattern RequireOrg/RequirePermission-guarded modules already use.

// AssignPlan upserts orgID's subscription to planID via
// subscription.Service's existing upsert (Task 3.3) — this module never
// reimplements it. A well-formed but nonexistent planID surfaces as a
// foreign-key violation (500), the same tolerance
// subscription.Service.AssignPlan's own doc comment already accepts.
func (s *Service) AssignPlan(ctx context.Context, actx AdminContext, orgID, planID uuid.UUID) error {
	if err := s.sub.AssignPlan(ctx, orgID, planID); err != nil {
		return err
	}
	s.audit.Record(ctx, auditlog.ActionAdminPlanAssigned, &actx.AdminID, &orgID,
		adminAuditMetadata(actx, map[string]any{"planId": planID}))
	return nil
}

// SetOrgLimits overwrites orgID's org_subscriptions.custom_limits (Task
// 3.3); nil clears back to plan-only limits. Requires an existing
// subscription row — org_subscriptions.plan_id is NOT NULL, so an org with
// no plan ever assigned has nothing for a custom override to attach to,
// and AdminSetOrgCustomLimits's 0-rows-affected result becomes 404 here
// rather than a silent no-op.
func (s *Service) SetOrgLimits(ctx context.Context, actx AdminContext, orgID uuid.UUID, customLimits *map[string]any) error {
	var raw []byte
	if customLimits != nil {
		b, err := json.Marshal(customLimits)
		if err != nil {
			return err
		}
		raw = b
	}

	rows, err := s.store.AdminSetOrgCustomLimits(ctx, db.AdminSetOrgCustomLimitsParams{
		OrganizationID: orgID,
		CustomLimits:   raw,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return apperror.New(apperror.NotFound)
	}

	s.audit.Record(ctx, auditlog.ActionAdminLimitsSet, &actx.AdminID, &orgID,
		adminAuditMetadata(actx, map[string]any{"customLimits": customLimits}))
	return nil
}

// DeleteOrganization deletes orgID after re-authenticating the caller and
// checking confirm against the org's own slug (Task 3.3) — typing the slug
// out is the deliberate friction on an irreversible delete (D4).
// memberships/connectors/mcp_api_keys/org_subscriptions all cascade;
// audit_logs.organization_id carries no FK (migration 00004), so the audit
// trail deliberately survives the org it describes.
//
// That is exactly why the audit write happens BEFORE the DELETE below, not
// after (execution plan Task 3.1's documented asymmetry): a best-effort
// write that failed once the org no longer existed would leave no trace of
// who deleted it, which is a strictly worse outcome than occasionally
// auditing a delete that then itself errors.
func (s *Service) DeleteOrganization(ctx context.Context, actx AdminContext, orgID uuid.UUID, confirm, password string) error {
	if err := s.reauth(ctx, actx.AdminID, password); err != nil {
		return err
	}

	org, err := s.store.AdminGetOrganizationByID(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.NotFound)
		}
		return err
	}
	if confirm != org.Slug {
		return apperror.New(apperror.OrgConfirmMismatch)
	}

	// Audit BEFORE the destructive statement — see doc comment above.
	s.audit.Record(ctx, auditlog.ActionAdminOrgDeleted, &actx.AdminID, &orgID,
		adminAuditMetadata(actx, map[string]any{"slug": org.Slug, "name": org.Name}))

	return s.store.AdminDeleteOrganization(ctx, orgID)
}

// ChangePlatformRole grants or revokes targetID's platform_role (Task 3.4).
// Guards, in order: reauth -> load target (missing -> USER_NOT_FOUND, not a
// silent no-op) -> CANNOT_TARGET_SELF -> (only when promoting to
// "superadmin") SUPERADMIN_LIMIT once superadminCap is reached.
//
// After the write, every session for the target is revoked in the same
// transaction — a demotion also ends the target's tenant sessions
// immediately. No Redis override key is needed the way agritech's
// reference build needed one: platform_role is read fresh from the
// database on every /admin request (D1), so there is no stale JWT claim
// for an override to compensate for.
func (s *Service) ChangePlatformRole(ctx context.Context, actx AdminContext, targetID uuid.UUID, role *string, password string) error {
	if err := s.reauth(ctx, actx.AdminID, password); err != nil {
		return err
	}

	target, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.UserNotFound)
		}
		return err
	}
	if targetID == actx.AdminID {
		return apperror.New(apperror.CannotTargetSelf)
	}

	if role != nil && *role == "superadmin" {
		count, err := s.store.CountSuperadmins(ctx)
		if err != nil {
			return err
		}
		if count >= superadminCap {
			return apperror.New(apperror.SuperadminLimit)
		}
	}

	if err := s.store.WithTx(ctx, func(q db.Querier) error {
		if err := q.SetUserPlatformRole(ctx, db.SetUserPlatformRoleParams{ID: targetID, PlatformRole: role}); err != nil {
			return err
		}
		return q.RevokeAllUserSessions(ctx, targetID)
	}); err != nil {
		return err
	}

	s.audit.Record(ctx, auditlog.ActionAdminPlatformRoleChanged, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{"targetUserId": targetID, "targetEmail": target.Email, "role": role}))
	return nil
}

// SetBan bans or unbans targetID (Task 3.4). Guards, in order: reauth ->
// load target -> CANNOT_TARGET_SELF -> TARGET_IS_PLATFORM_STAFF if the
// target holds any platform_role, applied to both directions (ban and
// unban) exactly as written — demote first, same reasoning
// CANNOT_TARGET_SELF exists for: a still-privileged account should never
// be reachable by this route at all.
//
// Ban: users.banned_at/ban_reason (the durable source of truth, D3) and
// every session for the target are set/revoked in one transaction, then
// the Redis banned:<userId> cache is primed best-effort — a failure there
// self-heals on the target's next login attempt regardless (Login
// re-checks BannedAt and re-primes the cache unconditionally, Task 1.5),
// and any already-issued access token is bounded by JWT_ACCESS_EXPIRES_IN
// (<=15min) either way. Deliberately does NOT touch mcp_api_keys: the
// gateway already refuses a banned owner's key at the RequireMCPKey join
// (Task 1.5), and revoking keys outright is irreversible where a ban is
// not — unbanning should restore exactly the access that existed before.
//
// Unban: the inverse DB write, then Redis Unban — NOT best-effort, unlike
// Ban above. banned:<userId> carries no TTL (internal/infra/redis/auth.go),
// so if this call fails the stale entry has no self-healing path anywhere
// else in the system: the user stays 401'd by Guards.verify despite the
// database now saying they're clear. The error is surfaced so the caller
// (and the operator watching the response) knows to retry.
func (s *Service) SetBan(ctx context.Context, actx AdminContext, targetID uuid.UUID, banned bool, reason *string, password string) error {
	if err := s.reauth(ctx, actx.AdminID, password); err != nil {
		return err
	}

	target, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.UserNotFound)
		}
		return err
	}
	if targetID == actx.AdminID {
		return apperror.New(apperror.CannotTargetSelf)
	}
	if target.PlatformRole != nil {
		return apperror.New(apperror.TargetIsPlatformStaff)
	}

	var bannedAt pgtype.Timestamp
	var banReason *string
	action := auditlog.ActionAdminUserUnbanned
	if banned {
		bannedAt = pgtype.Timestamp{Time: time.Now().UTC(), Valid: true}
		banReason = reason
		action = auditlog.ActionAdminUserBanned
	}

	if err := s.store.WithTx(ctx, func(q db.Querier) error {
		if err := q.SetUserBan(ctx, db.SetUserBanParams{ID: targetID, BannedAt: bannedAt, BanReason: banReason}); err != nil {
			return err
		}
		return q.RevokeAllUserSessions(ctx, targetID)
	}); err != nil {
		return err
	}

	if banned {
		if err := s.redisAuth.Ban(ctx, targetID); err != nil {
			s.log.Warn("admin ban: failed to prime redis ban cache; self-heals on the target's next login attempt", "error", err, "userId", targetID)
		}
	} else if err := s.redisAuth.Unban(ctx, targetID); err != nil {
		return err
	}

	s.audit.Record(ctx, action, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{"targetUserId": targetID, "targetEmail": target.Email, "reason": reason}))
	return nil
}

// requiredPlanLimitKeys are the limit keys upstream code actually enforces
// (subscription.Service.EnforceLimit's callers, and cmd/seed's default
// plans) — every plan must define all four, -1 meaning unlimited. A
// limits blob may carry additional keys beyond these; validatePlanLimits
// only rejects what code elsewhere depends on being present and
// well-typed.
//
// max_tool_calls_per_month joined the list in billing plan step 8, and the
// list's own definition is the argument: step 5 made it enforced on the
// MCP gateway's hot path (internal/module/mcp/service.go calls EnforceLimit
// with it before a tools/call dispatches), so by "the keys upstream code
// actually enforces" it belongs here, and migration 00015 already
// backfilled it onto every seeded plan.
//
// Leaving it out was not neutral. EnforceLimit treats a MISSING key as
// unlimited by design ("No subscription, no limit for key, or a limit of
// -1 ... all pass"), so a superadmin who created a plan through this route
// and simply didn't think about tool calls produced a silently uncapped
// tier — the one limit that maps directly to an upstream API bill, failing
// open. The cost of requiring it is that every create AND update must now
// supply it; a PUT that omits it is a 422 rather than a plan that quietly
// becomes unlimited, which is the trade this list exists to make.
var requiredPlanLimitKeys = []string{"max_members", "max_roles", "max_connectors", "max_tool_calls_per_month"}

// validateLimitValues rejects any limit whose value is not a whole number.
// json.Unmarshal decodes a JSON number into float64, so "integer" is
// checked against the value's own math.Trunc rather than a type assertion
// to int — a type assertion to int would reject every value, since none of
// them are ever actually a Go int after JSON decoding.
//
// This is the half that both plan limits and an org's custom_limits
// override need. It matters more than it looks: everything downstream
// reads these blobs through subscription.Service.EffectiveLimits, which
// unmarshals into map[string]float64 and — on failure — silently drops the
// WHOLE map rather than the one bad key. A single `"max_members": "ten"`
// would therefore disable every other override in the same blob while the
// console kept displaying them as set. Rejecting it at the edge is the
// only place that discrepancy can be caught.
func validateLimitValues(limits map[string]any) error {
	for key, v := range limits {
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return fmt.Errorf("limit %q must be an integer (-1 means unlimited)", key)
		}
	}
	return nil
}

// validatePlanLimits enforces POST/PUT /admin/plans's limits shape: every
// key in requiredPlanLimitKeys present, and every value (required or not)
// a whole number.
func validatePlanLimits(limits map[string]any) error {
	for _, key := range requiredPlanLimitKeys {
		if _, ok := limits[key]; !ok {
			return fmt.Errorf("missing required limit %q", key)
		}
	}
	return validateLimitValues(limits)
}

// validateCustomLimits enforces PUT /admin/organizations/:orgId/limits's
// body shape. Deliberately only the value check, NOT validatePlanLimits:
// custom_limits is a partial overlay on top of the plan's own limits
// (subscription.Service.EffectiveLimits merges custom over plan, custom
// winning per key), so requiring all of requiredPlanLimitKeys here would
// make it impossible to override just max_members and inherit the rest —
// which is the normal reason to set a custom limit at all.
func validateCustomLimits(limits map[string]any) error {
	return validateLimitValues(limits)
}

// PlanInput is the validated, defaults-applied plan body CreatePlan and
// UpdatePlan both take. A struct rather than five positional arguments
// because the last three are a *string, a bool and an int32 that would be
// trivially transposable at a call site, and because create and update
// take exactly the same set (admin.sql's AdminUpdatePlan: a PUT is a full
// replace, not a PATCH).
//
// Defaults for absent optional fields are applied in the handler, not
// here: "absent means published" is a wire-format decision belonging to
// the DTO (PlanCreateRequest's doc comment), and by the time a PlanInput
// exists every field is a decision somebody made.
type PlanInput struct {
	Name            string
	Limits          map[string]any
	StripeProductID *string
	IsPublic        bool
	SortOrder       int32
}

// PlanPriceInput is PlanInput's plan_prices twin: the validated,
// defaults-applied body of POST /admin/plans/:planId/prices. There is no
// update counterpart — a price is never edited in place (see the
// plan_prices block comment further down).
type PlanPriceInput struct {
	StripePriceID string
	UnitAmount    int64
	Currency      string
	Interval      string
	Active        bool
}

// CreatePlan creates a new subscription plan (Task 3.5). No re-auth: D4's
// re-auth list is delete-org/grant-role/ban specifically — plan CRUD
// shapes pricing tiers, not tenant or staff access. Billing plan step 8
// widened what a plan carries (stripe_product_id/is_public/sort_order) but
// did not change that: none of them grant anyone access to anything, and a
// mispriced tier is fixed by another PUT.
func (s *Service) CreatePlan(ctx context.Context, actx AdminContext, in PlanInput) (PlanItem, error) {
	b, err := json.Marshal(in.Limits)
	if err != nil {
		return PlanItem{}, err
	}
	plan, err := s.store.AdminCreatePlan(ctx, db.AdminCreatePlanParams{
		Name:            in.Name,
		Limits:          b,
		StripeProductID: in.StripeProductID,
		IsPublic:        in.IsPublic,
		SortOrder:       in.SortOrder,
	})
	if err != nil {
		return PlanItem{}, err
	}
	s.audit.Record(ctx, auditlog.ActionAdminPlanCreated, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{
			"planId": plan.ID, "name": plan.Name, "isPublic": plan.IsPublic,
		}))
	return toPlanItem(plan), nil
}

// UpdatePlan replaces planID's name, limits, and catalogue columns
// together (Task 3.5) — not a partial PATCH; admin.sql's AdminUpdatePlan
// doc comment and PlanUpdateRequest's have the reasoning and the
// re-publishes-a-hidden-plan consequence.
func (s *Service) UpdatePlan(ctx context.Context, actx AdminContext, planID uuid.UUID, in PlanInput) (PlanItem, error) {
	b, err := json.Marshal(in.Limits)
	if err != nil {
		return PlanItem{}, err
	}
	plan, err := s.store.AdminUpdatePlan(ctx, db.AdminUpdatePlanParams{
		ID:              planID,
		Name:            in.Name,
		Limits:          b,
		StripeProductID: in.StripeProductID,
		IsPublic:        in.IsPublic,
		SortOrder:       in.SortOrder,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanItem{}, apperror.New(apperror.NotFound)
		}
		return PlanItem{}, err
	}
	s.audit.Record(ctx, auditlog.ActionAdminPlanUpdated, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{
			"planId": plan.ID, "name": plan.Name, "isPublic": plan.IsPublic,
		}))
	return toPlanItem(plan), nil
}

// DeletePlan deletes planID (Task 3.5), refusing with PLAN_IN_USE if any
// org_subscriptions row still references it. admin.sql's
// AdminCountSubscriptionsByPlan doc comment has the reasoning: the database
// would reject the delete anyway (plans is referenced ON DELETE no action,
// migration 00005), so this check exists to return a real 409 instead of a
// 500 from a constraint violation.
func (s *Service) DeletePlan(ctx context.Context, actx AdminContext, planID uuid.UUID) error {
	plan, err := s.store.AdminGetPlanByID(ctx, planID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.NotFound)
		}
		return err
	}

	inUse, err := s.store.AdminCountSubscriptionsByPlan(ctx, planID)
	if err != nil {
		return err
	}
	if inUse > 0 {
		return apperror.New(apperror.PlanInUse)
	}

	if err := s.store.AdminDeletePlan(ctx, planID); err != nil {
		return err
	}
	s.audit.Record(ctx, auditlog.ActionAdminPlanDeleted, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{"planId": plan.ID, "name": plan.Name}))
	return nil
}

// ---- plan_prices (billing plan step 8) ----
//
// Three methods, and the two that do not exist are as deliberate as the
// three that do. There is no UpdatePlanPriceAmount and no DeletePlanPrice:
//
//   * A Stripe Price is immutable. Editing unit_amount here would make the
//     local catalogue disagree with the thing that actually charges the
//     card, and the console would then display a price nobody pays.
//   * Deleting a row destroys GetPlanByStripePriceID's ability to resolve
//     an EXISTING subscriber's entitlement plan from the Price on their
//     subscription — including a subscriber sitting on a long-deactivated
//     price, which is the normal state of anyone who bought before the
//     last re-pricing. `active` governs what may be SOLD, not what an
//     existing subscription MEANS, and only a delete conflates the two.
//
// Re-pricing is therefore insert-then-deactivate, and SetPlanPriceActive's
// PLAN_PRICE_LAST_ACTIVE guard enforces that order rather than trusting
// it. queries/admin.sql's plan_prices block comment is the other half of
// this note.

// ListPlanPrices returns every price for planID, active and inactive.
// Verifies the plan exists first so an unknown planId is a 404 rather than
// an empty list — "this plan has no prices" and "this plan does not exist"
// are different answers to a staff member debugging a checkout.
func (s *Service) ListPlanPrices(ctx context.Context, planID uuid.UUID) (PlanPricesListResponse, error) {
	if _, err := s.store.AdminGetPlanByID(ctx, planID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanPricesListResponse{}, apperror.New(apperror.NotFound)
		}
		return PlanPricesListResponse{}, err
	}

	rows, err := s.store.AdminListPlanPrices(ctx, planID)
	if err != nil {
		return PlanPricesListResponse{}, err
	}
	items := make([]PlanPriceItem, len(rows))
	for i, r := range rows {
		items[i] = toPlanPriceItem(r)
	}
	return PlanPricesListResponse{Items: items, Total: int64(len(items))}, nil
}

// CreatePlanPrice records an already-created Stripe Price against planID.
// It does not call Stripe: stripe-go is confined to
// internal/module/billing/stripe.go, and this module deals in strings.
// No re-auth, same reasoning as CreatePlan.
func (s *Service) CreatePlanPrice(ctx context.Context, actx AdminContext, planID uuid.UUID, in PlanPriceInput) (PlanPriceItem, error) {
	if _, err := s.store.AdminGetPlanByID(ctx, planID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanPriceItem{}, apperror.New(apperror.NotFound)
		}
		return PlanPriceItem{}, err
	}

	price, err := s.store.AdminCreatePlanPrice(ctx, db.AdminCreatePlanPriceParams{
		PlanID:          planID,
		StripePriceID:   in.StripePriceID,
		UnitAmount:      in.UnitAmount,
		Currency:        in.Currency,
		BillingInterval: in.Interval,
		Active:          in.Active,
	})
	if err != nil {
		return PlanPriceItem{}, err
	}

	s.audit.Record(ctx, auditlog.ActionAdminPlanPriceCreated, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{
			"planId":        planID,
			"priceId":       price.ID,
			"stripePriceId": price.StripePriceID,
			"unitAmount":    price.UnitAmount,
			"currency":      price.Currency,
			"interval":      price.Interval,
			"active":        price.Active,
		}))
	return toPlanPriceItem(price), nil
}

// SetPlanPriceActive flips priceID's active flag — the only mutation an
// existing price row accepts.
//
// Refuses with PLAN_PRICE_LAST_ACTIVE when deactivating would leave a
// PUBLIC plan with no active price for that currency/interval, because
// GET /plans would keep offering it while POST /billing/checkout answered
// PLAN_NOT_PURCHASABLE to everyone who clicked. Scoped to public plans on
// purpose: a hidden plan is not being sold, so there is nothing to make
// incoherent, and retiring a tier stays possible (hide it, then deactivate
// its prices). Activating is never refused.
func (s *Service) SetPlanPriceActive(ctx context.Context, actx AdminContext, planID, priceID uuid.UUID, active bool) (PlanPriceItem, error) {
	plan, err := s.store.AdminGetPlanByID(ctx, planID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanPriceItem{}, apperror.New(apperror.NotFound)
		}
		return PlanPriceItem{}, err
	}

	current, err := s.store.AdminGetPlanPrice(ctx, db.AdminGetPlanPriceParams{ID: priceID, PlanID: planID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanPriceItem{}, apperror.New(apperror.NotFound)
		}
		return PlanPriceItem{}, err
	}

	// Only a real active -> inactive transition can strand a public plan.
	// Re-deactivating an already-inactive price is a no-op that must not
	// be rejected, or a retried request would fail where the first
	// succeeded.
	if !active && current.Active && plan.IsPublic {
		remaining, err := s.store.AdminCountActivePlanPricesFor(ctx, db.AdminCountActivePlanPricesForParams{
			PlanID:          planID,
			Currency:        current.Currency,
			BillingInterval: current.Interval,
		})
		if err != nil {
			return PlanPriceItem{}, err
		}
		if remaining <= 1 {
			return PlanPriceItem{}, apperror.New(apperror.PlanPriceLastActive)
		}
	}

	price, err := s.store.AdminSetPlanPriceActive(ctx, db.AdminSetPlanPriceActiveParams{
		ID: priceID, PlanID: planID, Active: active,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlanPriceItem{}, apperror.New(apperror.NotFound)
		}
		return PlanPriceItem{}, err
	}

	action := auditlog.ActionAdminPlanPriceDeactivated
	if active {
		action = auditlog.ActionAdminPlanPriceActivated
	}
	s.audit.Record(ctx, action, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{
			"planId":        planID,
			"priceId":       price.ID,
			"stripePriceId": price.StripePriceID,
			"currency":      price.Currency,
			"interval":      price.Interval,
		}))
	return toPlanPriceItem(price), nil
}

// ---- filter-key builders ----
//
// Each mirrors the exact filter set its list method's cachedCount call
// depends on — see countcache.go's doc comment on why limit/offset must
// never appear here.

func organizationsFilterKey(search *string) string {
	return "organizations:search=" + derefStr(search)
}

func usersFilterKey(search, role *string, banned *bool) string {
	return "users:search=" + derefStr(search) + "&role=" + derefStr(role) + "&banned=" + derefBool(banned)
}

func connectorsFilterKey(organizationID *uuid.UUID, connType, status, search *string) string {
	return "connectors:org=" + derefUUID(organizationID) + "&type=" + derefStr(connType) + "&status=" + derefStr(status) + "&search=" + derefStr(search)
}

func mcpKeysFilterKey(organizationID, userID *uuid.UUID, search *string) string {
	return "mcpkeys:org=" + derefUUID(organizationID) + "&user=" + derefUUID(userID) + "&search=" + derefStr(search)
}

func auditLogsFilterKey(organizationID, userID *uuid.UUID, actionPatterns []string, from, to *time.Time) string {
	var fromStr, toStr string
	if from != nil {
		fromStr = from.UTC().Format(time.RFC3339Nano)
	}
	if to != nil {
		toStr = to.UTC().Format(time.RFC3339Nano)
	}
	return "auditlogs:org=" + derefUUID(organizationID) + "&user=" + derefUUID(userID) +
		"&actions=" + strings.Join(actionPatterns, ",") + "&from=" + fromStr + "&to=" + toStr
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefBool(b *bool) string {
	if b == nil {
		return "nil"
	}
	return strconv.FormatBool(*b)
}

func derefUUID(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// ---- row -> DTO mapping ----

func toConnectorItem(r db.AdminListConnectorsRow) ConnectorItem {
	return ConnectorItem{
		ID:                r.ID,
		OrganizationID:    r.OrganizationID,
		OrganizationName:  r.OrganizationName,
		Name:              r.Name,
		Type:              r.Type,
		Status:            r.Status,
		LastHealthCheckAt: fromPgTimestamp(r.LastHealthCheckAt),
		CreatedAt:         r.CreatedAt,
	}
}

func toMCPKeyItem(r db.AdminListMCPKeysRow) MCPKeyItem {
	return MCPKeyItem{
		ID:               r.ID,
		OrganizationID:   r.OrganizationID,
		OrganizationName: r.OrganizationName,
		UserID:           r.UserID,
		UserEmail:        r.UserEmail,
		Name:             r.Name,
		Scopes:           r.Scopes,
		LastUsedAt:       fromPgTimestamp(r.LastUsedAt),
		ExpiresAt:        fromPgTimestamp(r.ExpiresAt),
		RevokedAt:        fromPgTimestamp(r.RevokedAt),
		CreatedAt:        r.CreatedAt,
	}
}

// ---- pgtype <-> Go conversions ----
//
// Each admin module keeps its own copies of these small conversions
// (mirroring auditlog's and connector's own private helpers) rather than
// sharing one package-wide utility — see those modules' own fromPgUUID/
// fromPgTimestamp doc comments.

func toPgUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

func fromPgUUID(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	u := uuid.UUID(id.Bytes)
	return &u
}

func toPgTimestamp(t *time.Time) pgtype.Timestamp {
	if t == nil {
		return pgtype.Timestamp{}
	}
	return pgtype.Timestamp{Time: *t, Valid: true}
}

func fromPgTimestamp(t pgtype.Timestamp) *time.Time {
	if !t.Valid {
		return nil
	}
	tm := t.Time
	return &tm
}

// nonEmptyJSON coerces an empty/nil jsonb column to a nil json.RawMessage
// so it marshals as JSON null; json.Marshal panics on a non-nil empty
// []byte cast directly to json.RawMessage. Mirrors auditlog's own helper.
func nonEmptyJSON(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// buildActionPatterns converts the repeatable ?action= filter
// (execution plan Task 2.7) into LIKE patterns for AdminQueryAuditLogs/
// AdminCountAuditLogs: a trailing '*' means prefix match ("admin.*" ->
// "admin.%"), anything else matches by equality (a LIKE pattern with no
// wildcard characters behaves as equality). '%'/'_' in the literal portion
// are escaped so a user-supplied action can never be misread as a pattern.
//
// Returns nil for an empty/absent filter — AdminQueryAuditLogs's
// `action_patterns IS NULL` branch treats that as "no filter". Never
// returns a non-nil empty slice, which would bind an empty array and match
// nothing via `LIKE ANY('{}')` — the same nil-vs-empty-slice trap
// QueryAuditLogs's own comment documents for its actions text[] param.
func buildActionPatterns(actions []string) []string {
	if len(actions) == 0 {
		return nil
	}
	patterns := make([]string, len(actions))
	for i, a := range actions {
		prefix, isPrefix := strings.CutSuffix(a, "*")
		patterns[i] = escapeLike(prefix)
		if isPrefix {
			patterns[i] += "%"
		}
	}
	return patterns
}

// escapeLike escapes the characters LIKE treats specially so a literal
// action string can never be misread as a pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ==== Impersonation (execution plan Phase 4) ====

// Impersonate mints a short-lived, read-only token authenticating AS
// targetID, on behalf of the staff member in actx (docs/11-admin-panel.md
// §5). Available to support as well as superadmin: it grants no more than
// support already has — read access — and reading as the user is often the
// only way to reproduce a "my MCP client 401s" report.
//
// No password re-auth, deliberately (execution plan Task 4.4): the
// operation is read-only, and prompting for a password on a routine support
// action trains staff to type it reflexively, which makes the prompts on
// the operations that DO need it (delete-org, ban, role change) worth less.
//
// Refusals, in order: unknown user, then platform staff, then banned. Staff
// is checked before banned so that a banned staff account reports
// CANNOT_IMPERSONATE_STAFF rather than leaking that the account is banned
// — the staff refusal is the stronger statement and the one the caller
// must act on (demote first).
func (s *Service) Impersonate(ctx context.Context, actx AdminContext, targetID uuid.UUID, reason string) (ImpersonateResponse, error) {
	target, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ImpersonateResponse{}, apperror.New(apperror.UserNotFound)
		}
		return ImpersonateResponse{}, err
	}

	// Impersonation must never be a privilege-escalation ladder: a staff
	// account is off limits regardless of which role it holds, so support
	// cannot borrow a superadmin's identity and superadmins cannot quietly
	// act as one another.
	if target.PlatformRole != nil {
		return ImpersonateResponse{}, apperror.New(apperror.CannotImpersonateStaff)
	}

	// A banned user's own credentials are dead (Guards.verify 401s them);
	// minting a working token for that identity would route around the ban.
	if target.BannedAt.Valid {
		return ImpersonateResponse{}, apperror.New(apperror.AccountSuspended)
	}

	token, err := s.signer.SignImpersonationToken(target.ID, target.Email, actx.AdminID, impersonationTTL)
	if err != nil {
		return ImpersonateResponse{}, err
	}

	// Audited with the mandatory reason: the control on impersonation is
	// detection, not prevention (docs/11-admin-panel.md §5), so this write
	// is the entire accountability story for the next 10 minutes.
	s.audit.Record(ctx, auditlog.ActionAdminImpersonationStarted, &actx.AdminID, nil,
		adminAuditMetadata(actx, map[string]any{
			"targetUserId": targetID,
			"targetEmail":  target.Email,
			"reason":       reason,
		}))

	return ImpersonateResponse{
		AccessToken: token,
		ExpiresIn:   int(impersonationTTL.Seconds()),
		User: ImpersonatedUser{
			ID:          target.ID,
			Email:       target.Email,
			DisplayName: target.DisplayName,
		},
	}, nil
}
