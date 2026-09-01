import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api/endpoints", () => ({
  adminMe: vi.fn(),
  // Real contract string, not a mock — layout.tsx's own comparison against
  // it is exactly what these tests exercise (see the "not the 2FA message"
  // case below, which relies on this being the true value).
  TWO_FACTOR_REQUIRED_MESSAGE: "Two-factor authentication required",
}));

const sessionState = vi.hoisted(() => ({ status: "authed" as "loading" | "authed" | "anon" }));
vi.mock("@/lib/auth/use-session", () => ({
  useSession: () => ({ status: sessionState.status }),
}));

const pathnameState = vi.hoisted(() => ({ pathname: "/admin" }));
const routerMock = vi.hoisted(() => ({ replace: vi.fn(), push: vi.fn() }));
vi.mock("next/navigation", () => ({
  useRouter: () => routerMock,
  usePathname: () => pathnameState.pathname,
}));

import { ApiError } from "@/lib/api/client";
import { adminMe } from "@/lib/api/endpoints";
import AdminLayout from "./layout";

const adminMeMock = vi.mocked(adminMe);

function renderLayout() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <AdminLayout>
        <div>step-up page content</div>
      </AdminLayout>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  sessionState.status = "authed";
  pathnameState.pathname = "/admin";
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AdminLayout", () => {
  it("routes a 2FA-required 403 to the step-up page, not /overview", async () => {
    adminMeMock.mockRejectedValue(new ApiError(403, "Two-factor authentication required"));

    renderLayout();

    await waitFor(() => expect(routerMock.replace).toHaveBeenCalledWith("/admin/2fa"));
    expect(routerMock.replace).not.toHaveBeenCalledWith("/overview");
  });

  it("routes an ordinary 403 (no platform role) to /overview, not the step-up page", async () => {
    adminMeMock.mockRejectedValue(new ApiError(403, "Insufficient permissions"));

    renderLayout();

    await waitFor(() => expect(routerMock.replace).toHaveBeenCalledWith("/overview"));
    expect(routerMock.replace).not.toHaveBeenCalledWith("/admin/2fa");
  });

  it("routes a 401 to /overview the same as before", async () => {
    adminMeMock.mockRejectedValue(new ApiError(401, "Unauthorized"));

    renderLayout();

    await waitFor(() => expect(routerMock.replace).toHaveBeenCalledWith("/overview"));
  });

  it("renders the step-up page's own children immediately on /admin/2fa, without waiting on GET /admin/me", async () => {
    pathnameState.pathname = "/admin/2fa";
    // Never resolves — proves the step-up path doesn't gate on this query.
    adminMeMock.mockReturnValue(new Promise(() => {}));

    renderLayout();

    expect(await screen.findByText("step-up page content")).toBeInTheDocument();
  });

  it("still bounces a non-staff visitor away from /admin/2fa itself", async () => {
    pathnameState.pathname = "/admin/2fa";
    adminMeMock.mockRejectedValue(new ApiError(403, "Insufficient permissions"));

    renderLayout();

    await waitFor(() => expect(routerMock.replace).toHaveBeenCalledWith("/overview"));
  });
});
