package admin

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// defaultListLimit and maxListLimit bound every admin list endpoint's
// ?limit= except GET /admin/audit-logs, whose own cap is higher (see
// AuditLogsQuery) per the execution plan's Task 2.7.
const (
	defaultListLimit = 50
	maxListLimit     = 100
)

// ListQuery is the ?limit=/?offset=/?search= shape shared by every admin
// list endpoint (execution plan Task 2.2). Per-route query structs embed it
// and add their own filters. limit()/offset()/searchFilter() apply the
// documented defaults so the service layer never sees a raw pointer.
type ListQuery struct {
	Limit  *int    `query:"limit" validate:"omitempty,min=1,max=100"`
	Offset *int    `query:"offset" validate:"omitempty,min=0"`
	Search *string `query:"search"`
}

func (q ListQuery) limit() int32 {
	if q.Limit == nil {
		return defaultListLimit
	}
	return int32(*q.Limit)
}

func (q ListQuery) offset() int32 {
	if q.Offset == nil {
		return 0
	}
	return int32(*q.Offset)
}

// searchFilter normalizes an absent or empty ?search= to nil, so both bind
// the same "no filter" SQL predicate (admin.sql's `sqlc.narg('search') IS
// NULL` branch) rather than an empty-string ILIKE that would still (harmlessly,
// but pointlessly) touch every row.
func (q ListQuery) searchFilter() *string {
	if q.Search == nil || *q.Search == "" {
		return nil
	}
	return q.Search
}

// MeResponse is the GET /admin/me body: just enough for the frontend's
// layout guard to render the console shell and the caller's own role.
type MeResponse struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	DisplayName  *string   `json:"displayName"`
	PlatformRole string    `json:"platformRole"`
}

// ---- Organizations ----

// OrganizationsQuery is the GET /admin/organizations query string.
type OrganizationsQuery struct {
	ListQuery
}

// OrganizationListItem is one row of GET /admin/organizations. PlanName is
// nil for an org with no subscription.
type OrganizationListItem struct {
	ID             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	Slug           string    `json:"slug"`
	CreatedAt      time.Time `json:"createdAt"`
	MemberCount    int64     `json:"memberCount"`
	ConnectorCount int64     `json:"connectorCount"`
	MCPKeyCount    int64     `json:"mcpKeyCount"`
	PlanName       *string   `json:"planName"`
}

// OrganizationsListResponse is GET /admin/organizations's body.
type OrganizationsListResponse struct {
	Items []OrganizationListItem `json:"items"`
	Total int64                  `json:"total"`
}

// OrgMemberItem is one entry of OrganizationDetailResponse.Members.
type OrgMemberItem struct {
	UserID      uuid.UUID `json:"userId"`
	Email       string    `json:"email"`
	DisplayName *string   `json:"displayName"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joinedAt"`
}

// OrgAuditLogItem is one entry of OrganizationDetailResponse.RecentAuditLogs
// — the org is already known from the surrounding response, so unlike
// AuditLogItem (the cross-org GET /admin/audit-logs shape) this carries no
// organization name/id.
type OrgAuditLogItem struct {
	ID        uuid.UUID       `json:"id"`
	UserID    *uuid.UUID      `json:"userId"`
	Action    string          `json:"action"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"createdAt"`
}

// OrganizationDetailResponse is GET /admin/organizations/:orgId's body.
// EffectiveLimits and PlanName come from subscription.Service (Task 2.4)
// rather than this module re-deriving the custom-over-plan merge; both are
// nil/empty for an org with no subscription. Connectors and MCPKeys are
// metadata-only (docs/11-admin-panel.md §7) — same item shapes as the
// cross-org GET /admin/connectors and GET /admin/mcp-keys.
type OrganizationDetailResponse struct {
	ID              uuid.UUID          `json:"id"`
	Name            string             `json:"name"`
	Slug            string             `json:"slug"`
	CreatedAt       time.Time          `json:"createdAt"`
	UpdatedAt       time.Time          `json:"updatedAt"`
	PlanName        *string            `json:"planName"`
	EffectiveLimits map[string]float64 `json:"effectiveLimits"`
	Members         []OrgMemberItem    `json:"members"`
	Connectors      []ConnectorItem    `json:"connectors"`
	MCPKeys         []MCPKeyItem       `json:"mcpKeys"`
	RecentAuditLogs []OrgAuditLogItem  `json:"recentAuditLogs"`
}

// ---- Users ----

// UsersQuery is the GET /admin/users query string. Role takes
// "superadmin"/"support"/"none" ("none" meaning platformRole is unset);
// Banned filters on whether bannedAt is set.
type UsersQuery struct {
	ListQuery
	Role   *string `query:"role" validate:"omitempty,oneof=superadmin support none"`
	Banned *bool   `query:"banned"`
}

