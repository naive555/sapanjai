import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { GoogleSheetsForm } from "./google-sheets-form";

// Scans a Storage object for a substring across both keys and values —
// mirrors the helper in app/(dashboard)/mcp-keys/page.test.tsx, since a
// pasted OAuth credential must never end up in either.
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
  cleanup();
  vi.clearAllMocks();
});

function fillOAuthFields() {
  fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "client-abc.apps.googleusercontent.com" } });
  fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "shh-secret-value" } });
  fireEvent.change(screen.getByLabelText("Refresh token"), { target: { value: "1//0g-refresh-token-value" } });
}

describe("GoogleSheetsForm", () => {
  it("rejects a config with both allowlists empty and does not call onSubmit", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fillOAuthFields();
    // Deliberately leave both "Allowed spreadsheet IDs" and "Allowed Drive
    // folder IDs" blank — the security-boundary rule under test.
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/allowlist at least one spreadsheet or drive folder/i)).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("rejects a whitespace-only secret, which the backend's own required-check would accept", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "client-abc" } });
    // ParseConfig's requiredString only rejects the literal empty string, so
    // if this passes here it reaches Google verbatim and fails a health check
    // whose reason the API deliberately never returns.
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "   " } });
    fireEvent.change(screen.getByLabelText("Refresh token"), { target: { value: "\n" } });
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText("Client secret is required.")).toBeInTheDocument();
    expect(screen.getByText("Refresh token is required.")).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("trims credentials pasted with surrounding whitespace", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    // The shape a terminal copy produces: a trailing newline on each value.
    fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "client-abc\n" } });
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "  shh-secret  " } });
    fireEvent.change(screen.getByLabelText("Refresh token"), { target: { value: "1//0g-token\n" } });
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(onSubmit).toHaveBeenCalled());
    expect(onSubmit.mock.calls[0][0]).toEqual({
      oauth: {
        client_id: "client-abc",
        client_secret: "shh-secret",
        refresh_token: "1//0g-token",
      },
      scope: { spreadsheet_ids: ["1AbC"], drive_folder_ids: [] },
    });
  });

  it("clears the credential fields once a submit resolves, but keeps them when it rejects", async () => {
    const onSubmit = vi.fn().mockRejectedValueOnce(new Error("save failed")).mockResolvedValueOnce(undefined);
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fillOAuthFields();
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });

    // First submit rejects: what was typed has to survive so it can be retried.
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(screen.getByLabelText("Client secret")).toHaveValue("shh-secret-value"),
    );

    // Second submit resolves: the secret must not linger in the input.
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByLabelText("Client secret")).toHaveValue(""));
    expect(screen.getByLabelText("Refresh token")).toHaveValue("");
    expect(screen.getByLabelText("Client ID")).toHaveValue("");
  });

  it("sends the exact snake_case nested config shape the backend expects on a valid submit", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fillOAuthFields();
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), {
      target: { value: "1AbCSpreadsheet\n1XyZSpreadsheet" },
    });
    fireEvent.change(screen.getByLabelText("Allowed Drive folder IDs"), {
      target: { value: "0B1aFolder" },
    });
    fireEvent.change(screen.getByLabelText(/header row overrides/i), {
      target: { value: "1AbCSpreadsheet:3" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(onSubmit).toHaveBeenCalledWith({
        oauth: {
          refresh_token: "1//0g-refresh-token-value",
          client_id: "client-abc.apps.googleusercontent.com",
          client_secret: "shh-secret-value",
        },
        scope: {
          spreadsheet_ids: ["1AbCSpreadsheet", "1XyZSpreadsheet"],
          drive_folder_ids: ["0B1aFolder"],
          header_rows: { "1AbCSpreadsheet": 3 },
        },
      }),
    );
  });

  it("never writes the pasted client secret or refresh token to localStorage or sessionStorage", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fillOAuthFields();
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbCSpreadsheet" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(onSubmit).toHaveBeenCalled());

    expect(storageContains(localStorage, "shh-secret-value")).toBe(false);
    expect(storageContains(localStorage, "1//0g-refresh-token-value")).toBe(false);
    expect(storageContains(sessionStorage, "shh-secret-value")).toBe(false);
    expect(storageContains(sessionStorage, "1//0g-refresh-token-value")).toBe(false);
  });

  it("renders the secret fields as password inputs so they aren't shoulder-surfed", () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    expect(screen.getByLabelText("Client secret")).toHaveAttribute("type", "password");
    expect(screen.getByLabelText("Refresh token")).toHaveAttribute("type", "password");
  });
});
