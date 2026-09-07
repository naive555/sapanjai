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

// The form defaults to the service-account toggle position, so every OAuth
// test switches to it first — this is the one seam every one of them shares
// with the toggle itself.
function selectOAuth() {
  fireEvent.click(screen.getByRole("radio", { name: "OAuth (refresh token)" }));
}

function fillOAuthFields() {
  selectOAuth();
  fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "client-abc.apps.googleusercontent.com" } });
  fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: "shh-secret-value" } });
  fireEvent.change(screen.getByLabelText("Refresh token"), { target: { value: "1//0g-refresh-token-value" } });
}

const VALID_SERVICE_ACCOUNT_KEY = JSON.stringify({
  type: "service_account",
  client_email: "sheets-bot@my-project.iam.gserviceaccount.com",
  private_key: "-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----\n",
});

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

    selectOAuth();
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

    selectOAuth();
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
    // form.reset(emptyDefaults) also resets the credential toggle back to
    // its default position, so the OAuth fields disappear entirely rather
    // than merely going blank — the service-account field taking their
    // place is the visible proof nothing lingers.
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(screen.getByRole("radio", { name: "Service account (recommended)" })).toBeChecked(),
    );
    expect(screen.queryByLabelText("Client secret")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Refresh token")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Client ID")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Service account key (JSON)")).toHaveValue("");
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

  // The scopes are restated as literals rather than imported from the
  // component, so that a typo introduced there fails here instead of being
  // echoed back. A refresh token minted with a wrong scope is only caught by
  // the health check, which by design reports nothing about why it failed.
  it("prints the exact read-only scopes a refresh token has to carry", () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    selectOAuth();
    expect(
      screen.getByText("https://www.googleapis.com/auth/spreadsheets.readonly"),
    ).toBeInTheDocument();
    expect(screen.getByText("https://www.googleapis.com/auth/drive.readonly")).toBeInTheDocument();
  });

  it("points at the walkthrough for the credentials it can't help you obtain", () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    expect(screen.getByRole("link", { name: /walk through/i })).toHaveAttribute(
      "href",
      "/connectors/google-sheets-setup",
    );
  });

  it("says plainly that the allowlist, not the token, is what bounds access", () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    // The single most common support question is a correctly-credentialled
    // connector that "can't see" a sheet nobody added to the list, so the
    // form has to make the boundary explicit rather than implying it.
    expect(screen.getByText(/allowlist is the boundary/i)).toBeInTheDocument();
  });

  it("renders the secret fields as password inputs so they aren't shoulder-surfed", () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    selectOAuth();
    expect(screen.getByLabelText("Client secret")).toHaveAttribute("type", "password");
    expect(screen.getByLabelText("Refresh token")).toHaveAttribute("type", "password");
  });

  it("defaults to the service-account credential type, with its key field visible and the OAuth fields hidden", () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    expect(screen.getByRole("radio", { name: "Service account (recommended)" })).toBeChecked();
    expect(screen.getByLabelText("Service account key (JSON)")).toBeInTheDocument();
    expect(screen.queryByLabelText("Client ID")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Client secret")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Refresh token")).not.toBeInTheDocument();
  });

  it("swaps the credential fieldset when the toggle changes, in either direction", () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    selectOAuth();
    expect(screen.getByLabelText("Client ID")).toBeInTheDocument();
    expect(screen.queryByLabelText("Service account key (JSON)")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "Service account (recommended)" }));
    expect(screen.getByLabelText("Service account key (JSON)")).toBeInTheDocument();
    expect(screen.queryByLabelText("Client ID")).not.toBeInTheDocument();
  });

  it("submits a service_account config with the exact key_json shape the backend expects", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fireEvent.change(screen.getByLabelText("Service account key (JSON)"), {
      target: { value: VALID_SERVICE_ACCOUNT_KEY },
    });
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(onSubmit).toHaveBeenCalledWith({
        service_account: { key_json: VALID_SERVICE_ACCOUNT_KEY },
        scope: { spreadsheet_ids: ["1AbC"], drive_folder_ids: [] },
      }),
    );
  });

  it("echoes the service account's client_email once the pasted key parses, as the address to share a sheet with", async () => {
    render(<GoogleSheetsForm onSubmit={vi.fn()} submitting={false} />);

    fireEvent.change(screen.getByLabelText("Service account key (JSON)"), {
      target: { value: VALID_SERVICE_ACCOUNT_KEY },
    });

    expect(
      await screen.findByText("sheets-bot@my-project.iam.gserviceaccount.com"),
    ).toBeInTheDocument();
  });

  it("rejects a malformed pasted key before submit rather than sending it to the backend", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fireEvent.change(screen.getByLabelText("Service account key (JSON)"), {
      target: { value: "not actually json" },
    });
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/not valid json/i)).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("rejects a pasted key of the wrong Google credential type before submit", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    // A real, parseable file — just not a service-account key (this is the
    // shape of an OAuth client secret's own downloadable JSON).
    fireEvent.change(screen.getByLabelText("Service account key (JSON)"), {
      target: { value: JSON.stringify({ type: "authorized_user", client_id: "abc" }) },
    });
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(/must be "service_account"/i)).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("never writes the pasted service-account key to localStorage or sessionStorage", async () => {
    const onSubmit = vi.fn();
    render(<GoogleSheetsForm onSubmit={onSubmit} submitting={false} />);

    fireEvent.change(screen.getByLabelText("Service account key (JSON)"), {
      target: { value: VALID_SERVICE_ACCOUNT_KEY },
    });
    fireEvent.change(screen.getByLabelText("Allowed spreadsheet IDs"), { target: { value: "1AbC" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(onSubmit).toHaveBeenCalled());

    expect(storageContains(localStorage, "sheets-bot@my-project.iam.gserviceaccount.com")).toBe(false);
    expect(storageContains(sessionStorage, "sheets-bot@my-project.iam.gserviceaccount.com")).toBe(false);
  });
});
