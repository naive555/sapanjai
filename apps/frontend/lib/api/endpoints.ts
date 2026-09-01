import { apiRequest } from "./client";

export interface TokenResponse {
  accessToken: string;
  refreshToken: string;
}

export interface SuccessResponse {
  success: true;
}

// ---- Auth ----

export function register(input: { email: string; password: string; displayName?: string }) {
  return apiRequest<TokenResponse>("/auth/register", { method: "POST", body: input, noRetry: true });
}

export function login(input: { email: string; password: string }) {
  return apiRequest<TokenResponse>("/auth/login", { method: "POST", body: input, noRetry: true });
}

export function refresh(input: { refreshToken: string }) {
  return apiRequest<TokenResponse>("/auth/refresh", { method: "POST", body: input, noRetry: true });
}

export function logout(input: { refreshToken: string }) {
  return apiRequest<SuccessResponse>("/auth/logout", { method: "POST", body: input, noRetry: true });
}

export interface MeResponse {
  id: string;
  email: string;
  displayName: string | null;
  isVerified: boolean;
  // Nullable for every tenant user — this is the dashboard nav's own signal
  // for whether to render the "Admin" entry (lib/auth/use-session.tsx),
  // distinct from GET /admin/me below, which is the admin console guard's
  // own call (docs/11-admin-panel.md §1). It also happens to be how the
  // nav correctly hides itself while impersonating: /auth/me resolves to
  // the impersonated TARGET, who by construction can never hold a
  // platformRole (docs/11-admin-panel.md §5's CANNOT_IMPERSONATE_STAFF).
  platformRole: "superadmin" | "support" | null;
  createdAt: string;
}

// Authenticated — normal 401-retry-after-refresh semantics apply, unlike the
// four calls above.
export function me() {
  return apiRequest<MeResponse>("/auth/me");
}

// Public, single-use token in the body — a 401 here can't mean "stale access
// token" (there's no bearer token in play at all), so noRetry.
export function verifyEmail(input: { token: string }) {
  return apiRequest<SuccessResponse>("/auth/verify-email", { method: "POST", body: input, noRetry: true });
}

// Authenticated, no body — resends a link for the caller's own account.
export function resendVerification() {
  return apiRequest<SuccessResponse>("/auth/resend-verification", { method: "POST" });
}

// Public, always 200-shaped for enumeration safety (docs/02-api-contract.md)
// — the UI still has to swallow a network/5xx failure itself to preserve
// that shape end-to-end. noRetry: unauthenticated, same reasoning as above.
export function forgotPassword(input: { email: string }) {
  return apiRequest<SuccessResponse>("/auth/forgot-password", { method: "POST", body: input, noRetry: true });
}

// Public, single-use token in the body — noRetry, same reasoning as verifyEmail.
export function resetPassword(input: { token: string; password: string }) {
  return apiRequest<SuccessResponse>("/auth/reset-password", { method: "POST", body: input, noRetry: true });
}

// ---- Organizations ----

export interface OrgResponse {
  id: string;
  name: string;
  slug: string;
  createdAt: string;
  updatedAt: string;
}

export interface MembershipResponse {
  id: string;
  userId: string;
  organizationId: string;
  role: string;
  createdAt: string;
  organization: OrgResponse;
}

export interface MemberResponse {
  userId: string;
  email: string;
  displayName: string | null;
  role: string;
  joinedAt: string;
}

export function createOrganization(input: { name: string; slug: string }) {
  return apiRequest<OrgResponse>("/organizations", { method: "POST", body: input });
}

export function listOrganizations() {
  return apiRequest<MembershipResponse[]>("/organizations");
}

export function listMembers() {
  return apiRequest<MemberResponse[]>("/organizations/members");
}

export function invite(input: { email: string; role: "admin" | "member" }) {
  return apiRequest<SuccessResponse>("/organizations/invite", { method: "POST", body: input });
}

export function removeMember(userId: string) {
  return apiRequest<SuccessResponse>(`/organizations/members/${userId}`, { method: "DELETE" });
}

// ---- RBAC ----

