// Package admin implements the /admin module: cross-org READ surfaces for
// the platform staff console (docs/11-admin-panel.md). Every route is
// guarded by internal/middleware.Guards.RequirePlatformRole rather than
// RequireOrg/RequirePermission — platform staff typically hold no
// membership anywhere — and every query in
// internal/infra/database/queries/admin.sql deliberately carries no
// organization_id predicate the way tenant-facing queries do. That is the
// whole point of this module, not an oversight.
//
// This file implements only the read surfaces: execution plan
// (.claude/plans/2026-08-28-admin-panel.md) Phase 2, Tasks 2.1-2.10. The
// superadmin-only mutation routes (grant/revoke platform_role, ban,
// plan/limit changes, impersonation, ...) are later phases and are not
// implemented here.
//
// Decrypted connector config never appears anywhere in this package —
// ConnectorItem is metadata only (id, organizationId, organizationName,
// name, type, status, lastHealthCheckAt, createdAt), and no query here
// ever selects connectors.encrypted_config or mcp_api_keys.key_hash.
package admin

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

var (
	_ adminStore           = (*database.Store)(nil)
	_ subscriptionResolver = (*subscription.Service)(nil)
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
}

// subscriptionResolver is the subset of *subscription.Service the
// organization-detail view depends on. GetSubscription/EffectiveLimits
// already implement the custom-over-plan merge — Task 2.4 calls into them
// rather than reimplementing it here.
type subscriptionResolver interface {
	GetSubscription(ctx context.Context, organizationID uuid.UUID) (*db.GetOrgSubscriptionRow, error)
	EffectiveLimits(ctx context.Context, organizationID uuid.UUID) (map[string]float64, error)
	ListPlans(ctx context.Context) ([]db.Plan, error)
}

// Service implements the admin module's read surfaces.
type Service struct {
	store adminStore
	cache countCache
	audit *auditlog.Service
	sub   subscriptionResolver
	log   *slog.Logger
}

// NewService builds an admin Service from its explicit dependencies — no
// service locator (execution plan Task 2.9). The token service Phase 4
// (impersonation) needs is not wired in yet; this phase only reads.
func NewService(store adminStore, cache countCache, audit *auditlog.Service, sub subscriptionResolver, log *slog.Logger) *Service {
	return &Service{store: store, cache: cache, audit: audit, sub: sub, log: log}
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

// ListPlans returns every subscription plan. Plans is a tiny, seeded table
// with no staff-facing filters, so this bypasses cachedCount entirely.
func (s *Service) ListPlans(ctx context.Context) (PlansListResponse, error) {
	plans, err := s.sub.ListPlans(ctx)
	if err != nil {
		return PlansListResponse{}, err
	}
	items := make([]PlanItem, len(plans))
	for i, p := range plans {
		items[i] = PlanItem{ID: p.ID, Name: p.Name, Limits: p.Limits, CreatedAt: p.CreatedAt}
	}
	return PlansListResponse{Items: items, Total: int64(len(items))}, nil
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