// UserListItem is one row of GET /admin/users.
type UserListItem struct {
	ID           uuid.UUID  `json:"id"`
	Email        string     `json:"email"`
	DisplayName  *string    `json:"displayName"`
	IsVerified   bool       `json:"isVerified"`
	PlatformRole *string    `json:"platformRole"`
	BannedAt     *time.Time `json:"bannedAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	OrgCount     int64      `json:"orgCount"`
}

// UsersListResponse is GET /admin/users's body.
type UsersListResponse struct {
	Items []UserListItem `json:"items"`
	Total int64          `json:"total"`
}

// UserMembershipItem is one entry of UserDetailResponse.Memberships.
type UserMembershipItem struct {
	OrganizationID   uuid.UUID `json:"organizationId"`
	OrganizationName string    `json:"organizationName"`
	OrganizationSlug string    `json:"organizationSlug"`
	Role             string    `json:"role"`
	JoinedAt         time.Time `json:"joinedAt"`
}

// UserDetailResponse is GET /admin/users/:userId's body. Never carries
// PasswordHash — mapped explicitly field-by-field from db.User, never a
// struct embed (Task 2.5; the same hazard CLAUDE.md's log-redaction rule
// calls out for a whole request/body struct, applied to serialization).
type UserDetailResponse struct {
	ID             uuid.UUID            `json:"id"`
	Email          string               `json:"email"`
	DisplayName    *string              `json:"displayName"`
	IsVerified     bool                 `json:"isVerified"`
	PlatformRole   *string              `json:"platformRole"`
	BannedAt       *time.Time           `json:"bannedAt"`
	BanReason      *string              `json:"banReason"`
	CreatedAt      time.Time            `json:"createdAt"`
	Memberships    []UserMembershipItem `json:"memberships"`
	ActiveSessions int64                `json:"activeSessions"`
}

// ---- Connectors ----

// ConnectorsQuery is the GET /admin/connectors query string.
type ConnectorsQuery struct {
	ListQuery
	OrganizationID *string `query:"organizationId" validate:"omitempty,uuid"`
	Type           *string `query:"type"`
	Status         *string `query:"status" validate:"omitempty,oneof=active inactive error"`
}

// ConnectorItem is one row of GET /admin/connectors, and also one entry of
// OrganizationDetailResponse.Connectors. Metadata only, per
// docs/11-admin-panel.md §7 and CLAUDE.md: no encrypted_config, no
// decrypted config, not even a "config present" boolean beyond what Status
// already implies. Field set matches connector.ConnectorResponse plus the
// two cross-org identifiers.
type ConnectorItem struct {
	ID                uuid.UUID  `json:"id"`
	OrganizationID    uuid.UUID  `json:"organizationId"`
	OrganizationName  string     `json:"organizationName"`
	Name              string     `json:"name"`
	Type              string     `json:"type"`
	Status            string     `json:"status"`
	LastHealthCheckAt *time.Time `json:"lastHealthCheckAt"`
	CreatedAt         time.Time  `json:"createdAt"`
}

// ConnectorsListResponse is GET /admin/connectors's body.
type ConnectorsListResponse struct {
	Items []ConnectorItem `json:"items"`
	Total int64           `json:"total"`
}

// ---- MCP keys ----

// MCPKeysQuery is the GET /admin/mcp-keys query string.
type MCPKeysQuery struct {
	ListQuery
	OrganizationID *string `query:"organizationId" validate:"omitempty,uuid"`
	UserID         *string `query:"userId" validate:"omitempty,uuid"`
}

// MCPKeyItem is one row of GET /admin/mcp-keys, and also one entry of
// OrganizationDetailResponse.MCPKeys. Never carries KeyHash or a raw token
// (docs/11-admin-panel.md §7).
type MCPKeyItem struct {
	ID               uuid.UUID  `json:"id"`
	OrganizationID   uuid.UUID  `json:"organizationId"`
	OrganizationName string     `json:"organizationName"`
	UserID           uuid.UUID  `json:"userId"`
	UserEmail        string     `json:"userEmail"`
	Name             string     `json:"name"`
	Scopes           []string   `json:"scopes"`
	LastUsedAt       *time.Time `json:"lastUsedAt"`
	ExpiresAt        *time.Time `json:"expiresAt"`
	RevokedAt        *time.Time `json:"revokedAt"`
	CreatedAt        time.Time  `json:"createdAt"`
}

// MCPKeysListResponse is GET /admin/mcp-keys's body.
type MCPKeysListResponse struct {
	Items []MCPKeyItem `json:"items"`
	Total int64        `json:"total"`
}

// ---- Cross-org audit logs ----

// AuditLogsQuery is the GET /admin/audit-logs query string. Actions is
// repeatable (?action=a&action=b); a trailing '*' on any entry means prefix
// match. From/To are bound as raw strings, not *time.Time, for the same
// reason auditlog.QueryParams.Since is (internal/module/auditlog/dto.go):
// a *time.Time field's parse failure inside echo's binder surfaces as 400
// "Invalid request body" rather than the contract's 422 "Validation
// failed", so parsing happens explicitly in the handler instead. Limit's
// max is 200, not the generic shared shape's 100 (execution plan Task 2.7).
type AuditLogsQuery struct {
	OrganizationID *string  `query:"organizationId" validate:"omitempty,uuid"`
	UserID         *string  `query:"userId" validate:"omitempty,uuid"`
	Actions        []string `query:"action"`
	From           *string  `query:"from"`
	To             *string  `query:"to"`
	Limit          *int     `query:"limit" validate:"omitempty,min=1,max=200"`
	Offset         *int     `query:"offset" validate:"omitempty,min=0"`
}

func (q AuditLogsQuery) limit() int32 {
	if q.Limit == nil {
		return defaultListLimit
	}
	return int32(*q.Limit)
}

func (q AuditLogsQuery) offset() int32 {
	if q.Offset == nil {
		return 0
	}
	return int32(*q.Offset)
}

// AuditLogItem is one row of GET /admin/audit-logs: a plain audit_logs row
// plus the joined names a console needs to show instead of bare UUIDs.
// OrganizationName/UserEmail/OrganizationID/UserID are nil when the
// underlying columns are null (a system-actor entry with no user, or one
// recorded before the referenced row's own deletion is even possible in
// practice today, since neither organizations nor users cascade-null these
// columns — kept nullable to mirror the schema honestly).
type AuditLogItem struct {
	ID               uuid.UUID       `json:"id"`
	OrganizationID   *uuid.UUID      `json:"organizationId"`
	OrganizationName *string         `json:"organizationName"`
	UserID           *uuid.UUID      `json:"userId"`
	UserEmail        *string         `json:"userEmail"`
	Action           string          `json:"action"`
	Metadata         json.RawMessage `json:"metadata"`
	CreatedAt        time.Time       `json:"createdAt"`
}

// AuditLogsListResponse is GET /admin/audit-logs's body. Deliberately NOT
// the bare array the tenant-side GET /audit-logs returns — see execution
// plan Task 2.2; do not "fix" this to match that route or vice versa.
type AuditLogsListResponse struct {
	Items []AuditLogItem `json:"items"`
	Total int64          `json:"total"`
}

// ---- System stats ----

// EmailOutboxStats is GET /admin/system/stats's emailOutbox breakdown. A
// rising Failed count is the single best early warning that Resend or the
// EMAIL_FROM domain is misconfigured (CLAUDE.md's Background worker
// bullet).
type EmailOutboxStats struct {
	Pending int64 `json:"pending"`
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
}

// PlanBreakdownItem is one entry of StatsResponse.PlanBreakdown.
type PlanBreakdownItem struct {
	PlanName string `json:"planName"`
	OrgCount int64  `json:"orgCount"`
}

// StatsResponse is GET /admin/system/stats's body.
type StatsResponse struct {
	Organizations        int64               `json:"organizations"`
	Users                int64               `json:"users"`
	Connectors           int64               `json:"connectors"`
	MCPKeysTotal         int64               `json:"mcpKeysTotal"`
	MCPKeysActive        int64               `json:"mcpKeysActive"`
	SessionsActive       int64               `json:"sessionsActive"`
	AuditLogs            int64               `json:"auditLogs"`
	EmailOutbox          EmailOutboxStats    `json:"emailOutbox"`
	UsersLast7d          int64               `json:"usersLast7d"`
	OrganizationsLast7d  int64               `json:"organizationsLast7d"`
	PlanBreakdown        []PlanBreakdownItem `json:"planBreakdown"`
	RedisUsedMemoryHuman string              `json:"redisUsedMemoryHuman"`
}

// ---- Plans ----

// PlanItem is one row of GET /admin/plans.
type PlanItem struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Limits    json.RawMessage `json:"limits"`
	CreatedAt time.Time       `json:"createdAt"`
}

// PlansListResponse is GET /admin/plans's body. Plans is a tiny, seeded
// table (no staff-facing filters), so Total is simply len(Items) — it is
// not routed through cachedCount.
type PlansListResponse struct {
	Items []PlanItem `json:"items"`
	Total int64      `json:"total"`
}

// ---- Mutations (execution plan Phase 3) ----
//
// Every request DTO below that embeds a Password field is bound by
// httpx.BindAndValidate/BindBodyAndValidate straight off the wire — it must
// never be logged as a whole struct (CLAUDE.md's "log individual fields,
// never a whole request/body struct" rule, which centralized redaction in
// internal/shared/logger/redact.go cannot rescue here since it only matches
// attribute *keys*, and a struct logged via slog.Any("body", req) would
// serialize Password in full regardless of key-based redaction elsewhere).
// No admin handler in this package logs a bound request struct.

// SuccessResponse is the response body for every admin mutation that has
// no more interesting result to report than "it happened" — mirrors the
// SuccessResponse shape organization/subscription/mcpkey already use for
// the same kind of route.
type SuccessResponse struct {
	Success bool `json:"success"`
}

// AssignPlanRequest is the POST /admin/organizations/:orgId/plan body.
type AssignPlanRequest struct {
	PlanID string `json:"planId" validate:"required,uuid"`
}

// SetLimitsRequest is the PUT /admin/organizations/:orgId/limits body.
// CustomLimits is a pointer so JSON null (or an absent field — Go's
// encoding/json treats the two identically for a pointer target) and a
// present object are both representable: nil clears back to plan-only
// limits, non-nil sets/replaces the override.
//
// A present object goes through validateCustomLimits in the handler —
// every value must be a whole number, though (unlike a plan's own limits)
// no particular key is required, since this is a partial overlay.
type SetLimitsRequest struct {
	CustomLimits *map[string]any `json:"customLimits"`
}

// DeleteOrganizationRequest is the DELETE /admin/organizations/:orgId body.
// Confirm must equal the target organization's slug — typing it out is the
// friction that is deliberately the feature on an irreversible delete
// (docs/11-admin-panel.md D4). Password is the caller's own, re-verified by
// reauth before anything destructive happens.
type DeleteOrganizationRequest struct {
	Confirm  string `json:"confirm" validate:"required"`
	Password string `json:"password" validate:"required"`
}

// PlatformRoleRequest is the PATCH /admin/users/:userId/platform-role body.
// Role is nil to revoke platform staff status entirely (cmd/grantadmin's
// "-role none" as a JSON null/absent field), or "superadmin"/"support" to
// grant it.
type PlatformRoleRequest struct {
	Role     *string `json:"role" validate:"omitempty,oneof=superadmin support"`
	Password string  `json:"password" validate:"required"`
}

// BanRequest is the PATCH /admin/users/:userId/ban body. Reason is optional
// on unban (Banned: false) and conventionally set on ban, but nothing
// enforces that at the type level — the service doesn't care which
// direction Reason travels with.
type BanRequest struct {
	Banned   bool    `json:"banned"`
	Reason   *string `json:"reason" validate:"omitempty,max=500"`
	Password string  `json:"password" validate:"required"`
}

// PlanCreateRequest is the POST /admin/plans body.
type PlanCreateRequest struct {
	Name   string         `json:"name" validate:"required,min=1,max=100"`
	Limits map[string]any `json:"limits" validate:"required"`
}

// PlanUpdateRequest is the PUT /admin/plans/:planId body — a full replace
// of name+limits together (admin.sql's AdminUpdatePlan doc comment).
type PlanUpdateRequest struct {
	Name   string         `json:"name" validate:"required,min=1,max=100"`
	Limits map[string]any `json:"limits" validate:"required"`
}

// ---- Impersonation (execution plan Phase 4) ----

// ImpersonateRequest is the POST /admin/users/:userId/impersonate body.
// Reason is mandatory and minimum 10 characters (docs/11-admin-panel.md
// §5): impersonation is controlled by detection rather than prevention, so
// a reason that is actually written down is the control. The minimum length
// exists to make "x" or "-" fail rather than sail through as a reason.
type ImpersonateRequest struct {
	Reason string `json:"reason" validate:"required,min=10,max=500"`
}

// ImpersonatedUser is the target identity echoed back so the console can
// show who the staff member is now acting as. Metadata only — the same
// fields GET /admin/users already returns.
type ImpersonatedUser struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName *string   `json:"displayName"`
}

// ImpersonateResponse carries the minted token. There is deliberately NO
// refreshToken field: an impersonation token cannot be extended, only
// re-issued through this endpoint, and each re-issue writes its own audit
// entry (docs/11-admin-panel.md §5). ExpiresIn is seconds, matching the
// units POST /auth/login already uses on the wire.
type ImpersonateResponse struct {
	AccessToken string           `json:"accessToken"`
	ExpiresIn   int              `json:"expiresIn"`
	User        ImpersonatedUser `json:"user"`
}
