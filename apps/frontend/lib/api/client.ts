import {
  clearTokens,
  endImpersonation,
  getAccessToken,
  getActiveOrgId,
  getRefreshToken,
  isImpersonating,
  setTokens,
} from "@/lib/auth/token-store";

const API_BASE = "/api";

export class ApiError extends Error {
  status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

export interface RequestOptions {
  method?: string;
  body?: unknown;
  // A string[] value serializes as a repeated query param (?action=a&action=b),
  // for endpoints like GET /audit-logs whose `action` filter is repeatable.
  query?: Record<string, string | number | string[] | undefined>;
  // True for the four public auth-flow calls (register/login/refresh/logout):
  // a 401 there is a terminal result (bad credentials, dead refresh token),
  // not a stale-access-token signal, so it must not trigger refresh+retry.
  noRetry?: boolean;
}

function buildPath(path: string, query?: RequestOptions["query"]): string {
  if (!query) return path;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined) continue;
    if (Array.isArray(value)) {
      for (const v of value) params.append(key, v);
    } else {
      params.set(key, String(value));
    }
  }
  const qs = params.toString();
  return qs ? `${path}?${qs}` : path;
}

async function rawRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, query } = options;
  const headers: Record<string, string> = { "Content-Type": "application/json" };

  const token = getAccessToken();
  if (token) headers.Authorization = `Bearer ${token}`;

  const orgId = getActiveOrgId();
  if (orgId) headers["x-organization-id"] = orgId;

  const res = await fetch(`${API_BASE}${buildPath(path, query)}`, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  const text = await res.text();
  const data: unknown = text ? JSON.parse(text) : null;

  if (!res.ok) {
    const message =
      data && typeof data === "object" && "message" in data
        ? String((data as { message: unknown }).message)
        : `Request failed with status ${res.status}`;
    throw new ApiError(res.status, message);
  }

  return data as T;
}

// Single-flight refresh: concurrent 401s share one /auth/refresh call.
let refreshing: Promise<void> | null = null;

async function doRefresh(): Promise<void> {
  const refreshToken = getRefreshToken();
  if (!refreshToken) {
    clearTokens();
    throw new ApiError(401, "No refresh token");
  }

  try {
    const tokens = await rawRequest<{ accessToken: string; refreshToken: string }>(
      "/auth/refresh",
      { method: "POST", body: { refreshToken }, noRetry: true },
    );
    setTokens(tokens);
  } catch (err) {
    clearTokens();
    throw err;
  }
}

function ensureRefreshed(): Promise<void> {
  if (!refreshing) {
    refreshing = doRefresh().finally(() => {
      refreshing = null;
    });
  }
  return refreshing;
}

// ---- Impersonation expiry ----
//
// A 401 under an impersonation token means exactly one thing: the 10-minute
// TTL is up (docs/11-admin-panel.md §5) — never "stale access token, go
// refresh it". The ordinary retry path a few lines down exists to smooth
// over the ADMIN's own stale access token by silently swapping in a fresh
// one (via the refresh token sitting untouched in localStorage) and
// retrying the original call. Taking that same path here would restore the
// admin's own session invisibly and hand the admin's OWN data back to a
// screen that is still labelled "Viewing as <tenant>" — the one genuinely
// dangerous frontend bug this feature can produce (execution plan Task
// 5.2). So this path is handled separately below: end impersonation,
// restore the admin's session for real (so the app isn't left
// half-logged-out), but never retry *this* request under the restored
// identity — whatever screen made the call is left to fail and re-render
// once `impersonating` flips to false, and notifyImpersonationExpired() is
// what lets it also toast about *why*.
const impersonationExpiredListeners = new Set<() => void>();

export function subscribeImpersonationExpired(listener: () => void): () => void {
  impersonationExpiredListeners.add(listener);
  return () => impersonationExpiredListeners.delete(listener);
}

function notifyImpersonationExpired(): void {
  impersonationExpiredListeners.forEach((listener) => listener());
}

// Ends impersonation and runs the ordinary single-flight refresh against the
// refresh token in localStorage (never touched while impersonating), which
// restores the admin's own access token.
async function restoreAdminSession(): Promise<void> {
  endImpersonation();
  try {
    await ensureRefreshed();
  } catch {
    // No usable refresh token, or the backend rejected it — doRefresh has
    // already cleared tokens itself in that case; the session layer's own
    // bootstrap effect will observe that on its next read and settle on
    // "anon". Nothing more to do here.
  }
}

// Single-flighted the same way doRefresh is: several queries can 401 under
// the same expiring impersonation token within the same tick (a page that
// fires four queries on mount, say), and this must restore the admin's
// session — and fire the toast — exactly once for that whole cluster, not
// once per failed request.
let restoringFromExpiry: Promise<void> | null = null;
function restoreFromExpiry(): Promise<void> {
  if (!restoringFromExpiry) {
    restoringFromExpiry = restoreAdminSession()
      .then(() => notifyImpersonationExpired())
      .finally(() => {
        restoringFromExpiry = null;
      });
  }
  return restoringFromExpiry;
}

// Used by the "Exit" banner button (lib/auth/use-session.tsx) — restores the
// admin's own session the same way an expiry does, minus the toast: the
// admin knows they're exiting, it isn't a surprise.
export async function exitImpersonation(): Promise<void> {
  await restoreAdminSession();
}

export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  // Captured before the request runs, not re-read in the catch block below:
  // several requests can be in flight together, and re-checking after an
  // `await` would race a concurrent request's restoreAdminSession() call
  // flipping the flag back to false first — which would send *this*
  // request's retry out under the just-restored ADMIN token instead of
  // taking the safe branch. Tying the branch to what this specific request
  // was actually authenticated with when it was sent closes that race.
  const wasImpersonating = isImpersonating();
  try {
    return await rawRequest<T>(path, options);
  } catch (err) {
    if (err instanceof ApiError && err.status === 401 && !options.noRetry) {
      if (wasImpersonating) {
        await restoreFromExpiry();
        throw err;
      }
      await ensureRefreshed();
      return rawRequest<T>(path, options);
    }
    throw err;
  }
}
