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
  if (typeof window !== "undefined") {
    window.localStorage.removeItem(REFRESH_TOKEN_KEY);
    window.localStorage.removeItem(ACTIVE_ORG_KEY);
  }
  notifyActiveOrgChange();
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