export interface PermissionResponse {
  id: string;
  roleId: string;
  action: string;
  createdAt: string;
}

export interface RoleResponse {
  id: string;
  organizationId: string;
  name: string;
  description: string | null;
  createdAt: string;
  permissions: PermissionResponse[];
}

export interface RoleRowResponse {
  id: string;
  organizationId: string;
  name: string;
  description: string | null;
  createdAt: string;
}

export function listRoles() {
  return apiRequest<RoleResponse[]>("/rbac/roles");
}

export function createRole(input: { name: string; description?: string; permissions: string[] }) {
  return apiRequest<RoleRowResponse>("/rbac/roles", { method: "POST", body: input });
}

export function updatePermissions(roleId: string, permissions: string[]) {
  return apiRequest<SuccessResponse>(`/rbac/roles/${roleId}/permissions`, {
    method: "PUT",
    body: { permissions },
  });
}

export function assignRole(input: { userId: string; roleId: string }) {
  return apiRequest<SuccessResponse>("/rbac/assign", { method: "POST", body: input });
}

// ---- Subscription ----

export interface PlanResponse {
  id: string;
  name: string;
  limits: Record<string, unknown>;
  createdAt: string;
}

export interface SubscriptionResponse {
  id: string;
  organizationId: string;
  planId: string;
  customLimits: Record<string, unknown>;
  createdAt: string;
  updatedAt: string;
  plan: PlanResponse;
}

// null when the org has no subscription assigned yet.
export function getSubscription() {
  return apiRequest<SubscriptionResponse | null>("/subscription");
}

// Not in the source app — added in Phase 6 so the plan picker below can be
// populated (plan ids are server-generated UUIDs with no other way to
// discover them). See docs/03 "Deviations resolved during Phase 6".
export function listPlans() {
  return apiRequest<PlanResponse[]>("/plans");
}

// Note: the contract has no admin/permission check on this route (any org
// member can assign a plan) — a documented source-app quirk, kept for parity.
export function assignSubscription(planId: string) {
  return apiRequest<SuccessResponse>("/subscription/assign", { method: "POST", body: { planId } });
}

// ---- Connectors ----

// The connector row as every /connectors endpoint returns it. Deliberately
// has no `config` field and never will — the decrypted config (an upstream
// credential) never leaves the backend's connector.Service, so there is
// nothing here for the UI to render back into a form. See
// components/connectors/google-sheets-form.tsx.
export interface ConnectorResponse {
  id: string;
  organizationId: string;
  name: string;
  type: string;
  status: string;
  lastHealthCheckAt: string | null;
  createdAt: string;
  updatedAt: string;
}

export function listConnectors() {
  return apiRequest<ConnectorResponse[]>("/connectors");
}

export function getConnector(connectorId: string) {
  return apiRequest<ConnectorResponse>(`/connectors/${connectorId}`);
}

// config is required — a connector cannot exist without its credentials, so
// there is no "create empty, configure later" path (docs/02-api-contract.md).
export function createConnector(input: { name: string; type: string; config: Record<string, unknown> }) {
  return apiRequest<ConnectorResponse>("/connectors", { method: "POST", body: input });
}

// A supplied `config` is re-sealed *whole*, not merged with what's stored —
// callers must always submit the complete config object, never a partial one.
export function updateConnector(
  connectorId: string,
  input: { name?: string; status?: string; config?: Record<string, unknown> },
) {
  return apiRequest<ConnectorResponse>(`/connectors/${connectorId}`, { method: "PATCH", body: input });
}

export function deleteConnector(connectorId: string) {
  return apiRequest<SuccessResponse>(`/connectors/${connectorId}`, { method: "DELETE" });
}

// For type "generic" this always rejects with 501 HEALTH_CHECK_UNSUPPORTED
// (no checker registered) and leaves the row untouched. For "google_sheets"
// it's HTTP 200 either way — success or failure both write status/
// lastHealthCheckAt and return the updated row; the probe's underlying error
// is never returned, so callers cannot show a reason beyond "check failed".
export function healthCheckConnector(connectorId: string) {
  return apiRequest<ConnectorResponse>(`/connectors/${connectorId}/health-check`, { method: "POST" });
}

