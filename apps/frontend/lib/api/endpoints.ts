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
export function createMcpKey(input: { name: string; expiresInDays?: number }) {
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

export function getAuditLogs(
  filters: { userId?: string; action?: string; limit?: number } = {},
) {
  return apiRequest<AuditLogResponse[]>("/audit-logs", { query: filters });
}
