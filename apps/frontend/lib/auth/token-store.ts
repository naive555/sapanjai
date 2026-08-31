const REFRESH_TOKEN_KEY = "cp.refreshToken";
const ACTIVE_ORG_KEY = "cp.activeOrgId";

// Access token lives in memory only (lost on tab reload by design — the
// session bootstrap in use-session.ts re-derives it from the refresh token).
let accessToken: string | null = null;

export interface TokenPair {
  accessToken: string;
  refreshToken: string;
}

export function getAccessToken(): string | null {
  return accessToken;
}

export function setTokens(tokens: TokenPair): void {
  accessToken = tokens.accessToken;
  window.localStorage.setItem(REFRESH_TOKEN_KEY, tokens.refreshToken);
}

export function getRefreshToken(): string | null {
  if (typeof window === "undefined") return null;
  return window.localStorage.getItem(REFRESH_TOKEN_KEY);
}

export function clearTokens(): void {
  accessToken = null;
  impersonating = false;
  if (typeof window !== "undefined") {
    window.localStorage.removeItem(REFRESH_TOKEN_KEY);
    window.localStorage.removeItem(ACTIVE_ORG_KEY);
  }
  notifyActiveOrgChange();
  notifyImpersonationChange();
}

// Plain pub-sub so useActiveOrgId() (lib/org/active-org.ts) can subscribe via
// useSyncExternalStore instead of duplicating this value into React state —
// every code path that can change or clear the active org (explicit
// selection, logout, a failed background refresh) goes through this module,
// so there's a single source of truth and no risk of a stale org id
// surviving a session change.
type Listener = () => void;
const activeOrgListeners = new Set<Listener>();

function notifyActiveOrgChange(): void {
  activeOrgListeners.forEach((listener) => listener());
}

export function subscribeActiveOrg(listener: Listener): () => void {
  activeOrgListeners.add(listener);
  return () => activeOrgListeners.delete(listener);
}

export function getActiveOrgId(): string | null {
  if (typeof window === "undefined") return null;
  return window.localStorage.getItem(ACTIVE_ORG_KEY);
}

export function setActiveOrgId(orgId: string | null): void {
  if (typeof window === "undefined") return;
  if (orgId) {
    window.localStorage.setItem(ACTIVE_ORG_KEY, orgId);
  } else {
    window.localStorage.removeItem(ACTIVE_ORG_KEY);
  }
  notifyActiveOrgChange();
}

// ---- Impersonation ----
//
// Starting impersonation swaps the in-memory access token for a short-lived
// one that authenticates as the target tenant user (docs/11-admin-panel.md
// §5) — the admin's own refresh token in localStorage is never touched, so
// "exit" is just "drop this token and let the ordinary single-flight
// refresh (lib/api/client.ts) restore the real session from that untouched
// refresh token." The flag lives here, next to the token itself, because
// client.ts's 401 handler needs to read it synchronously with no dependency
// on React — same reasoning as colocating the active-org pub-sub above.
let impersonating = false;

export function isImpersonating(): boolean {
  return impersonating;
}

// beginImpersonation overwrites the in-memory access token with the
// impersonation token and flips the flag. Deliberately does NOT touch
// localStorage — that is the whole point of this client model (see
// lib/api/client.ts's doc comment on the 401-while-impersonating path).
export function beginImpersonation(impersonationAccessToken: string): void {
  accessToken = impersonationAccessToken;
  impersonating = true;
  notifyImpersonationChange();
}

// endImpersonation only clears the flag. It deliberately does not restore
// the admin's own token itself — that requires an actual network round trip
// (lib/api/client.ts's restoreAdminSession), which has no business in a
// module that otherwise only ever touches local state.
export function endImpersonation(): void {
  impersonating = false;
  notifyImpersonationChange();
}

const impersonationListeners = new Set<Listener>();
function notifyImpersonationChange(): void {
  impersonationListeners.forEach((listener) => listener());
}
export function subscribeImpersonation(listener: Listener): () => void {
  impersonationListeners.add(listener);
  return () => impersonationListeners.delete(listener);
}
