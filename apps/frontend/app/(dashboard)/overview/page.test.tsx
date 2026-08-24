import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AuditLogResponse, ConnectorResponse, McpKeyResponse, RoleResponse } from "@/lib/api/endpoints";

vi.mock("@/lib/api/endpoints", () => ({
  listConnectors: vi.fn(),
  listMcpKeys: vi.fn(),
  listRoles: vi.fn(),
  getAuditLogs: vi.fn(),
}));

// Mutable so the no-organization case below can be exercised: /overview is
// the landing page, so unlike every other org-scoped route it can actually
// be reached with nothing selected. vi.hoisted because vi.mock's factory is
// hoisted above ordinary top-level declarations.
const orgState = vi.hoisted(() => ({ id: null as string | null }));

vi.mock("@/lib/org/active-org", () => ({
  useActiveOrgId: () => orgState.id,
}));

import { getAuditLogs, listConnectors, listMcpKeys, listRoles } from "@/lib/api/endpoints";
import OverviewPage from "./page";

const listConnectorsMock = vi.mocked(listConnectors);
const listMcpKeysMock = vi.mocked(listMcpKeys);
const listRolesMock = vi.mocked(listRoles);
const getAuditLogsMock = vi.mocked(getAuditLogs);

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <OverviewPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

function makeConnector(overrides: Partial<ConnectorResponse> = {}): ConnectorResponse {
  return {
    id: "conn-1",
    organizationId: "org-1",
    name: "Acme sheet",
    type: "google_sheets",
    status: "inactive",
    lastHealthCheckAt: null,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
}

function makeKey(overrides: Partial<McpKeyResponse> = {}): McpKeyResponse {
  return {
    id: "key-1",
    organizationId: "org-1",
    userId: "user-1",
    name: "A key",
    scopes: null,
    lastUsedAt: null,
    expiresAt: null,
    revokedAt: null,
    createdAt: new Date().toISOString(),
    ...overrides,
  };
}

function makeRole(overrides: Partial<RoleResponse> = {}): RoleResponse {
  return {
    id: "role-1",
    organizationId: "org-1",
    name: "Viewer",
    description: null,
    createdAt: new Date().toISOString(),
    permissions: [],
    ...overrides,
  };
}

function makeLog(overrides: Partial<AuditLogResponse> = {}): AuditLogResponse {
  return {
    id: `log-${Math.random()}`,
    organizationId: "org-1",
    userId: "user-1",
    action: "mcp.tool.called",
    metadata: {},
    createdAt: new Date().toISOString(),
    ...overrides,
  };
}

beforeEach(() => {
  orgState.id = "org-1";
  listMcpKeysMock.mockResolvedValue([]);
  listRolesMock.mockResolvedValue([]);
  getAuditLogsMock.mockResolvedValue([]);
});

afterEach(() => {
  // This repo's vitest config has no test.globals, so @testing-library's
  // automatic afterEach cleanup can't rely on a global afterEach being
  // registered before it's imported — unmount explicitly (same idiom as the
  // sibling connector detail page's test file).
  cleanup();
  vi.clearAllMocks();
});

describe("OverviewPage", () => {
  it("asks for an organization, not a connector, when none is selected", async () => {
    // A returning user on a fresh browser has no stored org selection
    // (useActiveOrgId reads localStorage; nothing auto-selects), and
    // /overview is the one org-scoped route they can land on anyway. The
    // connector onboarding Span would name the wrong next step here.
    orgState.id = null;
    listConnectorsMock.mockResolvedValue([]);

    renderPage();

    expect(await screen.findByText(/No organization selected/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Pick one, or create your first/i })).toHaveAttribute(
      "href",
      "/organizations",
    );
    expect(screen.queryByText("create a connector to get started")).not.toBeInTheDocument();
  });

  it("renders the onboarding Span, not a crash or a separate empty state, with zero connectors", async () => {
    listConnectorsMock.mockResolvedValue([]);

    renderPage();

    expect(await screen.findByText("create a connector to get started")).toBeInTheDocument();
    expect(screen.getByText("no agent has connected yet")).toBeInTheDocument();
    expect(screen.getByText("nothing to reach until a connector exists")).toBeInTheDocument();
  });

  it("renders a single connector's Span alone", async () => {
    listConnectorsMock.mockResolvedValue([makeConnector({ id: "conn-1", name: "Only one" })]);

    renderPage();

    expect(await screen.findByText("Only one")).toBeInTheDocument();
    expect(screen.queryByText("create a connector to get started")).not.toBeInTheDocument();
  });

  it("stacks two or more connectors, each with its own Span", async () => {
    listConnectorsMock.mockResolvedValue([
      makeConnector({ id: "conn-1", name: "First" }),
      makeConnector({ id: "conn-2", name: "Second" }),
    ]);

    renderPage();

    expect(await screen.findByText("First")).toBeInTheDocument();
    expect(screen.getByText("Second")).toBeInTheDocument();
  });

  it("still renders a broken (inactive) connector's Span, showing the fix", async () => {
    listConnectorsMock.mockResolvedValue([makeConnector({ id: "conn-1", name: "Broken one", status: "inactive" })]);
    listMcpKeysMock.mockResolvedValue([makeKey()]);

    renderPage();

    expect(await screen.findByText("Broken one")).toBeInTheDocument();
    expect(screen.getByText("run a health check")).toBeInTheDocument();
  });

  it("computes the 24h row correctly from a mixed set of mcp.* rows, excluding mcp.session.started", async () => {
    listConnectorsMock.mockResolvedValue([]);
    getAuditLogsMock.mockResolvedValue([
      makeLog({ action: "mcp.session.started" }), // must not count as a "call"
      makeLog({ action: "mcp.tool.called", metadata: { tool: "sheets_query_rows", duration_ms: 120 } }),
      makeLog({ action: "mcp.tool.called", metadata: { tool: "sheets_query_rows", duration_ms: 3400 } }),
      makeLog({ action: "mcp.tool.called", metadata: { tool: "drive_get_file", duration_ms: 90 } }),
      makeLog({ action: "mcp.tool.denied", metadata: { tool: "drive_list_folder", missing_permission: "drive:read" } }),
      makeLog({ action: "mcp.ratelimit.hit", metadata: { tool: "sheets_query_rows" } }),
    ]);

    renderPage();

    // Waits on a data-dependent figure (not the static heading) so the
    // assertion doesn't race the audit-logs query's resolution.
    await screen.findByText("3");
    // denied: 1, rate-limited: 1
    expect(screen.getAllByText("1")).toHaveLength(2);
    // busiest tool: sheets_query_rows (2 calls), slowest tool: sheets_query_rows (3400ms)
    expect(screen.getAllByText("sheets_query_rows").length).toBeGreaterThan(0);
    expect(screen.getByText("3400ms")).toBeInTheDocument();
  });

  it("states the numbers are a lower bound when the response comes back at the cap", async () => {
    listConnectorsMock.mockResolvedValue([]);
    getAuditLogsMock.mockResolvedValue(
      Array.from({ length: 100 }, (_, i) =>
        makeLog({ id: `log-${i}`, action: "mcp.tool.called", metadata: { tool: "sheets_query_rows" } }),
      ),
    );

    renderPage();

    expect(await screen.findByText(/lower bound, not a total/)).toBeInTheDocument();
  });

  it("does not claim a lower bound when the window comes back under the cap", async () => {
    listConnectorsMock.mockResolvedValue([]);
    getAuditLogsMock.mockResolvedValue([makeLog({ action: "mcp.tool.called" })]);

    renderPage();

    await screen.findByText("1"); // the single call, once the query resolves
    expect(screen.queryByText(/lower bound, not a total/)).not.toBeInTheDocument();
  });

  it("renders a denied row's missing_permission through PermissionToken, and names the role that grants it", async () => {
    listConnectorsMock.mockResolvedValue([]);
    listRolesMock.mockResolvedValue([
      makeRole({ id: "role-sheets", name: "Sheets reader", permissions: [{ id: "p1", roleId: "role-sheets", action: "sheets:*", createdAt: new Date().toISOString() }] }),
    ]);
    getAuditLogsMock.mockResolvedValue([
      makeLog({
        action: "mcp.tool.denied",
        metadata: { tool: "sheets_query_rows", missing_permission: "sheets:read" },
      }),
    ]);

    renderPage();

    // PermissionToken splits the action into resource/verb spans.
    expect(await screen.findByText("sheets")).toBeInTheDocument();
    expect(screen.getByText("read")).toBeInTheDocument();
    expect(screen.getByText(/granted by/)).toBeInTheDocument();
    expect(screen.getByText("Sheets reader")).toBeInTheDocument();
  });

  it("says plainly when no existing role grants the missing permission", async () => {
    listConnectorsMock.mockResolvedValue([]);
    listRolesMock.mockResolvedValue([
      makeRole({ id: "role-other", name: "Billing", permissions: [{ id: "p1", roleId: "role-other", action: "billing:read", createdAt: new Date().toISOString() }] }),
    ]);
    getAuditLogsMock.mockResolvedValue([
      makeLog({
        action: "mcp.tool.denied",
        metadata: { tool: "drive_get_file", missing_permission: "drive:read" },
      }),
    ]);

    renderPage();

    expect(await screen.findByText(/no role grants/)).toBeInTheDocument();
    expect(screen.getByText("drive:read")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Roles" })).toHaveAttribute("href", "/roles");
  });
});