// ---- MCP keys ----

// One row as GET /mcp-keys returns it. Never carries the raw token or its
// hash — see CreateMcpKeyResponse for the one response that does carry the
// raw token. `scopes: null` means a full grant (narrowed at request time by
// the creator's live RBAC), not "no scopes".
export interface McpKeyResponse {
  id: string;
  organizationId: string;
  userId: string;
  name: string;
  scopes: string[] | null;
  lastUsedAt: string | null;
  expiresAt: string | null;
  revokedAt: string | null;
  createdAt: string;
}

// POST /mcp-keys response. apiKey carries the raw `sk_live_...` token and is
// populated **only** here — it is never returned by any other endpoint and
// cannot be recovered later.
export interface CreateMcpKeyResponse {
  id: string;
  name: string;
  apiKey: string;
  expiresAt: string | null;
  createdAt: string;
}

export function listMcpKeys() {
  return apiRequest<McpKeyResponse[]>("/mcp-keys");
}

// expiresInDays omitted (or undefined) mints a key that never expires.
// scopes follows the same three-state contract as McpKeyResponse.scopes:
// omitted/undefined means unrestricted (the key rides the creator's live
// grant); a non-empty array narrows it. Never pass `[]` here — the backend
// rejects an empty list with a 422 by design (docs/08-gateway-core.md §4).
export function createMcpKey(input: { name: string; expiresInDays?: number; scopes?: string[] }) {
  return apiRequest<CreateMcpKeyResponse>("/mcp-keys", { method: "POST", body: input });
}

// Idempotent — revoking an already-revoked key still succeeds.
export function revokeMcpKey(keyId: string) {
  return apiRequest<SuccessResponse>(`/mcp-keys/${keyId}`, { method: "DELETE" });
}

// ---- Audit logs ----

export interface AuditLogResponse {
  id: string;
  organizationId: string | null;
  userId: string | null;
  action: string;
  metadata: Record<string, unknown>;
  createdAt: string;
}

// action accepts either one value or several — a string[] serializes as a
// repeated ?action= param (client.ts's buildPath), matching the backend's
// repeatable-action filter (a single string still behaves exactly as
// before). since is an RFC3339 lower bound; nothing calls it yet (that's
// Phase 5's "last 24 hours" panel), but it's threaded through now since the
// backend already accepts it and there's no reason to leave it out.
export function getAuditLogs(
  filters: { userId?: string; action?: string | string[]; since?: string; limit?: number } = {},
) {
  return apiRequest<AuditLogResponse[]>("/audit-logs", { query: filters });
}

// ---- Admin (docs/11-admin-panel.md) ----
//
// Every /admin/* route requires platform staff — superadmin or support
// (§4's role matrix); a tenant user gets a 401/403 that the (admin)/admin
// layout's own guard bounces to /overview for. List endpoints here return
// `{ items, total }` — AdminListResponse below — unlike the tenant-side
// GET /audit-logs's bare array; the two shapes are deliberately not the
// same (execution plan Task 2.2) and neither should be "fixed" to match
// the other.

export interface AdminListResponse<T> {
  items: T[];
  total: number;
}

export interface AdminMeResponse {
  id: string;
  email: string;
  displayName: string | null;
  platformRole: "superadmin" | "support";
}

// The admin console layout's own guard — see the MeResponse.platformRole
// comment above for how this differs from GET /auth/me.
export function adminMe() {
  return apiRequest<AdminMeResponse>("/admin/me");
}

// The exact contract message for apperror.TwoFactorRequired
// (apps/backend/internal/shared/apperror/apperror.go's Map; docs/02-api-contract.md
// §"2FA step-up") — GET /admin/me's 403 body when ADMIN_REQUIRE_2FA=true and
// the caller has no live admin:2fa:<userId> Redis key yet. The API error
// body carries only `{ message }`, no machine-readable code (see ApiError in
// ./client.ts), so this string is the only way the (admin)/admin layout can
// tell "this staff member needs to complete 2FA step-up" apart from an
// ordinary 403 (a tenant user with no platform role at all, which still
// bounces to /overview). Defined once here rather than scattered across
// call sites.
export const TWO_FACTOR_REQUIRED_MESSAGE = "Two-factor authentication required";

