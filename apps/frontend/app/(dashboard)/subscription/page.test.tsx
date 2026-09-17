import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api/endpoints", () => ({
  getSubscription: vi.fn(),
  getUsage: vi.fn(),
  listPlans: vi.fn(),
  startCheckout: vi.fn(),
  openBillingPortal: vi.fn(),
}));

vi.mock("@/lib/org/active-org", () => ({
  useActiveOrgId: () => "org-1",
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

// Mutable so different tests can land on this page with a different
// ?checkout= value — same vi.hoisted idiom as
// app/(auth)/reset-password/page.test.tsx's searchParamsState.
const searchParamsState = vi.hoisted(() => ({ checkout: null as string | null }));

vi.mock("next/navigation", () => ({
  useSearchParams: () => ({
    get: (key: string) => (key === "checkout" ? searchParamsState.checkout : null),
  }),
}));

import { ApiError } from "@/lib/api/client";
import {
  getSubscription,
  getUsage,
  listPlans,
  openBillingPortal,
  startCheckout,
  type PlanResponse,
  type SubscriptionResponse,
  type UsageResponse,
} from "@/lib/api/endpoints";
import { toast } from "sonner";

import SubscriptionPage from "./page";

const getSubscriptionMock = vi.mocked(getSubscription);
const getUsageMock = vi.mocked(getUsage);
const listPlansMock = vi.mocked(listPlans);
const startCheckoutMock = vi.mocked(startCheckout);
const openBillingPortalMock = vi.mocked(openBillingPortal);
const toastErrorMock = vi.mocked(toast.error);

// jsdom throws "Not implemented: navigation" on a real window.location
// assignment; replace it with a stub so the redirect the page performs on
// a successful checkout/portal mutation can be asserted directly.
const assignMock = vi.fn();

const proPlan: PlanResponse = {
  id: "plan-pro",
  name: "pro",
  limits: { max_members: 10, max_tool_calls_per_month: 500 },
  createdAt: "2026-01-01T00:00:00Z",
  prices: [
    { unitAmount: 99000, currency: "thb", interval: "month" },
    { unitAmount: 990000, currency: "thb", interval: "year" },
  ],
};

const freePlan: PlanResponse = {
  id: "plan-free",
  name: "free",
  limits: { max_members: 3, max_tool_calls_per_month: -1 },
  createdAt: "2026-01-01T00:00:00Z",
  prices: [],
};

function subscriptionFixture(overrides: Partial<SubscriptionResponse> = {}): SubscriptionResponse {
  return {
    id: "sub-1",
    organizationId: "org-1",
    planId: "plan-free",
    customLimits: {},
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    plan: freePlan,
    status: null,
    currentPeriodEnd: null,
    cancelAtPeriodEnd: false,
    hasActiveSubscription: false,
    ...overrides,
  };
}

function usageFixture(overrides: Partial<UsageResponse> = {}): UsageResponse {
  return {
    periodStart: "2026-09-01T00:00:00Z",
    periodEnd: "2026-10-01T00:00:00Z",
    callCount: 12,
    limit: 500,
    // A different number from callCount, deliberately — the rollup this
    // reads from is allowed to lag the live total (UsageResponse.ByTool's
    // comment), so the two are not expected to match, and using the same
    // value here would make "12" ambiguous in the rendered DOM too.
    byTool: [{ tool: "sheets_query_rows", callCount: 9 }],
    ...overrides,
  };
}

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <SubscriptionPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

beforeEach(() => {
  searchParamsState.checkout = null;
  assignMock.mockClear();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...window.location, assign: assignMock },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("SubscriptionPage", () => {
  it("renders the current plan, its limits, and the usage meter", async () => {
    getSubscriptionMock.mockResolvedValue(subscriptionFixture());
    listPlansMock.mockResolvedValue([freePlan, proPlan]);
    getUsageMock.mockResolvedValue(usageFixture());

    renderPage();

    // "free" renders twice (the current-plan heading and its catalogue
    // row below), so this checks the heading specifically.
    expect(await screen.findByRole("heading", { name: "free" })).toBeInTheDocument();
    expect(await screen.findByText("12")).toBeInTheDocument();
    expect(await screen.findByText("/ 500 calls this period")).toBeInTheDocument();
    expect(await screen.findByText("sheets_query_rows")).toBeInTheDocument();
  });

  it("renders unlimited usage without a limit fraction", async () => {
    getSubscriptionMock.mockResolvedValue(subscriptionFixture());
    listPlansMock.mockResolvedValue([freePlan]);
    getUsageMock.mockResolvedValue(usageFixture({ limit: null, byTool: [] }));

    renderPage();

    expect(await screen.findByText("calls this period — unlimited")).toBeInTheDocument();
  });

  it("marks the current plan as not purchasable and offers Subscribe on the others", async () => {
    getSubscriptionMock.mockResolvedValue(subscriptionFixture({ planId: "plan-free" }));
    listPlansMock.mockResolvedValue([freePlan, proPlan]);
    getUsageMock.mockResolvedValue(usageFixture());

    renderPage();

    const rows = await screen.findAllByRole("row");
    const freeRow = rows.find((r) => within(r).queryByText("free"));
    const proRow = rows.find((r) => within(r).queryByText("pro"));
    expect(freeRow && within(freeRow).getByText("current plan")).toBeInTheDocument();
    expect(proRow && within(proRow).getAllByRole("button", { name: /Subscribe/ })).toHaveLength(2);
  });

  it("starts checkout with the clicked price's interval and redirects on success", async () => {
    getSubscriptionMock.mockResolvedValue(subscriptionFixture({ planId: "plan-free" }));
    listPlansMock.mockResolvedValue([freePlan, proPlan]);
    getUsageMock.mockResolvedValue(usageFixture());
    startCheckoutMock.mockResolvedValue({ url: "https://checkout.stripe.com/c/pay/cs_test_1" });

    renderPage();

    const yearlyButton = await screen.findByRole("button", { name: /Subscribe.*yr/ });
    fireEvent.click(yearlyButton);

    await waitFor(() => {
      expect(startCheckoutMock).toHaveBeenCalled();
    });
    // TanStack Query's mutationFn is invoked with the variables as its
    // first argument and an internal context object as its second —
    // toHaveBeenCalledWith would have to match both, so this checks the
    // one this page actually controls.
    expect(startCheckoutMock.mock.calls[0][0]).toEqual({ planId: "plan-pro", interval: "year" });
    await waitFor(() => {
      expect(assignMock).toHaveBeenCalledWith("https://checkout.stripe.com/c/pay/cs_test_1");
    });
  });

  it("surfaces a checkout error as a toast instead of redirecting", async () => {
    getSubscriptionMock.mockResolvedValue(subscriptionFixture({ planId: "plan-free" }));
    listPlansMock.mockResolvedValue([freePlan, proPlan]);
    getUsageMock.mockResolvedValue(usageFixture());
    startCheckoutMock.mockRejectedValue(new ApiError(409, "Plan is not available for purchase"));

    renderPage();

    const monthlyButton = await screen.findByRole("button", { name: /Subscribe.*mo/ });
    fireEvent.click(monthlyButton);

    await waitFor(() => {
      expect(toastErrorMock).toHaveBeenCalledWith("Plan is not available for purchase");
    });
    expect(assignMock).not.toHaveBeenCalled();
  });

  it("shows Manage billing only when the org has an active Stripe subscription, and redirects to the portal", async () => {
    getSubscriptionMock.mockResolvedValue(
      subscriptionFixture({ planId: "plan-pro", plan: proPlan, hasActiveSubscription: true, status: "active" }),
    );
    listPlansMock.mockResolvedValue([freePlan, proPlan]);
    getUsageMock.mockResolvedValue(usageFixture());
    openBillingPortalMock.mockResolvedValue({ url: "https://billing.stripe.com/p/session/bps_test_1" });

    renderPage();

    const manageButton = await screen.findByRole("button", { name: "Manage billing" });
    fireEvent.click(manageButton);

    await waitFor(() => {
      expect(openBillingPortalMock).toHaveBeenCalled();
    });
    await waitFor(() => {
      expect(assignMock).toHaveBeenCalledWith("https://billing.stripe.com/p/session/bps_test_1");
    });
  });

  it("does not render Manage billing for an org with no Stripe subscription", async () => {
    getSubscriptionMock.mockResolvedValue(subscriptionFixture({ hasActiveSubscription: false }));
    listPlansMock.mockResolvedValue([freePlan]);
    getUsageMock.mockResolvedValue(usageFixture());

    renderPage();

    await screen.findByRole("heading", { name: "free" });
    expect(screen.queryByRole("button", { name: "Manage billing" })).not.toBeInTheDocument();
  });

  it("renders a permission message instead of a usage meter on a 403", async () => {
    getSubscriptionMock.mockResolvedValue(subscriptionFixture());
    listPlansMock.mockResolvedValue([freePlan]);
    getUsageMock.mockRejectedValue(new ApiError(403, "Missing permission: billing:read"));

    renderPage();

    expect(
      await screen.findByText("You don't have permission to view usage in this organization."),
    ).toBeInTheDocument();
  });

  it("shows the checkout-success banner and never claims the plan already changed", async () => {
    searchParamsState.checkout = "success";
    getSubscriptionMock.mockResolvedValue(subscriptionFixture());
    listPlansMock.mockResolvedValue([freePlan]);
    getUsageMock.mockResolvedValue(usageFixture());

    renderPage();

    expect(await screen.findByText("Checkout complete")).toBeInTheDocument();
    expect(screen.getByText(/updates once its webhook lands/)).toBeInTheDocument();
  });

  it("shows the checkout-cancelled banner", async () => {
    searchParamsState.checkout = "cancelled";
    getSubscriptionMock.mockResolvedValue(subscriptionFixture());
    listPlansMock.mockResolvedValue([freePlan]);
    getUsageMock.mockResolvedValue(usageFixture());

    renderPage();

    expect(await screen.findByText("Checkout cancelled")).toBeInTheDocument();
  });
});
