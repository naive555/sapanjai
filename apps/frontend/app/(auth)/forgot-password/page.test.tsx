import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api/endpoints", () => ({
  forgotPassword: vi.fn(),
}));

import { ApiError } from "@/lib/api/client";
import { forgotPassword } from "@/lib/api/endpoints";
import ForgotPasswordPage from "./page";

const forgotPasswordMock = vi.mocked(forgotPassword);

// The exact enumeration-safe copy from docs/02-api-contract.md, reused
// verbatim in both tests below to prove the two paths render identically.
const PANEL_TEXT = "If an account exists for that address, we've sent a reset link.";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function fillAndSubmit(email: string) {
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: email } });
  fireEvent.click(screen.getByRole("button", { name: "Send reset link" }));
}

describe("ForgotPasswordPage", () => {
  it("shows the confirmation panel after a 200", async () => {
    forgotPasswordMock.mockResolvedValue({ success: true });

    render(<ForgotPasswordPage />);
    fillAndSubmit("user@example.com");

    expect(await screen.findByText(PANEL_TEXT)).toBeInTheDocument();
    expect(forgotPasswordMock).toHaveBeenCalledWith({ email: "user@example.com" });
  });

  it("shows the identical panel when the request fails, never an error toast", async () => {
    forgotPasswordMock.mockRejectedValue(new ApiError(500, "Internal server error"));

    render(<ForgotPasswordPage />);
    fillAndSubmit("user@example.com");

    // Same text node as the success case — the failure must be
    // indistinguishable from a legitimate send, by design.
    expect(await screen.findByText(PANEL_TEXT)).toBeInTheDocument();
  });

  it("does not call the API for an invalid email", async () => {
    render(<ForgotPasswordPage />);
    fillAndSubmit("not-an-email");

    expect(await screen.findByText(/enter a valid email/i)).toBeInTheDocument();
    expect(forgotPasswordMock).not.toHaveBeenCalled();
  });

  it("links back to login from the confirmation panel", async () => {
    forgotPasswordMock.mockResolvedValue({ success: true });

    render(<ForgotPasswordPage />);
    fillAndSubmit("user@example.com");

    await screen.findByText(PANEL_TEXT);
    expect(screen.getByRole("link", { name: /back to login/i })).toHaveAttribute("href", "/login");
  });
});
