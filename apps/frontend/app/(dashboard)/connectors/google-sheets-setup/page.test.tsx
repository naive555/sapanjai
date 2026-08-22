import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import GoogleSheetsSetupPage from "./page";

/*
 * These assertions restate the two scopes as literals on purpose, rather
 * than importing a shared constant. The value that has to stay true is the
 * one in the backend's own request
 * (apps/backend/internal/adapter/googlesheets/oauth.go), and a test that
 * imports the same constant the page renders would pass through any typo
 * introduced in it. A customer who authorizes a token with a mistyped scope
 * gets no error here at all — their connector just fails its health check
 * with a message that deliberately says nothing useful.
 */
const SHEETS_SCOPE = "https://www.googleapis.com/auth/spreadsheets.readonly";
const DRIVE_SCOPE = "https://www.googleapis.com/auth/drive.readonly";

afterEach(cleanup);

describe("Google Sheets setup walkthrough", () => {
  it("prints both read-only scopes verbatim", () => {
    render(<GoogleSheetsSetupPage />);

    expect(screen.getByText(SHEETS_SCOPE)).toBeInTheDocument();
    expect(screen.getByText(DRIVE_SCOPE)).toBeInTheDocument();
  });

  it("never offers a write scope", () => {
    const { container } = render(<GoogleSheetsSetupPage />);
    const text = container.textContent ?? "";

    // The read-write forms of the same two scopes, which are what a reader
    // lands on if they pick from Google's scope list by eye.
    expect(text).not.toContain("auth/spreadsheets ");
    expect(text).not.toContain("auth/drive ");
    expect(text).not.toContain("auth/drive.file");
  });

  it("builds an authorization URL that can actually return a refresh token", () => {
    render(<GoogleSheetsSetupPage />);

    // Without both of these Google returns an access token and no refresh
    // token — the failure the page's own callout is about, and the one that
    // would make the whole walkthrough dead-end.
    const authUrl = screen.getByText(/accounts\.google\.com/).textContent ?? "";
    expect(authUrl).toContain("access_type=offline");
    expect(authUrl).toContain("prompt=consent");
    expect(authUrl).toContain(encodeURIComponent(SHEETS_SCOPE));
    expect(authUrl).toContain(encodeURIComponent(DRIVE_SCOPE));
  });

  it("keeps a numbered list counting across the code block that splits it", () => {
    const { container } = render(<GoogleSheetsSetupPage />);

    // Step 4's manual route is a four-item list with the authorization URL
    // between items 2 and 3, so the tail has to carry start={3}. Left to
    // default it renders as "1." again, which reads as a fresh procedure.
    const split = Array.from(container.querySelectorAll("ol")).find(
      (ol) => ol.getAttribute("start") !== null,
    );
    expect(split?.getAttribute("start")).toBe("3");
  });

  it("routes the reader to the connectors list to finish and verify", () => {
    render(<GoogleSheetsSetupPage />);

    const connectorLinks = screen
      .getAllByRole("link")
      .filter((el) => el.getAttribute("href") === "/connectors");
    expect(connectorLinks.length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: /MCP keys/i })).toHaveAttribute("href", "/mcp-keys");
  });
});
