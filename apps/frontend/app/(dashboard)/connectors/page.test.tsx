import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ConnectorResponse } from "@/lib/api/endpoints";

vi.mock("@/lib/api/endpoints", () => ({
  listConnectors: vi.fn(),
  createConnector: vi.fn(),
  deleteConnector: vi.fn(),
  healthCheckConnector: vi.fn(),
}));

vi.mock("@/lib/org/active-org", () => ({
  useActiveOrgId: () => "org-1",
}));

// Health-check failure surfaces only as a toast (the row itself is
// untouched on error), so asserting the "unsupported type" behaviour needs
// the actual message sonner was called with, not just a DOM change.
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

import { toast } from "sonner";
import { ApiError } from "@/lib/api/client";
import { createConnector, deleteConnector, healthCheckConnector, listConnectors } from "@/lib/api/endpoints";
import ConnectorsPage from "./page";

const listConnectorsMock = vi.mocked(listConnectors);
const createConnectorMock = vi.mocked(createConnector);
const deleteConnectorMock = vi.mocked(deleteConnector);
const healthCheckConnectorMock = vi.mocked(healthCheckConnector);
const toastErrorMock = vi.mocked(toast.error);

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <ConnectorsPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

function makeConnector(overrides: Partial<ConnectorResponse>): ConnectorResponse {
  return {
    id: "connector-1",
    organizationId: "org-1",
    name: "Some connector",
    type: "generic",
    status: "inactive",
    lastHealthCheckAt: null,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  // This repo's vitest config has no test.globals, so @testing-library's
  // automatic afterEach cleanup (which relies on a global afterEach being
  // registered before it's imported) can't be relied on — unmount explicitly.
  cleanup();
  vi.clearAllMocks();
});