// ---- TOTP step-up (apps/backend/internal/module/admin/{handler,totp}.go,
// docs/02-api-contract.md §"2FA step-up") ----
//
// These three routes are the one part of /admin exempt from the 2FA gate
// they themselves implement (RequirePlatformRoleNo2FA) — a staff member
// still needs a platform role to call them, just not a live step-up yet.
// The frontend UI for this flow lives at app/(admin)/admin/2fa/page.tsx,
// reachable even while GET /admin/me is 403ing with
// TWO_FACTOR_REQUIRED_MESSAGE above. Never log or persist OtpauthURI or
// RecoveryCodes client-side (CLAUDE.md's redaction rule) — both are live
// bearer credentials, shown exactly once.

export interface TOTPEnrollResponse {
  otpauthUri: string;
}

// No request body. Re-callable: enrolling again wipes any prior
// confirmation and recovery codes along with the superseded secret, so a
// caller with an existing enrollment must be warned before this runs.
export function enrollTOTP() {
  return apiRequest<TOTPEnrollResponse>("/admin/2fa/enroll", { method: "POST" });
}

export interface TOTPConfirmResponse {
  recoveryCodes: string[];
}

// code: the 6-digit code from the authenticator app just enrolled. 400
// TOTP_NOT_ENROLLED if enroll never ran; 401 INVALID_TOTP_CODE on a wrong
// code.
export function confirmTOTP(code: string) {
  return apiRequest<TOTPConfirmResponse>("/admin/2fa/confirm", { method: "POST", body: { code } });
}

// code: a live TOTP code OR an unused recovery code — the backend tries
// both, so this validates only non-emptiness, not a fixed shape. On success
// the backend sets admin:2fa:<userId> in Redis for 12h, which is what every
// other /admin route checks. 400 TOTP_NOT_ENROLLED if confirm never landed;
// 401 INVALID_TOTP_CODE on a wrong code; 429 TOO_MANY_ATTEMPTS at 5/15min.
export function verifyTOTP(code: string) {
  return apiRequest<SuccessResponse>("/admin/2fa/verify", { method: "POST", body: { code } });
}

export interface AdminOrgListItem {
  id: string;
  name: string;
  slug: string;
  createdAt: string;
  memberCount: number;
  connectorCount: number;
  mcpKeyCount: number;
  planName: string | null;
}

export function listAdminOrganizations(params: { search?: string; limit?: number; offset?: number } = {}) {
  return apiRequest<AdminListResponse<AdminOrgListItem>>("/admin/organizations", { query: params });
}

export interface AdminOrgMember {
  userId: string;
  email: string;
  displayName: string | null;
  role: string;
  joinedAt: string;
}

// Metadata only — never encrypted_config, never decrypted config, not even
// a "config present" boolean beyond what status already implies
// (docs/11-admin-panel.md §7). Shared by GET /admin/connectors and the
// Connectors section of GET /admin/organizations/:orgId.
export interface AdminConnectorItem {
  id: string;
  organizationId: string;
  organizationName: string;
  name: string;
  type: string;
  status: string;
  lastHealthCheckAt: string | null;
  createdAt: string;
}

// Metadata only — never keyHash, never a raw token (docs/11-admin-panel.md
// §7). Shared by GET /admin/mcp-keys and the MCP keys section of GET
// /admin/organizations/:orgId.
export interface AdminMcpKeyItem {
  id: string;
  organizationId: string;
  organizationName: string;
  userId: string;
  userEmail: string;
  name: string;
  scopes: string[] | null;
  lastUsedAt: string | null;
  expiresAt: string | null;
  revokedAt: string | null;
  createdAt: string;
}

