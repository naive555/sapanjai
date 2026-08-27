import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api/endpoints", () => ({
  resetPassword: vi.fn(),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

// Mutable so the missing-token case can be exercised — same vi.hoisted
// idiom as verify-email/page.test.tsx's searchParamsState.
const searchParamsState = vi.hoisted(() => ({ token: "tok-abc" as string | null }));
const routerMock = vi.hoisted(() => ({ replace: vi.fn() }));

vi.mock("next/navigation", () => ({
  useSearchParams: () => ({
    get: (key: string) => (key === "token" ? searchParamsState.token : null),
  }),
  useRouter: () => routerMock,
}));

import { ApiError } from "@/lib/api/client";
import { resetPassword } from "@/lib/api/endpoints";
import { toast } from "sonner";
import ResetPasswordPage from "./page";

const resetPasswordMock = vi.mocked(resetPassword);
const toastSuccessMock = vi.mocked(toast.success);

beforeEach(() => {
  searchParamsState.token = "tok-abc";
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function fillForm(password: string, confirmPassword: string) {
  fireEvent.change(screen.getByLabelText("New password"), { target: { value: password } });
  fireEvent.change(screen.getByLabelText("Confirm password"), { target: { value: confirmPassword } });
  fireEvent.click(screen.getByRole("button", { name: "Reset password" }));
}

describe("ResetPasswordPage", () => {
  it("blocks submission when the confirmation doesn't match", async () => {
    render(<ResetPasswordPage />);
    fillForm("password123", "password456");

    expect(await screen.findByText("Passwords don't match")).toBeInTheDocument();
    expect(resetPasswordMock).not.toHaveBeenCalled();
  });

  it("resets the password and redirects to login on success", async () => {
    resetPasswordMock.mockResolvedValue({ success: true });

    render(<ResetPasswordPage />);
    fillForm("password123", "password123");

    await waitFor(() =>
      expect(resetPasswordMock).toHaveBeenCalledWith({ token: "tok-abc", password: "password123" }),
    );
    await waitFor(() => expect(routerMock.replace).toHaveBeenCalledWith("/login"));
    expect(toastSuccessMock).toHaveBeenCalled();
  });

  it("shows an inline error and a link back to forgot-password on INVALID_RESET_TOKEN", async () => {
    resetPasswordMock.mockRejectedValue(new ApiError(400, "This reset link is invalid or has expired."));

    render(<ResetPasswordPage />);
    fillForm("password123", "password123");

    expect(await screen.findByText("This reset link is invalid or has expired.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /request a new link/i })).toHaveAttribute(
      "href",
      "/forgot-password",
    );
    expect(routerMock.replace).not.toHaveBeenCalled();
  });

  it("renders the missing-token state without a form when the token is absent", async () => {
    searchParamsState.token = null;

    render(<ResetPasswordPage />);

    expect(await screen.findByText("This reset link is missing its token.")).toBeInTheDocument();
    expect(screen.queryByLabelText("New password")).not.toBeInTheDocument();
    expect(resetPasswordMock).not.toHaveBeenCalled();
  });
});
