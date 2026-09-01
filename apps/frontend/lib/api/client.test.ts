import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { apiRequest, subscribeImpersonationExpired } from "./client";
import { beginImpersonation, clearTokens, getAccessToken, getRefreshToken, isImpersonating, setTokens } from "@/lib/auth/token-store";

function jsonResponse(status: number, body: unknown): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    text: async () => JSON.stringify(body),
  } as Response;
}

// Routes a mocked fetch by URL (ignoring query string) so tests aren't
// coupled to the exact order concurrent requests happen to fire in.
function makeFetchMock(handlers: Record<string, (callIndex: number) => Response>) {
  const counts: Record<string, number> = {};
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input).split("?")[0];
    const idx = counts[url] ?? 0;
    counts[url] = idx + 1;
    const handler = handlers[url];
    if (!handler) throw new Error(`unexpected fetch to ${url}`);
    return handler(idx);
  });
}

beforeEach(() => {
  clearTokens();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("apiRequest", () => {
  it("retries exactly once after a single refresh on 401", async () => {
    setTokens({ accessToken: "old-token", refreshToken: "refresh-token" });

    const fetchMock = makeFetchMock({
      "/api/protected": (i) =>
        i === 0 ? jsonResponse(401, { message: "Unauthorized" }) : jsonResponse(200, { ok: true }),
      "/api/auth/refresh": () =>
        jsonResponse(200, { accessToken: "new-token", refreshToken: "new-refresh" }),
    });
    vi.stubGlobal("fetch", fetchMock);

    const result = await apiRequest("/protected");

    expect(result).toEqual({ ok: true });
    expect(fetchMock).toHaveBeenCalledTimes(3); // 401, refresh, retry
    expect(getAccessToken()).toBe("new-token");
  });

  it("shares a single refresh across concurrent 401s (single-flight)", async () => {
    setTokens({ accessToken: "old-token", refreshToken: "refresh-token" });

    const fetchMock = makeFetchMock({
      "/api/a": (i) => (i === 0 ? jsonResponse(401, { message: "Unauthorized" }) : jsonResponse(200, { a: true })),
      "/api/b": (i) => (i === 0 ? jsonResponse(401, { message: "Unauthorized" }) : jsonResponse(200, { b: true })),
      "/api/auth/refresh": () =>
        jsonResponse(200, { accessToken: "new-token", refreshToken: "new-refresh" }),
    });
    vi.stubGlobal("fetch", fetchMock);

    const [a, b] = await Promise.all([apiRequest("/a"), apiRequest("/b")]);

    expect(a).toEqual({ a: true });
    expect(b).toEqual({ b: true });

    const refreshCalls = fetchMock.mock.calls.filter(([input]) => String(input) === "/api/auth/refresh");
    expect(refreshCalls).toHaveLength(1);
  });

  it("clears tokens and rejects when the refresh call itself fails", async () => {
    setTokens({ accessToken: "old-token", refreshToken: "dead-refresh" });

    const fetchMock = makeFetchMock({
      "/api/protected": () => jsonResponse(401, { message: "Unauthorized" }),
      "/api/auth/refresh": () => jsonResponse(401, { message: "Invalid refresh token" }),
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(apiRequest("/protected")).rejects.toThrow("Invalid refresh token");
    expect(getAccessToken()).toBeNull();
    expect(getRefreshToken()).toBeNull();
  });

  it("does not retry a 401 from a noRetry (public auth-flow) call", async () => {
    const fetchMock = makeFetchMock({
      "/api/auth/login": () => jsonResponse(401, { message: "Invalid email or password" }),
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(
      apiRequest("/auth/login", { method: "POST", body: { email: "a@b.com", password: "wrong" }, noRetry: true }),
    ).rejects.toThrow("Invalid email or password");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("serializes a string[] query value as a repeated param, and scalars as before", async () => {
    let requestedUrl = "";
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      requestedUrl = String(input);
      return jsonResponse(200, []);
    });
    vi.stubGlobal("fetch", fetchMock);

    await apiRequest("/audit-logs", {
      query: { action: ["mcp.tool.called", "mcp.tool.denied"], limit: 50, userId: undefined },
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(requestedUrl).toContain("/audit-logs");
    const [, qs] = requestedUrl.split("?");
    const params = new URLSearchParams(qs);
    expect(params.getAll("action")).toEqual(["mcp.tool.called", "mcp.tool.denied"]);
    expect(params.get("limit")).toBe("50");
    expect(params.has("userId")).toBe(false);
  });

  // execution plan Task 5.2: "the one genuinely dangerous frontend bug
  // available here" — a 401 under an impersonation token must never take
  // the ordinary refresh-and-retry path above, because that path would
  // silently swap the admin's own session back in (via the refresh token
  // sitting untouched in localStorage the whole time) and then hand the
  // ADMIN's own data back to a caller that still thinks it's reading the
  // impersonated tenant's data. See docs/11-admin-panel.md §5.
  describe("the 401-while-impersonating path", () => {
    it("never retries the failing request under the restored admin token", async () => {
      // The exact client model: admin's refresh token already in
      // localStorage from an ordinary login, then impersonation swaps only
      // the in-memory access token (lib/auth/token-store.ts's
      // beginImpersonation) — the refresh token is never touched.
      setTokens({ accessToken: "admin-token", refreshToken: "admin-refresh" });
      beginImpersonation("impersonation-token");

      const fetchMock = makeFetchMock({
        "/api/admin-protected": () => jsonResponse(401, { message: "Unauthorized" }),
        "/api/auth/refresh": () =>
          jsonResponse(200, { accessToken: "restored-admin-token", refreshToken: "admin-refresh" }),
      });
      vi.stubGlobal("fetch", fetchMock);

      await expect(apiRequest("/admin-protected")).rejects.toThrow("Unauthorized");

      // The dangerous case this test exists to catch: if apiRequest took
      // the ordinary branch, /admin-protected would have been called a
      // second time (the retry) — under the just-restored admin token.
      const protectedCalls = fetchMock.mock.calls.filter(([input]) =>
        String(input).startsWith("/api/admin-protected"),
      );
      expect(protectedCalls).toHaveLength(1);

      // The admin's own session is still restored in the background — the
      // app must not be left with a dead in-memory token — just never used
      // to silently answer the call that was actually about the tenant.
      expect(getAccessToken()).toBe("restored-admin-token");
      expect(isImpersonating()).toBe(false);
    });

    it("notifies subscribeImpersonationExpired listeners exactly once", async () => {
      setTokens({ accessToken: "admin-token", refreshToken: "admin-refresh" });
      beginImpersonation("impersonation-token");

      const fetchMock = makeFetchMock({
        "/api/admin-protected": () => jsonResponse(401, { message: "Unauthorized" }),
        "/api/auth/refresh": () =>
          jsonResponse(200, { accessToken: "restored-admin-token", refreshToken: "admin-refresh" }),
      });
      vi.stubGlobal("fetch", fetchMock);

      const listener = vi.fn();
      const unsubscribe = subscribeImpersonationExpired(listener);

      await expect(apiRequest("/admin-protected")).rejects.toThrow();

      expect(listener).toHaveBeenCalledTimes(1);
      unsubscribe();
    });

    it("single-flights the restore across concurrent 401s under the same expiring token", async () => {
      // A page that fires several queries on mount (the realistic case: the
      // dashboard loading connectors, mcp-keys, and audit logs together) can
      // have all of them 401 within the same tick once the 10-minute
      // impersonation token expires. This must restore the admin session
      // and fire the expiry notification exactly once for the whole
      // cluster — not once per failed request, and never racing a second
      // request into retrying under a half-restored token (see client.ts's
      // own comment on why `wasImpersonating` is captured per-request up
      // front rather than re-checked after an await).
      setTokens({ accessToken: "admin-token", refreshToken: "admin-refresh" });
      beginImpersonation("impersonation-token");

      const fetchMock = makeFetchMock({
        "/api/a": () => jsonResponse(401, { message: "Unauthorized" }),
        "/api/b": () => jsonResponse(401, { message: "Unauthorized" }),
        "/api/auth/refresh": () =>
          jsonResponse(200, { accessToken: "restored-admin-token", refreshToken: "admin-refresh" }),
      });
      vi.stubGlobal("fetch", fetchMock);

      const listener = vi.fn();
      subscribeImpersonationExpired(listener);

      const results = await Promise.allSettled([apiRequest("/a"), apiRequest("/b")]);

      expect(results.every((r) => r.status === "rejected")).toBe(true);
      const refreshCalls = fetchMock.mock.calls.filter(([input]) => String(input) === "/api/auth/refresh");
      expect(refreshCalls).toHaveLength(1);
      expect(listener).toHaveBeenCalledTimes(1);
    });

    it("a normal (non-impersonating) 401 still takes the ordinary refresh-and-retry path", async () => {
      // Sanity check that the new branch didn't cannibalize the existing
      // behavior — the first test in this file covers this in more detail,
      // this just confirms it still holds with the impersonation flag
      // available and explicitly false.
      setTokens({ accessToken: "old-token", refreshToken: "refresh-token" });
      expect(isImpersonating()).toBe(false);

      const fetchMock = makeFetchMock({
        "/api/protected": (i) =>
          i === 0 ? jsonResponse(401, { message: "Unauthorized" }) : jsonResponse(200, { ok: true }),
        "/api/auth/refresh": () => jsonResponse(200, { accessToken: "new-token", refreshToken: "new-refresh" }),
      });
      vi.stubGlobal("fetch", fetchMock);

      const result = await apiRequest("/protected");

      expect(result).toEqual({ ok: true });
      expect(getAccessToken()).toBe("new-token");
    });
  });
});