describe("ConnectorsPage", () => {
  it("renders connectors with status visually distinguished across active/inactive/error", async () => {
    const connectors: ConnectorResponse[] = [
      makeConnector({ id: "c-active", name: "Billing sheet", type: "google_sheets", status: "active" }),
      makeConnector({ id: "c-inactive", name: "Fresh connector", type: "generic", status: "inactive" }),
      makeConnector({ id: "c-error", name: "Broken sheet", type: "google_sheets", status: "error" }),
    ];
    listConnectorsMock.mockResolvedValue(connectors);

    renderPage();

    const rowActive = (await screen.findByText("Billing sheet")).closest("tr")!;
    expect(within(rowActive).getByText("Active")).toBeInTheDocument();

    const rowInactive = screen.getByText("Fresh connector").closest("tr")!;
    expect(within(rowInactive).getByText("Inactive")).toBeInTheDocument();

    const rowError = screen.getByText("Broken sheet").closest("tr")!;
    expect(within(rowError).getByText("Error")).toBeInTheDocument();

    // Only a google_sheets row is a link through to its config page.
    expect(within(rowActive).getByRole("link", { name: "Billing sheet" })).toHaveAttribute(
      "href",
      "/connectors/c-active/google-sheets",
    );
    expect(within(rowInactive).queryByRole("link")).not.toBeInTheDocument();
  });

  it("deletes the selected connector with the right id and refetches the list", async () => {
    const connector = makeConnector({ id: "connector-9", name: "Doomed connector" });
    listConnectorsMock.mockResolvedValueOnce([connector]).mockResolvedValueOnce([]);
    deleteConnectorMock.mockResolvedValue({ success: true });

    renderPage();

    await screen.findByText("Doomed connector");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));

    // Only the first arg is asserted — react-query's mutationFn wrapper
    // passes a second (mutation-context) argument the real deleteConnector
    // signature doesn't have, same as the established idiom in
    // mcp-keys/page.test.tsx's revoke assertion.
    await waitFor(() => expect(deleteConnectorMock.mock.calls[0]?.[0]).toBe("connector-9"));
    await waitFor(() => expect(listConnectorsMock).toHaveBeenCalledTimes(2));
  });

  it("surfaces the unsupported-type message for a generic connector's health check, not a generic failure", async () => {
    const connector = makeConnector({ id: "connector-generic", name: "Generic one", type: "generic" });
    listConnectorsMock.mockResolvedValue([connector]);
    healthCheckConnectorMock.mockRejectedValue(
      new ApiError(501, "Health check not supported for this connector type"),
    );

    renderPage();

    await screen.findByText("Generic one");
    fireEvent.click(screen.getByRole("button", { name: "Run health check" }));

    await waitFor(() => expect(healthCheckConnectorMock.mock.calls[0]?.[0]).toBe("connector-generic"));
    await waitFor(() => expect(toastErrorMock).toHaveBeenCalledWith("This connector type has no health check yet."));
    // The generic-failure message must never fire for this specific case.
    expect(toastErrorMock).not.toHaveBeenCalledWith(
      expect.stringContaining("Health check failed"),
    );
  });

  it("creates a generic connector from raw JSON, parsed before the call", async () => {
    listConnectorsMock.mockResolvedValue([]);
    createConnectorMock.mockResolvedValue(makeConnector({ id: "new-generic", name: "New generic" }));

    renderPage();

    await screen.findByText(/no connectors yet/i);
    fireEvent.click(screen.getByRole("button", { name: /create connector/i }));

    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "New generic" } });
    fireEvent.change(screen.getByLabelText("Config (JSON)"), {
      target: { value: '{"apiKey":"abc123"}' },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() =>
      expect(createConnectorMock).toHaveBeenCalledWith({
        name: "New generic",
        type: "generic",
        config: { apiKey: "abc123" },
      }),
    );
  });
  it("creates a google_sheets connector and keeps the pasted secret out of the mutation cache", async () => {
    const secret = "shh-client-secret";
    listConnectorsMock.mockResolvedValue([]);
    createConnectorMock.mockResolvedValue(
      makeConnector({ id: "new-sheets", name: "Acme sheet", type: "google_sheets" }),
    );

    const queryClient = renderPage();

    await screen.findByText(/no connectors yet/i);
    fireEvent.click(screen.getByRole("button", { name: /create connector/i }));

    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Acme sheet" } });
    // Switching type swaps the raw-JSON textarea for the shared google-sheets
    // fields, which receive this form's (wider) Control — the wiring under
    // test as much as the payload is.
    // base-ui's Select opens on keyboard/pointer interaction and commits a
    // choice on the item's pointer sequence — a bare click on either does
    // nothing in jsdom.
    fireEvent.keyDown(screen.getByLabelText("Type"), { key: "ArrowDown" });
    const sheetsOption = await screen.findByRole("option", { name: "Google Sheets" });
    fireEvent.pointerDown(sheetsOption, { pointerType: "mouse", button: 0 });
    fireEvent.pointerUp(sheetsOption, { pointerType: "mouse", button: 0 });
    fireEvent.click(sheetsOption);

    fireEvent.change(await screen.findByLabelText("Client ID"), { target: { value: "client-abc" } });
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: secret } });
    fireEvent.change(screen.getByLabelText("Refresh token"), { target: { value: "1//0g-token" } });
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() =>
      expect(createConnectorMock).toHaveBeenCalledWith({
        name: "Acme sheet",
        type: "google_sheets",
        config: {
          oauth: { client_id: "client-abc", client_secret: secret, refresh_token: "1//0g-token" },
          scope: { spreadsheet_ids: ["1AbC"], drive_folder_ids: [] },
        },
      }),
    );

    // useMutation retains its last `variables` — here, the plaintext config —
    // until the entry leaves the MutationCache.
    await waitFor(() => {
      const cached = JSON.stringify(queryClient.getMutationCache().getAll().map((m) => m.state));
      expect(cached).not.toContain(secret);
    });
  });
});
