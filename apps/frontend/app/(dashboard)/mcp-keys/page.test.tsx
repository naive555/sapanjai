import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { CreateMcpKeyResponse, McpKeyResponse } from "@/lib/api/endpoints";

vi.mock("@/lib/api/endpoints", () => ({
  listMcpKeys: vi.fn(),
  createMcpKey: vi.fn(),
  revokeMcpKey: vi.fn(),
}));

vi.mock("@/lib/org/active-org", () => ({
  useActiveOrgId: () => "org-1",
}));

import { createMcpKey, listMcpKeys, revokeMcpKey } from "@/lib/api/endpoints";
import McpKeysPage from "./page";

const listMcpKeysMock = vi.mocked(listMcpKeys);
const createMcpKeyMock = vi.mocked(createMcpKey);
const revokeMcpKeyMock = vi.mocked(revokeMcpKey);

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <McpKeysPage />
    </QueryClientProvider>,
  );
  return queryClient;
}

// Scans a Storage object for a substring across both keys and values, since
// the raw key must never end up in either.
function storageContains(storage: Storage, needle: string): boolean {
  for (let i = 0; i < storage.length; i++) {
    const key = storage.key(i);
    if (!key) continue;
    if (key.includes(needle) || (storage.getItem(key) ?? "").includes(needle)) return true;
  }
  return false;
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  // @testing-library/react's automatic afterEach cleanup relies on a global
  // `afterEach` being registered before it's imported; this repo's vitest
  // config doesn't set `test.globals`, so unmount explicitly instead of
  // relying on that auto-registration.
  cleanup();
  vi.clearAllMocks();
});