// One entry of OrganizationDetailResponse.RecentAuditLogs on the backend —
// the org is already known from the surrounding response, so unlike
// AdminAuditLogItem (the cross-org GET /admin/audit-logs shape below) this
// carries no organization name/id of its own.
export interface AdminOrgAuditLogItem {
  id: string;
  userId: string | null;
  action: string;
  metadata: Record<string, unknown>;
  createdAt: string;
}

// EffectiveLimits comes from subscription.Service's own custom-over-plan
// merge (never re-derived here) and is empty for an org with no
// subscription — same for planName.
export interface AdminOrganizationDetail {
  id: string;
  name: string;
  slug: string;
  createdAt: string;
  updatedAt: string;
  planName: string | null;
  effectiveLimits: Record<string, number>;
  members: AdminOrgMember[];
  connectors: AdminConnectorItem[];
  mcpKeys: AdminMcpKeyItem[];
  recentAuditLogs: AdminOrgAuditLogItem[];
}

export function getAdminOrganization(orgId: string) {
  return apiRequest<AdminOrganizationDetail>(`/admin/organizations/${orgId}`);
}

export function assignAdminOrgPlan(orgId: string, planId: string) {
  return apiRequest<SuccessResponse>(`/admin/organizations/${orgId}/plan`, { method: "POST", body: { planId } });
}

// customLimits: null clears the override back to plan-only limits; a
// present object replaces it whole (it is not merged with what's stored).
export function setAdminOrgLimits(orgId: string, customLimits: Record<string, unknown> | null) {
  return apiRequest<SuccessResponse>(`/admin/organizations/${orgId}/limits`, {
    method: "PUT",
    body: { customLimits },
  });
}

// confirm must equal the organization's own slug — typing it out is the
// deliberate friction on an irreversible delete (docs/11-admin-panel.md D4).
export function deleteAdminOrganization(orgId: string, input: { confirm: string; password: string }) {
  return apiRequest<SuccessResponse>(`/admin/organizations/${orgId}`, { method: "DELETE", body: input });
}

export interface AdminUserListItem {
  id: string;
  email: string;
  displayName: string | null;
  isVerified: boolean;
  platformRole: "superadmin" | "support" | null;
  bannedAt: string | null;
  createdAt: string;
  orgCount: number;
}

export function listAdminUsers(
  params: {
    search?: string;
    role?: "superadmin" | "support" | "none";
    banned?: boolean;
    limit?: number;
    offset?: number;
  } = {},
) {
  return apiRequest<AdminListResponse<AdminUserListItem>>("/admin/users", {
    query: {
      search: params.search,
      role: params.role,
      // RequestOptions's query type has no boolean case (buildPath just
      // String()s everything else anyway) — spelled out here so ?banned=
      // reads "true"/"false" rather than relying on an implicit coercion.
      banned: params.banned === undefined ? undefined : String(params.banned),
      limit: params.limit,
      offset: params.offset,
    },
  });
}

export interface AdminUserMembership {
  organizationId: string;
  organizationName: string;
  organizationSlug: string;
  role: string;
  joinedAt: string;
}

// Never carries passwordHash — the backend's own DTO mapping is explicit
// field-by-field for the same reason (Task 2.5's doc comment).
export interface AdminUserDetail {
  id: string;
  email: string;
  displayName: string | null;
  isVerified: boolean;
  platformRole: "superadmin" | "support" | null;
  bannedAt: string | null;
  banReason: string | null;
  createdAt: string;
  memberships: AdminUserMembership[];
  activeSessions: number;
}

export function getAdminUser(userId: string) {
  return apiRequest<AdminUserDetail>(`/admin/users/${userId}`);
}

export function listAdminConnectors(
  params: {
    organizationId?: string;
    type?: string;
    status?: string;
    search?: string;
    limit?: number;
    offset?: number;
  } = {},
) {
  return apiRequest<AdminListResponse<AdminConnectorItem>>("/admin/connectors", { query: params });
}

export function listAdminMcpKeys(
  params: { organizationId?: string; userId?: string; search?: string; limit?: number; offset?: number } = {},
) {
  return apiRequest<AdminListResponse<AdminMcpKeyItem>>("/admin/mcp-keys", { query: params });
}

