import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// use-session.tsx pulls in `me`/`logout`/`refresh` from lib/api/endpoints —
// stubbed rather than left real so SessionProvider's own bootstrap effect
// (which reads getRefreshToken()) has something deterministic to resolve
// against, same pattern verification-banner.test.tsx already uses.
vi.mock("@/lib/api/endpoints", () => ({
  me: vi.fn(),
  logout: vi.fn(),
  refresh: vi.fn(),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

// The banner's "Exit" button calls useRouter().push, unused by these tests
// (none of them click Exit) but required for the component to render at all
// outside an actual Next.js app router tree.
const routerPushMock = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: routerPushMock }),
}));

import { apiRequest } from "@/lib/api/client";
import { me } from "@/lib/api/endpoints";
import { SessionProvider, useSession } from "@/lib/auth/use-session";
import { toast } from "sonner";
import { ImpersonationBanner } from "./impersonation-banner";

const meMock = vi.mocked(me);
const toastErrorMock = vi.mocked(toast.error);

function jsonResponse(status: number, body: unknown): Response {
  return { status, ok: status >= 200 && status < 300, text: async () => JSON.stringify(body) } as Response;
}

// Mirrors what POST /admin/users/:userId/impersonate's real caller
// (app/(admin)/admin/users/[userId]/page-client.tsx) does once it has a
// response — starts impersonation from inside the provider tree, which is
// the only place lib/auth/use-session.tsx's startImpersonation is reachable
// from (it isn't exported as a bare function; it's context state).
function Harness() {
  const { startImpersonation } = useSession();
  return (
    <>
      <button
        onClick={() =>
          startImpersonation({
            accessToken: "impersonation-token",
            expiresIn: 600,
            user: { id: "target-1", email: "target@example.com", displayName: null },
          })
        }
      >
        start impersonation
      </button>
      <ImpersonationBanner />
    </>
  );
}

function renderHarness() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <SessionProvider>
        <Harness />
      </SessionProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  localStorage.clear();
  meMock.mockResolvedValue({
    id: "admin-1",
    email: "admin@example.com",
    displayName: null,
    isVerified: true,
    platformRole: "superadmin",
    createdAt: new Date().toISOString(),
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

describe("ImpersonationBanner", () => {
  it("renders nothing before impersonation starts", () => {
    renderHarness();
    expect(screen.queryByText(/viewing as/i)).not.toBeInTheDocument();
  });

  it("shows the target's email once impersonation starts", async () => {
    renderHarness();
    screen.getByRole("button", { name: "start impersonation" }).click();

    expect(await screen.findByText(/viewing as/i)).toBeInTheDocument();
    expect(screen.getByText("target@example.com")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Exit" })).toBeInTheDocument();
  });

  // THE path execution plan Task 5.2 calls out as the one genuinely
  // dangerous frontend bug available in this feature: a 401 under the
  // impersonation token must never be silently smoothed over by the
  // ordinary refresh-and-retry flow (that would swap the admin's own
  // session back in under a UI still labeled "Viewing as <tenant>" — see
  // lib/api/client.ts's and lib/auth/use-session.tsx's matching comments).
  // Exercised here through the real client.ts/use-session.tsx wiring, not a
  // mock of either, so it proves the two modules are actually connected.
  it("clears itself and toasts when a 401 arrives while impersonating", async () => {
    renderHarness();
    screen.getByRole("button", { name: "start impersonation" }).click();
    await screen.findByText(/viewing as/i);

    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input).split("?")[0];
      if (url === "/api/admin/organizations") return jsonResponse(401, { message: "Unauthorized" });
      if (url === "/api/auth/refresh") {
        return jsonResponse(200, { accessToken: "restored-admin-token", refreshToken: "admin-refresh" });
      }
      throw new Error(`unexpected fetch to ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);

    // Stands in for whatever query the admin console was mid-flight on when
    // the 10-minute impersonation token ran out — apiRequest is the real,
    // unmocked client.ts export.
    await expect(apiRequest("/admin/organizations")).rejects.toThrow();

    await waitFor(() => expect(screen.queryByText(/viewing as/i)).not.toBeInTheDocument());
    expect(toastErrorMock).toHaveBeenCalledWith(
      "Impersonation session expired — you're back in your own account.",
    );
    // Never retried under the restored admin token — only the one 401 and
    // the one refresh call, never a second call to the original path.
    const orgCalls = fetchMock.mock.calls.filter(([input]) => String(input) === "/api/admin/organizations");
    expect(orgCalls).toHaveLength(1);
  });
});
