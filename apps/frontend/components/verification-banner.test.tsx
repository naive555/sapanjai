import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api/endpoints", () => ({
  me: vi.fn(),
  resendVerification: vi.fn(),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

import { ApiError } from "@/lib/api/client";
import { me, resendVerification, type MeResponse } from "@/lib/api/endpoints";
import { toast } from "sonner";
import { VerificationBanner } from "./verification-banner";

const meMock = vi.mocked(me);
const resendVerificationMock = vi.mocked(resendVerification);
const toastSuccessMock = vi.mocked(toast.success);
const toastErrorMock = vi.mocked(toast.error);

function makeUser(overrides: Partial<MeResponse> = {}): MeResponse {
  return {
    id: "user-1",
    email: "user@example.com",
    displayName: null,
    isVerified: false,
    createdAt: new Date().toISOString(),
    ...overrides,
  };
}

function renderBanner() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <VerificationBanner />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  sessionStorage.clear();
});

afterEach(() => {
  // This repo's vitest config has no test.globals, so @testing-library's
  // automatic afterEach cleanup can't rely on a global afterEach being
  // registered before it's imported — unmount explicitly.
  cleanup();
  vi.clearAllMocks();
});

describe("VerificationBanner", () => {
  it("renders nothing while the me query is loading", () => {
    meMock.mockReturnValue(new Promise(() => {})); // never resolves

    renderBanner();

    expect(screen.queryByText(/verify your email/i)).not.toBeInTheDocument();
  });

  it("renders nothing once the account is verified", async () => {
    meMock.mockResolvedValue(makeUser({ isVerified: true }));

    renderBanner();

    // Let the query settle, then confirm the banner still never showed up.
    await waitFor(() => expect(meMock).toHaveBeenCalled());
    expect(screen.queryByText(/verify your email/i)).not.toBeInTheDocument();
  });

  it("shows the nag with a resend button when unverified", async () => {
    meMock.mockResolvedValue(makeUser({ isVerified: false }));

    renderBanner();

    expect(await screen.findByText(/verify your email/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Resend" })).toBeInTheDocument();
  });

  it("sends the resend request and toasts on success", async () => {
    meMock.mockResolvedValue(makeUser({ isVerified: false }));
    resendVerificationMock.mockResolvedValue({ success: true });

    renderBanner();
    fireEvent.click(await screen.findByRole("button", { name: "Resend" }));

    await waitFor(() => expect(resendVerificationMock).toHaveBeenCalledTimes(1));
    expect(toastSuccessMock).toHaveBeenCalled();
  });

  it("shows the cooldown message from the 429 response, not a generic error", async () => {
    meMock.mockResolvedValue(makeUser({ isVerified: false }));
    resendVerificationMock.mockRejectedValue(
      new ApiError(429, "Please wait before requesting another verification email."),
    );

    renderBanner();
    fireEvent.click(await screen.findByRole("button", { name: "Resend" }));

    await waitFor(() =>
      expect(toastErrorMock).toHaveBeenCalledWith("Please wait before requesting another verification email."),
    );
  });

  it("hides after dismiss and persists that in sessionStorage", async () => {
    meMock.mockResolvedValue(makeUser({ isVerified: false }));

    renderBanner();
    await screen.findByText(/verify your email/i);

    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));

    expect(screen.queryByText(/verify your email/i)).not.toBeInTheDocument();
    expect(sessionStorage.getItem("verification-banner-dismissed")).toBe("1");
  });

  it("stays hidden on a fresh mount once sessionStorage already has the dismissal", async () => {
    sessionStorage.setItem("verification-banner-dismissed", "1");
    meMock.mockResolvedValue(makeUser({ isVerified: false }));

    renderBanner();

    await waitFor(() => expect(meMock).toHaveBeenCalled());
    expect(screen.queryByText(/verify your email/i)).not.toBeInTheDocument();
  });
});
