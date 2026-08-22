import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ConnectorResponse } from "@/lib/api/endpoints";

vi.mock("@/lib/api/endpoints", () => ({
  getConnector: vi.fn(),
  updateConnector: vi.fn(),
}));

vi.mock("@/lib/org/active-org", () => ({
  useActiveOrgId: () => "org-1",
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ id: "conn-1" }),
}));

import { ApiError } from "@/lib/api/client";
import { getConnector, updateConnector } from "@/lib/api/endpoints";
import GoogleSheetsConnectorPage from "./page";

const getConnectorMock = vi.mocked(getConnector);
const updateConnectorMock = vi.mocked(updateConnector);

const SECRET = "shh-client-secret";

const sheetsConnector: ConnectorResponse = {
  id: "conn-1",
  organizationId: "org-1",
  name: "Acme sheet",
  type: "google_sheets",
  status: "inactive",
  lastHealthCheckAt: null,
  createdAt: new Date().toISOString(),
  updatedAt: new Date().toISOString(),
};

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <GoogleSheetsConnectorPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

function fillValidConfig() {
  fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "client-abc" } });
  fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: SECRET } });
  fireEvent.change(screen.getByLabelText("Refresh token"), { target: { value: "1//0g-token" } });
  fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("GoogleSheetsConnectorPage", () => {
  it("PATCHes the full config and leaves no credential behind in the mutation cache", async () => {
    getConnectorMock.mockResolvedValue(sheetsConnector);
    updateConnectorMock.mockResolvedValue({ ...sheetsConnector, status: "inactive" });

    const queryClient = renderPage();

    await screen.findByText("Acme sheet");
    fillValidConfig();
    fireEvent.click(screen.getByRole("button", { name: "Save configuration" }));

    await waitFor(() => expect(updateConnectorMock).toHaveBeenCalled());
    expect(updateConnectorMock.mock.calls[0][0]).toBe("conn-1");
    expect(updateConnectorMock.mock.calls[0][1]).toEqual({
      config: {
        oauth: { client_id: "client-abc", client_secret: SECRET, refresh_token: "1//0g-token" },
        scope: { spreadsheet_ids: ["1AbC"], drive_folder_ids: [] },
      },
    });

    // useMutation keeps its last `variables` (the plaintext config) until the
    // mutation is reset — the page has to clear it, or a saved secret sits in
    // the client for as long as this page stays mounted.
    await waitFor(() => {
      const cached = JSON.stringify(queryClient.getMutationCache().getAll().map((m) => m.state));
      expect(cached).not.toContain(SECRET);
    });
    expect(JSON.stringify(queryClient.getQueryData(["connectors", "org-1", "conn-1"]))).not.toContain(SECRET);
    expect(localStorage.getItem("config")).toBeNull();
    expect(sessionStorage.length).toBe(0);
  });

  it("renders a not-found state for another org's or a nonexistent id", async () => {
    getConnectorMock.mockRejectedValue(new ApiError(404, "Resource not found"));

    renderPage();

    expect(await screen.findByText(/doesn't exist, or belongs to a different organization/i)).toBeInTheDocument();
    expect(screen.queryByLabelText("Client secret")).not.toBeInTheDocument();
  });

  it("refuses to edit a connector of another type instead of showing the sheets form", async () => {
    getConnectorMock.mockResolvedValue({ ...sheetsConnector, type: "generic" });

    renderPage();

    await screen.findByText(/only edits google sheets configuration/i);
    expect(screen.queryByLabelText("Client secret")).not.toBeInTheDocument();
  });
});
