import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { AuditLogResponse, ConnectorResponse, McpKeyResponse } from "@/lib/api/endpoints";
import { ConnectorSpan } from "./connector-span";

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

function makeLog(overrides: Partial<AuditLogResponse> = {}): AuditLogResponse {
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

afterEach(() => {
  cleanup();
});

describe("ConnectorSpan", () => {
  it("renders the onboarding checklist, fully dashed, when nothing is set up yet", () => {
    render(<ConnectorSpan connector={makeConnector()} mcpKeys={[]} recentLogs={[]} />);

    expect(screen.getByText("no agent has connected yet")).toBeInTheDocument();
    expect(screen.getByText(/mint a key/)).toBeInTheDocument();
    expect(screen.getByText("run a health check")).toBeInTheDocument();
    expect(screen.getByText("not reachable until the connector is active")).toBeInTheDocument();
  });

  it("emphasises only the leftmost broken node — an inactive connector doesn't relight the key node behind it", () => {
    const connector = makeConnector({ status: "inactive" });
    const keys = [makeKey()];
    const logs = [makeLog({ action: "mcp.session.started", metadata: { connector_id: "conn-1" } })];

    render(<ConnectorSpan connector={connector} mcpKeys={keys} recentLogs={logs} />);

    // Agent and key are both satisfied, so neither reads as broken...
    expect(screen.getByText(/last seen/)).toBeInTheDocument();
    expect(screen.getByText(/org-wide, not per-connector/)).toBeInTheDocument();
    // ...but the connector itself is the (leftmost) broken node, and its
    // caption names the fix.
    expect(screen.getByText("run a health check")).toBeInTheDocument();
    // Nothing downstream of it lights up either.
    expect(screen.getByText("not reachable until the connector is active")).toBeInTheDocument();
  });

  it("renders a fully connected chain with no broken node once everything is set up", () => {
    const connector = makeConnector({ status: "active", lastHealthCheckAt: new Date().toISOString() });
    const keys = [makeKey()];
    const logs = [
      makeLog({ action: "mcp.session.started", metadata: { connector_id: "conn-1" } }),
      makeLog({ action: "mcp.tool.called", metadata: { connector_id: "conn-1" } }),
    ];

    render(<ConnectorSpan connector={connector} mcpKeys={keys} recentLogs={logs} />);

    expect(screen.getByText("active")).toBeInTheDocument();
    expect(screen.queryByText("run a health check")).not.toBeInTheDocument();
    expect(screen.queryByText("mint a key")).not.toBeInTheDocument();
    expect(screen.queryByText("no agent has connected yet")).not.toBeInTheDocument();
  });

  it("counts only usable keys (not revoked, not expired) and flags one expiring within 7 days", () => {
    const now = Date.now();
    const keys: McpKeyResponse[] = [
      makeKey({ id: "usable", revokedAt: null, expiresAt: null }),
      makeKey({ id: "revoked", revokedAt: new Date(now).toISOString() }),
      makeKey({ id: "expired", expiresAt: new Date(now - 1000).toISOString() }),
      makeKey({ id: "expiring-soon", expiresAt: new Date(now + 1000 * 60 * 60 * 24 * 2).toISOString() }),
    ];

    render(<ConnectorSpan connector={makeConnector()} mcpKeys={keys} recentLogs={[]} />);

    expect(screen.getByText("2 keys")).toBeInTheDocument();
    expect(screen.getByText(/1 expiring within 7 days/)).toBeInTheDocument();
  });

  it("surfaces the denial count from recent logs, not just the call count", () => {
    const connector = makeConnector({ status: "active", lastHealthCheckAt: new Date().toISOString() });
    const logs = [
      makeLog({ action: "mcp.tool.called", metadata: { connector_id: "conn-1" } }),
      makeLog({ action: "mcp.tool.called", metadata: { connector_id: "conn-1" } }),
      makeLog({ action: "mcp.tool.denied", metadata: { connector_id: "conn-1" } }),
    ];

    render(<ConnectorSpan connector={connector} mcpKeys={[makeKey()]} recentLogs={logs} />);

    expect(screen.getByText("2 calls")).toBeInTheDocument();
    expect(screen.getByText("1 denied")).toBeInTheDocument();
  });
});