describe("McpKeysPage", () => {
  it("renders the fetched keys with correct derived status", async () => {
    const now = Date.now();
    const keys: McpKeyResponse[] = [
      {
        id: "key-active",
        organizationId: "org-1",
        userId: "user-1",
        name: "Alpha key",
        scopes: null,
        lastUsedAt: null,
        expiresAt: null,
        revokedAt: null,
        createdAt: new Date(now).toISOString(),
      },
      {
        id: "key-expired",
        organizationId: "org-1",
        userId: "user-1",
        name: "Beta key",
        scopes: ["sheets:read"],
        lastUsedAt: null,
        expiresAt: new Date(now - 1000 * 60 * 60).toISOString(),
        revokedAt: null,
        createdAt: new Date(now - 1000 * 60 * 60 * 24).toISOString(),
      },
      {
        id: "key-revoked",
        organizationId: "org-1",
        userId: "user-1",
        name: "Gamma key",
        scopes: [],
        lastUsedAt: null,
        // Revoked takes precedence even though it's also past expiry.
        expiresAt: new Date(now - 1000 * 60 * 60).toISOString(),
        revokedAt: new Date(now - 1000).toISOString(),
        createdAt: new Date(now - 1000 * 60 * 60 * 48).toISOString(),
      },
    ];
    listMcpKeysMock.mockResolvedValue(keys);

    renderPage();

    const rowActive = (await screen.findByText("Alpha key")).closest("tr")!;
    expect(within(rowActive).getByText("Active")).toBeInTheDocument();
    expect(within(rowActive).getByRole("button", { name: "Revoke" })).toBeInTheDocument();

    const rowExpired = screen.getByText("Beta key").closest("tr")!;
    expect(within(rowExpired).getByText("Expired")).toBeInTheDocument();
    // Deliberate choice: an expired key can still be revoked (bookkeeping,
    // not access control — expiry already blocks calls).
    expect(within(rowExpired).getByRole("button", { name: "Revoke" })).toBeInTheDocument();

    const rowRevoked = screen.getByText("Gamma key").closest("tr")!;
    expect(within(rowRevoked).getByText("Revoked")).toBeInTheDocument();
    expect(within(rowRevoked).queryByRole("button", { name: "Revoke" })).not.toBeInTheDocument();
  });

  it("reveals the raw key exactly once and never touches storage", async () => {
    listMcpKeysMock.mockResolvedValue([]);
    const created: CreateMcpKeyResponse = {
      id: "new-key",
      name: "New key",
      apiKey: "sk_live_abcdef0123456789",
      expiresAt: null,
      createdAt: new Date().toISOString(),
    };
    createMcpKeyMock.mockResolvedValue(created);

    const queryClient = renderPage();

    await screen.findByText(/no mcp keys yet/i);

    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    const nameInput = await screen.findByLabelText("Name");
    fireEvent.change(nameInput, { target: { value: "New key" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(createMcpKeyMock).toHaveBeenCalledWith({ name: "New key", expiresInDays: undefined }));

    expect(await screen.findByText("sk_live_abcdef0123456789")).toBeInTheDocument();
    expect(storageContains(localStorage, "sk_live_abcdef0123456789")).toBe(false);
    expect(storageContains(sessionStorage, "sk_live_abcdef0123456789")).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: /saved this key/i }));

    await waitFor(() => {
      expect(screen.queryByText("sk_live_abcdef0123456789")).not.toBeInTheDocument();
    });
    // Not just gone from the DOM: useMutation caches its last `data` (the
    // whole create response, raw token included), and that entry outlives
    // the hook's own reset() unless the cache drops it too.
    await waitFor(() => {
      const cached = JSON.stringify(queryClient.getMutationCache().getAll().map((m) => m.state));
      expect(cached).not.toContain("sk_live_abcdef0123456789");
    });
    expect(storageContains(localStorage, "sk_live_abcdef0123456789")).toBe(false);
    expect(storageContains(sessionStorage, "sk_live_abcdef0123456789")).toBe(false);
  });

  it("hands back a wiring command carrying the real token, and says why a tool may be missing", async () => {
    listMcpKeysMock.mockResolvedValue([]);
    createMcpKeyMock.mockResolvedValue({
      id: "new-key",
      name: "New key",
      apiKey: "sk_live_abcdef0123456789",
      expiresAt: null,
      createdAt: new Date().toISOString(),
    } satisfies CreateMcpKeyResponse);

    renderPage();
    await screen.findByText(/no mcp keys yet/i);
    fireEvent.click(screen.getByRole("button", { name: /create key/i }));
    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "New key" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    const command = (await screen.findByText(/claude mcp add sapanjai/)).textContent ?? "";

    // The token is interpolated into the command, not left as a placeholder:
    // this dialog is the only moment it exists, so making the reader paste it
    // in a second time is a step that can only go wrong.
    expect(command).toContain("sk_live_abcdef0123456789");

    // --header is variadic and swallows whatever follows it, so a command
    // with the URL after the flag silently registers a server with no
    // address. Order is the correctness property here, not cosmetics.
    expect(command.indexOf("/mcp/")).toBeLessThan(command.indexOf("--header"));

    // "Why can't the agent see drive_get_file" is a support ticket unless the
    // separate-permission rule is stated where the key is handed over.
    expect(screen.getByText("drive:read")).toBeInTheDocument();
    expect(screen.getByText("drive_get_file")).toBeInTheDocument();
  });

  it("rejects an out-of-range expiry instead of silently minting a never-expiring key", async () => {
    listMcpKeysMock.mockResolvedValue([]);

    renderPage();

    await screen.findByText(/no mcp keys yet/i);
    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    fireEvent.change(await screen.findByLabelText("Name"), { target: { value: "Overflow key" } });
    // Out of range, and short enough to survive the number input's value
    // sanitization — a digit string long enough to parse to Infinity (the
    // case the ceiling exists for) is blanked by the input itself before any
    // resolver sees it, in jsdom and in a real browser alike, so the bound is
    // what's testable here.
    fireEvent.change(screen.getByLabelText(/expires in/i), { target: { value: "99999" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));

    expect(await screen.findByText(/between 1 and 3650/i)).toBeInTheDocument();
    expect(createMcpKeyMock).not.toHaveBeenCalled();
  });

  it("revokes the selected key and refetches the list", async () => {
    const key: McpKeyResponse = {
      id: "key-1",
      organizationId: "org-1",
      userId: "user-1",
      name: "Alpha key",
      scopes: null,
      lastUsedAt: null,
      expiresAt: null,
      revokedAt: null,
      createdAt: new Date().toISOString(),
    };
    listMcpKeysMock.mockResolvedValueOnce([key]).mockResolvedValueOnce([{ ...key, revokedAt: new Date().toISOString() }]);
    revokeMcpKeyMock.mockResolvedValue({ success: true });

    renderPage();

    await screen.findByText("Alpha key");
    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));

    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(revokeMcpKeyMock.mock.calls[0]?.[0]).toBe("key-1"));
    await waitFor(() => expect(listMcpKeysMock).toHaveBeenCalledTimes(2));
  });
});