// The cross-org shape — carries organizationName/userEmail that the
// tenant-side AuditLogResponse has no need for (it's already scoped to one
// org by the caller's own header).
export interface AdminAuditLogItem {
  id: string;
  organizationId: string | null;
  organizationName: string | null;
  userId: string | null;
  userEmail: string | null;
  action: string;
  metadata: Record<string, unknown>;
  createdAt: string;
}

// action is repeatable (a string[] serializes as ?action=a&action=b, same
// as getAuditLogs above) and a trailing '*' on any entry means prefix match
// — "admin.*" is how the overview page's "recent activity" panel reads.
// from/to are RFC3339; limit's ceiling is 200 here, not the generic admin
// list shape's 100 (execution plan Task 2.7).
export function queryAdminAuditLogs(
  params: {
    organizationId?: string;
    userId?: string;
    action?: string | string[];
    from?: string;
    to?: string;
    limit?: number;
    offset?: number;
  } = {},
) {
  return apiRequest<AdminListResponse<AdminAuditLogItem>>("/admin/audit-logs", { query: params });
}

export interface AdminEmailOutboxStats {
  pending: number;
  sent: number;
  failed: number;
}

export interface AdminPlanBreakdownItem {
  planName: string;
  orgCount: number;
}

export interface AdminStats {
  organizations: number;
  users: number;
  connectors: number;
  mcpKeysTotal: number;
  mcpKeysActive: number;
  sessionsActive: number;
  auditLogs: number;
  emailOutbox: AdminEmailOutboxStats;
  usersLast7d: number;
  organizationsLast7d: number;
  planBreakdown: AdminPlanBreakdownItem[];
  redisUsedMemoryHuman: string;
}

export function getAdminSystemStats() {
  return apiRequest<AdminStats>("/admin/system/stats");
}

export interface AdminPlan {
  id: string;
  name: string;
  limits: Record<string, unknown>;
  createdAt: string;
}

export function listAdminPlans() {
  return apiRequest<AdminListResponse<AdminPlan>>("/admin/plans");
}

export function createAdminPlan(input: { name: string; limits: Record<string, unknown> }) {
  return apiRequest<AdminPlan>("/admin/plans", { method: "POST", body: input });
}

export function updateAdminPlan(planId: string, input: { name: string; limits: Record<string, unknown> }) {
  return apiRequest<AdminPlan>(`/admin/plans/${planId}`, { method: "PUT", body: input });
}

// Refused with 409 PLAN_IN_USE if any org still subscribes to it — the
// backend is the actual enforcement; pages that show a disabled state
// beforehand are only approximating it from GET /admin/system/stats's
// planBreakdown.
export function deleteAdminPlan(planId: string) {
  return apiRequest<SuccessResponse>(`/admin/plans/${planId}`, { method: "DELETE" });
}

// role: null revokes platform staff status entirely.
export function changeAdminUserPlatformRole(
  userId: string,
  input: { role: "superadmin" | "support" | null; password: string },
) {
  return apiRequest<SuccessResponse>(`/admin/users/${userId}/platform-role`, { method: "PATCH", body: input });
}

export function setAdminUserBan(userId: string, input: { banned: boolean; reason?: string; password: string }) {
  return apiRequest<SuccessResponse>(`/admin/users/${userId}/ban`, { method: "PATCH", body: input });
}

// No refreshToken field, deliberately — an impersonation token cannot be
// extended, only re-issued through this same endpoint, and each re-issue
// writes its own audit entry (docs/11-admin-panel.md §5). expiresIn is
// seconds, the same unit POST /auth/login already uses on the wire.
export interface AdminImpersonateResponse {
  accessToken: string;
  expiresIn: number;
  user: { id: string; email: string; displayName: string | null };
}

// reason is mandatory server-side (min 10 chars) — impersonation is
// controlled by detection rather than prevention, so a reason that's
// actually written down is the control (docs/11-admin-panel.md §5).
export function impersonateAdminUser(userId: string, reason: string) {
  return apiRequest<AdminImpersonateResponse>(`/admin/users/${userId}/impersonate`, {
    method: "POST",
    body: { reason },
  });
}
