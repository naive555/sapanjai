import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { apiRequest } from "./client";
import { clearTokens, getAccessToken, getRefreshToken, setTokens } from "@/lib/auth/token-store";

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
});
