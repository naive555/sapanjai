import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AuditLogResponse, ConnectorResponse } from "@/lib/api/endpoints";

vi.mock("@/lib/api/endpoints", () => ({
  getConnector: vi.fn(),
  listMcpKeys: vi.fn(),
  getAuditLogs: vi.fn(),
  healthCheckConnector: vi.fn(),
}));

vi.mock("@/lib/org/active-org", () => ({
  useActiveOrgId: () => "org-1",
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

import { ApiError } from "@/lib/api/client";
import { getAuditLogs, getConnector, listMcpKeys } from "@/lib/api/endpoints";
import { ConnectorDetailClient } from "./page-client";

const getConnectorMock = vi.mocked(getConnector);
const listMcpKeysMock = vi.mocked(listMcpKeys);
const getAuditLogsMock = vi.mocked(getAuditLogs);

function renderPage(connectorId = "conn-1", gatewayUrl: string | null = "http://gateway.example") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ConnectorDetailClient connectorId={connectorId} gatewayUrl={gatewayUrl} />
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

function makeToolCalledLog(overrides: Partial<AuditLogResponse> = {}): AuditLogResponse {
  return {
    id: "log-1",
    organizationId: "org-1",
    userId: "user-1",
    action: "mcp.tool.called",
    metadata: {},
    createdAt: new Date().toISOString(),
    ...overrides,
  };
}

beforeEach(() => {
  listMcpKeysMock.mockResolvedValue([]);
  getAuditLogsMock.mockResolvedValue([]);
});

afterEach(() => {
  // This repo's vitest config has no test.globals, so @testing-library's
  // automatic afterEach cleanup can't rely on a global afterEach being
  // registered before it's imported — unmount explicitly (same idiom as
  // the sibling connector pages' test files).
  cleanup();
  vi.clearAllMocks();
});

describe("ConnectorDetailClient", () => {
  it("renders the endpoint and wiring snippet with the connector id resolved and a token placeholder", async () => {
    getConnectorMock.mockResolvedValue(makeConnector());

    renderPage();

    await screen.findByText("Endpoint");

    const body = document.body.textContent ?? "";
    expect(body).toContain("http://gateway.example/mcp/conn-1");
    expect(body).toContain("<paste your key>");
    // No real token ever flows into this page's props — only the placeholder
    // above stands in for one, unlike the once-only reveal on /mcp-keys.
    expect(body).not.toMatch(/sk_live_\w+/);
  });

  it("puts the endpoint URL before --header, which is the correctness property of the command", async () => {
    getConnectorMock.mockResolvedValue(makeConnector());

    renderPage();

    const command = (await screen.findByText(/claude mcp add sapanjai/)).textContent ?? "";

    // --header is variadic and swallows whatever follows it, so a command
    // with the URL after the flag silently registers a server with no
    // address. Order is the correctness property here, not cosmetics — this
    // assertion moved here with the snippet itself, off the /mcp-keys reveal
    // dialog that used to be the command's only home.
    expect(command.indexOf("/mcp/")).toBeLessThan(command.indexOf("--header"));
  });

  it("says the endpoint host is a guess when GATEWAY_URL is unset for the deployment", async () => {
    getConnectorMock.mockResolvedValue(makeConnector());

    renderPage("conn-1", null);

    // The fallback is the dashboard's own origin, which is the API host only
    // by coincidence — the note is what stops a wrong host being copied out
    // of here and quietly pasted into an agent's config.
    expect(await screen.findByText("Endpoint host is a guess")).toBeInTheDocument();
    expect(document.body.textContent ?? "").toContain(`${window.location.origin}/mcp/conn-1`);
  });

  it("renders a generic connector fully, without assuming google_sheets-only fields", async () => {
    getConnectorMock.mockResolvedValue(makeConnector({ type: "generic", name: "Generic one" }));

    renderPage();

    await screen.findByText("Endpoint");
    expect(screen.getAllByText("Generic").length).toBeGreaterThan(0);
    expect(screen.queryByRole("link", { name: "Google Sheets configuration" })).not.toBeInTheDocument();
    // The boundary Callout names sheets/drive tools that don't exist for a
    // generic connector — it must not render for one.
    expect(screen.queryByText(/drive:read/)).not.toBeInTheDocument();
    expect(screen.getByText("Health")).toBeInTheDocument();
  });

  it("renders the span fully dashed, with onboarding labels, when nothing is set up yet", async () => {
    getConnectorMock.mockResolvedValue(makeConnector());
    listMcpKeysMock.mockResolvedValue([]);
    getAuditLogsMock.mockResolvedValue([]);

    renderPage();

    await screen.findByText("Endpoint");
    expect(screen.getByText("no agent has connected yet")).toBeInTheDocument();
    expect(screen.getByText(/mint a key/)).toBeInTheDocument();
    expect(screen.getByText("run a health check")).toBeInTheDocument();
  });

  it("filters recent activity to this connector's connector_id, excluding another connector's rows", async () => {
    getConnectorMock.mockResolvedValue(makeConnector());
    getAuditLogsMock.mockResolvedValue([
      makeToolCalledLog({
        id: "log-mine",
        metadata: { connector_id: "conn-1", tool: "sheets_query_rows" },
      }),
      makeToolCalledLog({
        id: "log-other",
        metadata: { connector_id: "conn-2", tool: "sheets_read_range" },
      }),
    ]);

    renderPage();

    await screen.findByText("Endpoint");
    expect(await screen.findByText("sheets_query_rows")).toBeInTheDocument();
    expect(screen.queryByText("sheets_read_range")).not.toBeInTheDocument();
  });

  it("renders the wrong-org/nonexistent copy on a 404", async () => {
    getConnectorMock.mockRejectedValue(new ApiError(404, "Resource not found"));

    renderPage();

    expect(await screen.findByText(/doesn't exist, or belongs to a different organization/i)).toBeInTheDocument();
  });
});
