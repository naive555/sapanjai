import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/api/endpoints", () => ({
  verifyEmail: vi.fn(),
}));

// Mutable so the missing-token case can be exercised without a second mock
// module. vi.hoisted because vi.mock's factory is hoisted above ordinary
// top-level declarations (same idiom as overview/page.test.tsx's orgState).
const searchParamsState = vi.hoisted(() => ({ token: null as string | null }));

vi.mock("next/navigation", () => ({
  useSearchParams: () => ({
    get: (key: string) => (key === "token" ? searchParamsState.token : null),
  }),
}));

import { ApiError } from "@/lib/api/client";
import { verifyEmail } from "@/lib/api/endpoints";
import VerifyEmailPage from "./page";

const verifyEmailMock = vi.mocked(verifyEmail);

function renderPage(options: { strict?: boolean } = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
  const tree = (
    <QueryClientProvider client={queryClient}>
      <VerifyEmailPage />
    </QueryClientProvider>
  );
  render(options.strict ? <StrictMode>{tree}</StrictMode> : tree);
  return { invalidateSpy };
}

beforeEach(() => {
  searchParamsState.token = "tok-abc";
});

afterEach(() => {
  // This repo's vitest config has no test.globals, so @testing-library's
  // automatic afterEach cleanup can't rely on a global afterEach being
  // registered before it's imported — unmount explicitly.
  cleanup();
  vi.clearAllMocks();
});

describe("VerifyEmailPage", () => {
  it("verifies the token once, invalidates the me query, and shows the success CTA", async () => {
    verifyEmailMock.mockResolvedValue({ success: true });

    const { invalidateSpy } = renderPage();

    expect(await screen.findByText("Your email is verified.")).toBeInTheDocument();
    expect(verifyEmailMock).toHaveBeenCalledTimes(1);
    expect(verifyEmailMock).toHaveBeenCalledWith({ token: "tok-abc" });
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ["me"] });
    expect(screen.getByRole("link", { name: "Continue" })).toHaveAttribute("href", "/organizations");
  });

  it("posts exactly once even under StrictMode's double-invoked effects", async () => {
    verifyEmailMock.mockResolvedValue({ success: true });

    renderPage({ strict: true });

    await screen.findByText("Your email is verified.");
    expect(verifyEmailMock).toHaveBeenCalledTimes(1);
  });

  it("shows the server's error message plus a sign-in affordance on failure", async () => {
    verifyEmailMock.mockRejectedValue(new ApiError(400, "This verification link has already been used."));

    renderPage();

    expect(await screen.findByText("This verification link has already been used.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /sign in to request a new link/i })).toHaveAttribute(
      "href",
      "/login",
    );
  });

  it("renders the error state without calling the API when the token is missing", async () => {
    searchParamsState.token = null;

    renderPage();

    expect(await screen.findByText("This verification link is missing its token.")).toBeInTheDocument();
    expect(verifyEmailMock).not.toHaveBeenCalled();
  });
});
