import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api/endpoints", () => ({
  enrollTOTP: vi.fn(),
  confirmTOTP: vi.fn(),
  verifyTOTP: vi.fn(),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

const routerMock = vi.hoisted(() => ({ push: vi.fn() }));
vi.mock("next/navigation", () => ({
  useRouter: () => routerMock,
}));

import { ApiError } from "@/lib/api/client";
import { confirmTOTP, enrollTOTP, verifyTOTP } from "@/lib/api/endpoints";
import { toast } from "sonner";
import AdminTwoFactorPage from "./page";

const enrollTOTPMock = vi.mocked(enrollTOTP);
const confirmTOTPMock = vi.mocked(confirmTOTP);
const verifyTOTPMock = vi.mocked(verifyTOTP);
const toastErrorMock = vi.mocked(toast.error);
const toastSuccessMock = vi.mocked(toast.success);

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <AdminTwoFactorPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  // jsdom has no navigator.clipboard by default — stub it so the "Copy all"
  // button's handler resolves deterministically rather than hitting the
  // (silent, toast-driven) fallback branch in every test.
  Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AdminTwoFactorPage", () => {
  it("defaults to the verify form", () => {
    renderPage();

    expect(screen.getByLabelText(/authenticator code or recovery code/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Verify" })).toBeInTheDocument();
  });

  it("verifies a code and sends the admin on into the console, mentioning the 12h window", async () => {
    verifyTOTPMock.mockResolvedValue({ success: true });

    renderPage();
    fireEvent.change(screen.getByLabelText(/authenticator code or recovery code/i), {
      target: { value: "123456" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Verify" }));

    await waitFor(() => expect(verifyTOTPMock).toHaveBeenCalledWith("123456"));
    await waitFor(() => expect(routerMock.push).toHaveBeenCalledWith("/admin"));
    expect(toastSuccessMock).toHaveBeenCalledWith(expect.stringMatching(/12 hours/));
  });

  it("surfaces the API's exact message on a wrong code", async () => {
    verifyTOTPMock.mockRejectedValue(new ApiError(401, "Invalid two-factor code"));

    renderPage();
    fireEvent.change(screen.getByLabelText(/authenticator code or recovery code/i), {
      target: { value: "000000" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Verify" }));

    await waitFor(() => expect(toastErrorMock).toHaveBeenCalledWith("Invalid two-factor code"));
    expect(routerMock.push).not.toHaveBeenCalled();
  });

  it("switches to the enroll flow automatically when verify reports TOTP_NOT_ENROLLED", async () => {
    verifyTOTPMock.mockRejectedValue(new ApiError(400, "Two-factor authentication not enrolled"));

    renderPage();
    fireEvent.change(screen.getByLabelText(/authenticator code or recovery code/i), {
      target: { value: "123456" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Verify" }));

    expect(await screen.findByRole("button", { name: "Generate new secret" })).toBeInTheDocument();
  });

  it("runs the full enroll -> confirm -> recovery-codes sequence", async () => {
    enrollTOTPMock.mockResolvedValue({
      otpauthUri:
        "otpauth://totp/Sapanjai%20Admin:staff%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=Sapanjai+Admin&algorithm=SHA1&digits=6&period=30",
    });
    confirmTOTPMock.mockResolvedValue({
      recoveryCodes: ["code-1", "code-2", "code-3", "code-4", "code-5", "code-6", "code-7", "code-8", "code-9", "code-10"],
    });

    renderPage();

    fireEvent.click(screen.getByRole("button", { name: "Set up / replace authenticator" }));
    fireEvent.click(screen.getByRole("button", { name: "Generate new secret" }));

    // The otpauth URI and its parsed-out base32 secret are both rendered as
    // selectable text — no QR library per CLAUDE.md's constraint.
    expect(await screen.findByText(/otpauth:\/\/totp/)).toBeInTheDocument();
    expect(screen.getByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(/enter the current code it shows/i), {
      target: { value: "654321" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(confirmTOTPMock).toHaveBeenCalledWith("654321"));
    expect(await screen.findByText("code-1")).toBeInTheDocument();
    expect(screen.getByText("code-10")).toBeInTheDocument();
    expect(screen.getByText(/shown exactly once/i)).toBeInTheDocument();

    // "Continue to verify" stays disabled until the codes are acknowledged.
    const continueButton = screen.getByRole("button", { name: /continue to verify/i });
    expect(continueButton).toBeDisabled();

    fireEvent.click(screen.getByRole("checkbox", { name: /i've saved these recovery codes somewhere safe/i }));
    expect(continueButton).not.toBeDisabled();

    fireEvent.click(continueButton);

    // Back on the verify form, ready for a fresh code from the
    // just-confirmed authenticator.
    expect(await screen.findByRole("button", { name: "Verify" })).toBeInTheDocument();
    expect(screen.queryByText("code-1")).not.toBeInTheDocument();
  });

  it("warns before re-enrolling over an existing authenticator", () => {
    renderPage();

    fireEvent.click(screen.getByRole("button", { name: "Set up / replace authenticator" }));

    expect(screen.getByText(/invalidates it and every existing recovery code/i)).toBeInTheDocument();
  });
});
