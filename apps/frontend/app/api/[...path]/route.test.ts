import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

import { GET, POST } from "./route";

// The proxy forwards the caller's headers wholesale (it has to — the
// Authorization header is how every authenticated call reaches the API), so
// the two it deletes are load-bearing security behaviour rather than
// housekeeping. Without these tests, a refactor that rebuilt the header copy
// could drop the deletes and nothing anywhere in either codebase would fail:
// the backend's ADMIN_IP_ALLOWLIST would start trusting a caller-supplied
// address again, and every admin audit entry's `ip` would become forgeable.
// See apps/backend/internal/server/server.go's e.IPExtractor comment.

function forwardedHeaders(fetchMock: ReturnType<typeof vi.fn>): Headers {
  const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined;
  return new Headers(init?.headers);
}

function stubFetch() {
  const fetchMock = vi.fn(async () => new Response("{}", { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

beforeEach(() => {
  process.env.BACKEND_URL = "http://api.test";
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the /api proxy's header handling", () => {
  it("never forwards a caller-supplied X-Forwarded-For or X-Real-IP", async () => {
    const fetchMock = stubFetch();

    await GET(
      new NextRequest("http://localhost:4000/api/admin/me", {
        headers: {
          authorization: "Bearer staff-token",
          "x-forwarded-for": "10.0.0.1",
          "x-real-ip": "10.0.0.1",
        },
      }),
      { params: Promise.resolve({ path: ["admin", "me"] }) },
    );

    const sent = forwardedHeaders(fetchMock);
    // The spoof attempt: 10.0.0.1 is exactly the shape of an address an
    // operator would put in ADMIN_IP_ALLOWLIST.
    expect(sent.get("x-forwarded-for")).toBeNull();
    expect(sent.get("x-real-ip")).toBeNull();
    // ...while the headers the proxy exists to relay still get through.
    expect(sent.get("authorization")).toBe("Bearer staff-token");
  });

  it("strips them on body-carrying methods too, not just GET", async () => {
    const fetchMock = stubFetch();

    await POST(
      new NextRequest("http://localhost:4000/api/admin/users/x/impersonate", {
        method: "POST",
        headers: { "content-type": "application/json", "x-forwarded-for": "10.0.0.1" },
        body: JSON.stringify({ reason: "a sufficiently long reason" }),
      }),
      { params: Promise.resolve({ path: ["admin", "users", "x", "impersonate"] }) },
    );

    expect(forwardedHeaders(fetchMock).get("x-forwarded-for")).toBeNull();
  });

  it("strips regardless of header-name casing", async () => {
    // Headers is case-insensitive, but the delete() calls are lowercase
    // literals — this pins that a caller cannot evade them by casing.
    const fetchMock = stubFetch();

    await GET(
      new NextRequest("http://localhost:4000/api/admin/me", {
        headers: { "X-Forwarded-For": "10.0.0.1", "X-Real-IP": "10.0.0.1" },
      }),
      { params: Promise.resolve({ path: ["admin", "me"] }) },
    );

    const sent = forwardedHeaders(fetchMock);
    expect(sent.get("x-forwarded-for")).toBeNull();
    expect(sent.get("x-real-ip")).toBeNull();
  });
});
