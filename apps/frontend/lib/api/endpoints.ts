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
