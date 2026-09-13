import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SourceLink } from "./source-link";

afterEach(() => {
  cleanup();
  vi.unstubAllEnvs();
});

describe("SourceLink", () => {
  // AGPL-3.0 §13 is discharged by this link, so these are compliance
  // assertions, not cosmetic ones.
  it("offers the source even with nothing configured", () => {
    render(<SourceLink />);

    expect(screen.getByRole("link", { name: "Source" })).toHaveAttribute(
      "href",
      "https://github.com/naive555/sapanjai",
    );
  });

  it("points at the exact commit the build came from", () => {
    vi.stubEnv("SOURCE_URL", "https://git.example.com/acme/sapanjai-fork");
    vi.stubEnv("SOURCE_COMMIT", "5745b7d1c0ffee5745b7d1c0ffee5745b7d1c0ff");

    render(<SourceLink />);

    expect(screen.getByRole("link", { name: "Source" })).toHaveAttribute(
      "href",
      "https://git.example.com/acme/sapanjai-fork/tree/5745b7d1c0ffee5745b7d1c0ffee5745b7d1c0ff",
    );
    expect(screen.getByText("5745b7d")).toBeInTheDocument();
  });

  it("trims a trailing slash rather than emitting a doubled path", () => {
    vi.stubEnv("SOURCE_URL", "https://git.example.com/acme/fork/");
    vi.stubEnv("SOURCE_COMMIT", "abc1234");

    render(<SourceLink />);

    expect(screen.getByRole("link", { name: "Source" })).toHaveAttribute(
      "href",
      "https://git.example.com/acme/fork/tree/abc1234",
    );
  });

  it("falls back to the bare repository when SOURCE_COMMIT is not a sha", () => {
    // A branch name, a tag, or an injected path segment must not be
    // interpolated into the URL.
    vi.stubEnv("SOURCE_COMMIT", "main/../../evil");

    render(<SourceLink />);

    expect(screen.getByRole("link", { name: "Source" })).toHaveAttribute(
      "href",
      "https://github.com/naive555/sapanjai",
    );
  });

  it("names the licence", () => {
    render(<SourceLink />);

    expect(screen.getByRole("link", { name: "AGPL-3.0" })).toHaveAttribute(
      "href",
      "https://www.gnu.org/licenses/agpl-3.0.html",
    );
  });
});
