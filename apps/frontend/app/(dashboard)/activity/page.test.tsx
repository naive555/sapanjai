import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AuditLogResponse } from "@/lib/api/endpoints";

vi.mock("@/lib/api/endpoints", () => ({
  getAuditLogs: vi.fn(),
  listMembers: vi.fn(),
}));

vi.mock("@/lib/org/active-org", () => ({
  useActiveOrgId: () => "org-1",
}));

import { getAuditLogs, listMembers } from "@/lib/api/endpoints";
import ActivityPage from "./page";

const getAuditLogsMock = vi.mocked(getAuditLogs);
const listMembersMock = vi.mocked(listMembers);

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ActivityPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

function makeLog(overrides: Partial<AuditLogResponse>): AuditLogResponse {
  return {
    id: "log-1",
    organizationId: "org-1",
    userId: null,
    action: "user.login",
    metadata: {},
    createdAt: new Date().toISOString(),
    ...overrides,
  };
}

beforeEach(() => {
  listMembersMock.mockResolvedValue([]);
});

afterEach(() => {
  // This repo's vitest config has no test.globals, so @testing-library's
  // automatic afterEach cleanup (which relies on a global afterEach being
  // registered before it's imported) can't be relied on — unmount explicitly.
  cleanup();
  vi.clearAllMocks();
});

describe("ActivityPage", () => {
  it("renders a denied tool's missing_permission through PermissionToken's grammar", async () => {
    getAuditLogsMock.mockResolvedValue([
      makeLog({
        id: "log-denied",
        action: "mcp.tool.denied",
        metadata: { connector_id: "conn-1", tool: "drive_get_file", missing_permission: "drive:read" },
      }),
    ]);

    renderPage();

    const row = (await screen.findByText("drive_get_file")).closest("tr")!;
    // PermissionToken splits resource:verb into two spans rather than one
    // text node — assert the pieces it renders, the same way permission
    // grammar reads on the roles page.
    expect(within(row).getByText("drive")).toBeInTheDocument();
    expect(within(row).getByText("read")).toBeInTheDocument();
  });

  it("shows tool, duration, and row count for a mcp.tool.called row", async () => {
    getAuditLogsMock.mockResolvedValue([
      makeLog({
        id: "log-called",
        action: "mcp.tool.called",
        metadata: { connector_id: "conn-1", tool: "sheets_query_rows", duration_ms: 412, row_count: 88 },
      }),
    ]);

    renderPage();

    const row = (await screen.findByText("sheets_query_rows")).closest("tr")!;
    expect(within(row).getByText("412ms")).toBeInTheDocument();
    expect(within(row).getByText("· 88 rows")).toBeInTheDocument();
  });

  it("falls back to key=value rendering for an action it doesn't know, instead of blank or throwing", async () => {
    getAuditLogsMock.mockResolvedValue([
      makeLog({
        id: "log-unknown",
        action: "billing.invoice.issued",
        metadata: { invoice_id: "inv-1", amount: 42 },
      }),
    ]);

    renderPage();

    const row = (await screen.findByText("billing.invoice.")).closest("tr")!;
    expect(within(row).getByText("inv-1")).toBeInTheDocument();
    expect(within(row).getByText("42")).toBeInTheDocument();
  });

  it("does not throw on malformed metadata — missing tool, wrong types, and null", async () => {
    getAuditLogsMock.mockResolvedValue([
      makeLog({ id: "log-missing-tool", action: "mcp.tool.denied", metadata: { missing_permission: "sheets:read" } }),
      makeLog({ id: "log-wrong-type", action: "mcp.tool.called", metadata: { tool: "x", duration_ms: "412" } }),
      // metadata is typed as Record<string, unknown>, but the wire contract
      // only guarantees an object — cast past the type to cover a
      // genuinely-null field the same way runtime JSON can produce one.
      makeLog({ id: "log-null", action: "mcp.tool.called", metadata: null as unknown as Record<string, unknown> }),
    ]);

    renderPage();

    // Reaching this line at all is the assertion — a throw during render
    // would fail the test before any of these run.
    expect(await screen.findAllByRole("row")).not.toHaveLength(0);
    // duration_ms as a string isn't a number per the guard, so it's omitted
    // rather than rendered as "412msms" or similar.
    expect(screen.queryByText(/412ms/)).not.toBeInTheDocument();
  });

  it("narrows to mcp.* rows when 'Gateway only' is on, and disables the Action select", async () => {
    getAuditLogsMock.mockResolvedValue([
      makeLog({ id: "log-login", action: "user.login" }),
      makeLog({
        id: "log-called",
        action: "mcp.tool.called",
        metadata: { tool: "sheets_read_range", duration_ms: 10 },
      }),
    ]);

    renderPage();

    // ActionToken splits "user.login" across two spans, so it isn't any
    // single element's own text — assert through the table's full text
    // content rather than an exact getByText match.
    const table = await screen.findByRole("table");
    await screen.findByText("sheets_read_range");
    expect(table).toHaveTextContent("user.login");

    fireEvent.click(screen.getByRole("button", { name: "Gateway only" }));

    expect(screen.getByText("sheets_read_range")).toBeInTheDocument();
    expect(table).not.toHaveTextContent("user.login");

    // The Action select is the first combobox (the Gateway button isn't
    // one) — overridden, not ANDed, while the toggle is on.
    expect(screen.getAllByRole("combobox")[0]).toBeDisabled();
  });
});
