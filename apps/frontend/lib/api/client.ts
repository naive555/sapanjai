import {
  clearTokens,
  getAccessToken,
  getActiveOrgId,
  getRefreshToken,
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
  query?: Record<string, string | number | undefined>;
  // True for the four public auth-flow calls (register/login/refresh/logout):
  // a 401 there is a terminal result (bad credentials, dead refresh token),
  // not a stale-access-token signal, so it must not trigger refresh+retry.
  noRetry?: boolean;
}

function buildPath(path: string, query?: RequestOptions["query"]): string {
  if (!query) return path;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined) params.set(key, String(value));
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

export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  try {
    return await rawRequest<T>(path, options);
  } catch (err) {
    if (err instanceof ApiError && err.status === 401 && !options.noRetry) {
      await ensureRefreshed();
      return rawRequest<T>(path, options);
    }
    throw err;
  }
}
