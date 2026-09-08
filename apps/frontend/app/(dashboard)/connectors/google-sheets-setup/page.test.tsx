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
  it("makes the service-account path the primary, numbered walkthrough", () => {
    render(<GoogleSheetsSetupPage />);

    // The numbered steps (the <Step> component's <h2>) are the primary
    // walkthrough. All four service-account-specific ones should be among
    // them, in order.
    const stepTitles = screen.getAllByRole("heading", { level: 2 }).map((el) => el.textContent);
    const projectIdx = stepTitles.findIndex((t) => /create a google cloud project/i.test(t ?? ""));
    const enableIdx = stepTitles.findIndex((t) => /enable the sheets api/i.test(t ?? ""));
    const serviceAccountIdx = stepTitles.findIndex((t) => /create a service account/i.test(t ?? ""));
    const shareIdx = stepTitles.findIndex((t) => /share each spreadsheet/i.test(t ?? ""));

    expect(projectIdx).toBeGreaterThanOrEqual(0);
    expect(enableIdx).toBeGreaterThan(projectIdx);
    expect(serviceAccountIdx).toBeGreaterThan(enableIdx);
    expect(shareIdx).toBeGreaterThan(serviceAccountIdx);

    // The OAuth client / refresh-token instructions are not numbered <h2>
    // steps at all — they live inside the collapsed disclosure as plain
    // sub-headings, which is what keeps them from reading as an equal,
    // parallel path.
    expect(stepTitles.some((t) => /create an oauth client/i.test(t ?? ""))).toBe(false);
    expect(screen.getByRole("heading", { level: 3, name: /create an oauth client/i })).toBeInTheDocument();
  });

  it("names the form's exact field label and echoed client_email", () => {
    render(<GoogleSheetsSetupPage />);

    expect(screen.getByText("Service account key (JSON)")).toBeInTheDocument();
    expect(screen.getAllByText(/client_email/).length).toBeGreaterThan(0);
  });

  it("warns about Workspace-blocked external sharing before any step, not just in step 4", () => {
    const { container } = render(<GoogleSheetsSetupPage />);
    const text = container.textContent ?? "";

    // The callout has to land before the first numbered step's heading, i.e.
    // in the opening section of the page — not buried down in step 4.
    const calloutIdx = text.indexOf("does your organization block external sharing");
    const stepIdx = text.indexOf("Create a Google Cloud project");
    expect(calloutIdx).toBeGreaterThanOrEqual(0);
    expect(stepIdx).toBeGreaterThan(calloutIdx);

    expect(text).toContain("gserviceaccount.com");
  });

  it("collapses the OAuth walkthrough by default and labels it as the fallback path", () => {
    const { container } = render(<GoogleSheetsSetupPage />);

    const details = container.querySelector("details");
    expect(details).not.toBeNull();
    expect(details).not.toHaveAttribute("open");

    const summary = details?.querySelector("summary");
    expect(summary?.textContent ?? "").toMatch(/oauth/i);
    expect(summary?.textContent ?? "").toMatch(/workspace admin blocks external sharing/i);
  });

  it("still prints both read-only scopes verbatim, inside the OAuth walkthrough", () => {
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
    // token — the failure the OAuth walkthrough's own callout is about, and
    // the one that would make the whole thing dead-end.
    const authUrl = screen.getByText(/accounts\.google\.com/).textContent ?? "";
    expect(authUrl).toContain("access_type=offline");
    expect(authUrl).toContain("prompt=consent");
    expect(authUrl).toContain(encodeURIComponent(SHEETS_SCOPE));
    expect(authUrl).toContain(encodeURIComponent(DRIVE_SCOPE));
  });

  it("keeps a numbered list counting across the code block that splits it", () => {
    const { container } = render(<GoogleSheetsSetupPage />);

    // The OAuth walkthrough's manual route is a four-item list with the
    // authorization URL between items 2 and 3, so the tail has to carry
    // start={3}. Left to default it renders as "1." again, which reads as a
    // fresh procedure.
    const split = Array.from(container.querySelectorAll("ol")).find(
      (ol) => ol.getAttribute("start") !== null,
    );
    expect(split?.getAttribute("start")).toBe("3");
  });

  it("keeps the Testing / seven-day refresh-token warning substantively unchanged", () => {
    const { container } = render(<GoogleSheetsSetupPage />);
    const text = container.textContent ?? "";

    expect(text).toContain("Leaving the consent screen in Testing will break this in a week");
    expect(text).toMatch(/expires its\s*refresh tokens after roughly seven days/);
  });

  it("routes the reader to the connectors list to finish and verify", () => {
    render(<GoogleSheetsSetupPage />);

    const connectorLinks = screen
      .getAllByRole("link")
      .filter((el) => el.getAttribute("href") === "/connectors");
    expect(connectorLinks.length).toBeGreaterThan(0);
    expect(screen.getByRole("link", { name: /MCP keys/i })).toHaveAttribute("href", "/mcp-keys");
  });

  it("tells the reader where to find spreadsheet/folder IDs without duplicating it in the OAuth section", () => {
    const { container } = render(<GoogleSheetsSetupPage />);

    const details = container.querySelector("details") as HTMLElement;
    expect(details.textContent ?? "").toMatch(/step 4 above/i);
    // The URL-shape examples (the actual "how to find an ID" instructions)
    // live only in step 4, not duplicated inside the collapsed section.
    expect(details.textContent ?? "").not.toContain("THIS_PART");
  });
});
