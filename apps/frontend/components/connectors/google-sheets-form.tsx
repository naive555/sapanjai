"use client";

import Link from "next/link";
import { zodResolver } from "@hookform/resolvers/zod";
import { Controller, useForm, useWatch, type Control } from "react-hook-form";
import { z } from "zod";

import { Callout } from "@/components/callout";
import { CopyableCode } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Textarea } from "@/components/ui/textarea";

// The exact scopes the adapter requests when it exchanges a refresh token
// (apps/backend/internal/adapter/googlesheets/oauth.go). Shown here because a
// token minted with the wrong ones fails only at health-check time, with a
// message that deliberately says nothing about why — so the cheapest place to
// catch the mistake is beside the field where the token is pasted.
const SHEETS_SCOPE = "https://www.googleapis.com/auth/spreadsheets.readonly";
const DRIVE_SCOPE = "https://www.googleapis.com/auth/drive.readonly";

// Splits a one-id-per-line (commas also accepted) textarea into string[] —
// same idiom as roles/page.tsx's permissions textarea.
function parseIdList(text: string | undefined): string[] {
  return (text ?? "")
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

// header_rows is a spreadsheet-id -> row-number map
// (internal/adapter/googlesheets/config.go's optionalHeaderRows), entered
// here as one "spreadsheet_id:row_number" per line.
function parseHeaderRows(text: string | undefined): { headerRows: Record<string, number>; error?: string } {
  const lines = (text ?? "")
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);

  const headerRows: Record<string, number> = {};
  for (const line of lines) {
    const separator = line.lastIndexOf(":");
    const id = separator === -1 ? "" : line.slice(0, separator).trim();
    const rowText = separator === -1 ? "" : line.slice(separator + 1).trim();
    const row = Number(rowText);
    if (!id || !rowText || !/^\d+$/.test(rowText) || row < 1) {
      return {
        headerRows: {},
        error: `"${line}" must be "spreadsheet_id:row_number" with a whole number row of 1 or more`,
      };
    }
    headerRows[id] = row;
  }
  return { headerRows };
}

// Validates a pasted service-account key file before it ever reaches the
// backend. Mirrors config.service_account.key_json's requirement in
// internal/adapter/googlesheets/config.go (must parse, must yield a
// client_email), plus a "type" check the backend doesn't need — Go's
// google.JWTConfigFromJSON already refuses a malformed key on its own, but
// nothing there stops the wrong Google credential file (an OAuth client
// secret, an "authorized_user" file) from being pasted here instead. A paste
// error caught in the browser is far cheaper than one surfaced later as a
// failed health check, whose reason the API deliberately never returns.
function parseServiceAccountKey(text: string | undefined): { email?: string; error?: string } {
  const trimmed = (text ?? "").trim();
  if (!trimmed) {
    return { error: "Service account key JSON is required." };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return { error: "Not valid JSON — paste the entire downloaded key file." };
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return { error: "Not valid JSON — paste the entire downloaded key file." };
  }
  const record = parsed as Record<string, unknown>;
  if (record.type !== "service_account") {
    return { error: 'Not a service account key — its "type" field must be "service_account".' };
  }
  const email = typeof record.client_email === "string" ? record.client_email.trim() : "";
  if (!email) {
    return { error: "Missing client_email — paste the entire downloaded key file, not a partial copy." };
  }
  return { email };
}

// The plain field shape, with no cross-field rule yet — exported so the
// connectors list page's create-connector form can `.merge()` these fields
// into its own (name + type + these) schema without duplicating them.
export const googleSheetsConfigFieldsSchema = z.object({
  // Service account is the default (docs/07-sheets-adapter-decisions.md):
  // it has no consent screen and no refresh token for Google to expire
  // after seven days, so the recommended path is the one a customer falls
  // into without choosing anything.
  credentialKind: z.enum(["service_account", "oauth"]),
  serviceAccountKeyJson: z.string().optional(),
  clientId: z.string().optional(),
  clientSecret: z.string().optional(),
  refreshToken: z.string().optional(),
  spreadsheetIdsText: z.string().optional(),
  driveFolderIdsText: z.string().optional(),
  headerRowsText: z.string().optional(),
});

export type GoogleSheetsFieldValues = z.infer<typeof googleSheetsConfigFieldsSchema>;

// The one place the google_sheets config rule lives (mirrors ParseConfig in
// internal/adapter/googlesheets/config.go): exactly one credential variant
// validated depending on credentialKind (a parseable service-account key, or
// all three OAuth fields), at least one allowlist non-empty, header_rows
// optional but must parse. Both call sites — this file's standalone
// GoogleSheetsForm (the edit page) and the connectors list page's
// create-connector form — run values through this same function, so the
// rule can't drift between them.
export function refineGoogleSheetsConfig(values: GoogleSheetsFieldValues, ctx: z.RefinementCtx) {
  if (values.credentialKind === "service_account") {
    const { error } = parseServiceAccountKey(values.serviceAccountKeyJson);
    if (error) {
      ctx.addIssue({ code: "custom", path: ["serviceAccountKeyJson"], message: error });
    }
  } else {
    // All three trimmed, not just checked for emptiness: these are pasted by
    // hand, and the backend's requiredString only rejects the literal empty
    // string — so a lone space would sail through both sides and only
    // surface as a health check that fails with a reason the API
    // deliberately never returns.
    if (!values.clientId?.trim()) {
      ctx.addIssue({ code: "custom", path: ["clientId"], message: "Client ID is required." });
    }
    if (!values.clientSecret?.trim()) {
      ctx.addIssue({ code: "custom", path: ["clientSecret"], message: "Client secret is required." });
    }
    if (!values.refreshToken?.trim()) {
      ctx.addIssue({ code: "custom", path: ["refreshToken"], message: "Refresh token is required." });
    }
  }

  const spreadsheetIds = parseIdList(values.spreadsheetIdsText);
  const driveFolderIds = parseIdList(values.driveFolderIdsText);
  if (spreadsheetIds.length === 0 && driveFolderIds.length === 0) {
    ctx.addIssue({
      code: "custom",
      path: ["spreadsheetIdsText"],
      message:
        "Allowlist at least one spreadsheet or Drive folder — this is the adapter's security boundary, enforced independently of whatever the credential can otherwise reach.",
    });
  }

  const headerRows = parseHeaderRows(values.headerRowsText);
  if (headerRows.error) {
    ctx.addIssue({ code: "custom", path: ["headerRowsText"], message: headerRows.error });
  }
}

// Standalone schema for this file's own GoogleSheetsForm (the edit page,
// which only ever deals with google_sheets connectors).
export const googleSheetsConfigSchema = googleSheetsConfigFieldsSchema.superRefine(refineGoogleSheetsConfig);

// Converts validated field values into the exact nested snake_case shape
// PATCH /connectors/:id expects (docs/02-api-contract.md). Submitting this
// always carries the full config — there is no partial/merge update, so a
// caller must not omit a field just because it "didn't change": nothing was
// pre-filled to begin with.
export function toGoogleSheetsConfig(values: GoogleSheetsFieldValues): Record<string, unknown> {
  const spreadsheetIds = parseIdList(values.spreadsheetIdsText);
  const driveFolderIds = parseIdList(values.driveFolderIdsText);
  const { headerRows } = parseHeaderRows(values.headerRowsText);

  const scope: Record<string, unknown> = {
    spreadsheet_ids: spreadsheetIds,
    drive_folder_ids: driveFolderIds,
  };
  if (Object.keys(headerRows).length > 0) {
    scope.header_rows = headerRows;
  }

  if (values.credentialKind === "service_account") {
    return {
      service_account: {
        // Trimmed for the same reason as the OAuth fields below: a file
        // pasted from an editor or a terminal routinely carries a trailing
        // newline.
        key_json: values.serviceAccountKeyJson?.trim(),
      },
      scope,
    };
  }

  return {
    oauth: {
      // Trimmed for the same reason the id lists are: a credential copied
      // from a terminal or a docs page routinely carries a trailing newline,
      // and Google rejects it verbatim with an error this UI never gets to
      // see.
      refresh_token: values.refreshToken?.trim(),
      client_id: values.clientId?.trim(),
      client_secret: values.clientSecret?.trim(),
    },
    scope,
  };
}

const emptyDefaults: GoogleSheetsFieldValues = {
  credentialKind: "service_account",
  serviceAccountKeyJson: "",
  clientId: "",
  clientSecret: "",
  refreshToken: "",
  spreadsheetIdsText: "",
  driveFolderIdsText: "",
  headerRowsText: "",
};

/**
 * The fields only — no `<form>`, no submit button — so a caller that needs
 * these fields embedded inside a larger form (the connectors list page's
 * create-connector dialog, which also collects name + type) can render them
 * without nesting one `<form>` inside another.
 */
// react-hook-form's `Control<T>` doesn't stay assignable once T is generic
// (Path<T> can't be resolved against an unresolved type parameter), so a
// caller embedding these fields inside a larger form (name + type + these)
// narrows its own wider Control down to this exact shape when passing it in
// — see the create-connector form on the connectors list page. `errors` is
// typed structurally instead of via react-hook-form's own FieldErrors<T>,
// since only `.message` is ever read here and every field-values type's
// error object is structurally compatible with that.
export function GoogleSheetsFormFields({
  control,
  errors,
  mode = "replace",
}: {
  control: Control<GoogleSheetsFieldValues>;
  errors: Partial<Record<keyof GoogleSheetsFieldValues, { message?: string } | undefined>>;
  // "create" has nothing stored to replace yet, so it gets the forward-
  // looking half of the same fact rather than a warning about overwriting
  // something that doesn't exist.
  mode?: "create" | "replace";
}) {
  // useWatch (not the form's own .watch()) — same reasoning as the create-
  // connector form's `selectedType`: the latter returns a function React
  // Compiler can't safely memoize.
  const credentialKind = useWatch({ control, name: "credentialKind" });
  const serviceAccountKeyJson = useWatch({ control, name: "serviceAccountKeyJson" });
  // Recomputed on every keystroke rather than only at submit time, so the
  // echoed client_email below (and its absence) tracks what's currently
  // pasted, not what was last valid.
  const parsedServiceAccount = parseServiceAccountKey(serviceAccountKeyJson);

  return (
    <FieldGroup>
      <Callout>
        {mode === "replace"
          ? "Submitting replaces the entire stored configuration — both the credentials and the allowlist — since nothing here can be pre-filled: no endpoint ever returns a stored config back."
          : "Credentials are sealed at rest and never returned by the API, so this form can't be pre-filled later — a future edit replaces the whole configuration rather than merging into it."}{" "}
        Don&apos;t have these values yet?{" "}
        <Link
          href="/connectors/google-sheets-setup"
          className="text-foreground underline underline-offset-4 hover:text-signal"
        >
          Walk through getting them from Google
        </Link>
        .
      </Callout>

      <Field data-invalid={!!errors.credentialKind}>
        <FieldLabel>Credential type</FieldLabel>
        <Controller
          control={control}
          name="credentialKind"
          render={({ field }) => (
            <RadioGroup value={field.value} onValueChange={field.onChange}>
              <Field orientation="horizontal" className="gap-2">
                <RadioGroupItem value="service_account" id="gs-credential-service-account" />
                <FieldLabel htmlFor="gs-credential-service-account" className="font-normal">
                  Service account (recommended)
                </FieldLabel>
              </Field>
              <Field orientation="horizontal" className="gap-2">
                <RadioGroupItem value="oauth" id="gs-credential-oauth" />
                <FieldLabel htmlFor="gs-credential-oauth" className="font-normal">
                  OAuth (refresh token)
                </FieldLabel>
              </Field>
            </RadioGroup>
          )}
        />
        <FieldDescription>
          A service account has no consent screen and no refresh token for Google to expire after seven
          days — share each spreadsheet/folder below with its address, the way you&apos;d share it with a
          colleague. Choose OAuth only if a Google Workspace admin blocks sharing outside the domain, which
          is the one thing a service account can&apos;t work around.
        </FieldDescription>
        <FieldError errors={[errors.credentialKind]} />
      </Field>

      {credentialKind === "service_account" ? (
        <Field data-invalid={!!errors.serviceAccountKeyJson}>
          <FieldLabel htmlFor="gs-service-account-key">Service account key (JSON)</FieldLabel>
          <Controller
            control={control}
            name="serviceAccountKeyJson"
            render={({ field }) => (
              <Textarea
                id="gs-service-account-key"
                rows={8}
                className="font-mono text-sm"
                placeholder={'{\n  "type": "service_account",\n  "client_email": "...",\n  ...\n}'}
                // This field holds a private key, so it carries the same
                // no-autofill treatment as the OAuth secret inputs below.
                // spellCheck is off for a reason beyond the red squiggles:
                // a browser's enhanced spellcheck ships textarea contents to
                // a remote service, which for a private key is a leak the
                // rest of this codebase works hard to prevent.
                autoComplete="off"
                spellCheck={false}
                {...field}
              />
            )}
          />
          <FieldDescription>
            Paste the entire JSON key file Google downloads when you create the service account — not an
            excerpt.
          </FieldDescription>
          {/* Echoed only once the paste parses — see parseServiceAccountKey.
              A stored key is never returned by the API, so there is nothing
              to echo here on the edit page until a fresh paste is validated. */}
          {parsedServiceAccount.email ? (
            <FieldDescription>
              Share each spreadsheet/folder below with{" "}
              <span className="font-mono text-foreground">{parsedServiceAccount.email}</span> (Viewer) — that
              address, not your own Google account, is what has to be granted access.
            </FieldDescription>
          ) : null}
          <FieldError errors={[errors.serviceAccountKeyJson]} />
        </Field>
      ) : (
        <>
          <Field data-invalid={!!errors.clientId}>
            <FieldLabel htmlFor="gs-client-id">Client ID</FieldLabel>
            <Controller
              control={control}
              name="clientId"
              render={({ field }) => <Input id="gs-client-id" autoComplete="off" {...field} />}
            />
            <FieldError errors={[errors.clientId]} />
          </Field>

          <Field data-invalid={!!errors.clientSecret}>
            <FieldLabel htmlFor="gs-client-secret">Client secret</FieldLabel>
            <Controller
              control={control}
              name="clientSecret"
              render={({ field }) => (
                <Input id="gs-client-secret" type="password" autoComplete="new-password" {...field} />
              )}
            />
            <FieldError errors={[errors.clientSecret]} />
          </Field>

          <Field data-invalid={!!errors.refreshToken}>
            <FieldLabel htmlFor="gs-refresh-token">Refresh token</FieldLabel>
            <Controller
              control={control}
              name="refreshToken"
              render={({ field }) => (
                <Input id="gs-refresh-token" type="password" autoComplete="new-password" {...field} />
              )}
            />
            <FieldDescription>
              Pasted manually for now — there is no OAuth consent flow in the dashboard yet. The token has to
              carry exactly these two scopes, and no others:
            </FieldDescription>
            <CopyableCode value={SHEETS_SCOPE} label="the read-only Sheets scope" />
            <CopyableCode value={DRIVE_SCOPE} label="the read-only Drive scope" />
            <FieldDescription>
              Note the <span className="font-mono">.readonly</span> on each. Nothing in the gateway writes to
              a sheet, so a token carrying write scopes gains you nothing and widens what a leak would cost.
            </FieldDescription>
            <FieldError errors={[errors.refreshToken]} />
          </Field>
        </>
      )}

      <Callout variant="boundary" title="This allowlist is the boundary — not the credentials">
        Every request is checked against these two lists before Sapanjai calls Google. An ID that is not
        listed here is refused even when the connector&apos;s own credential could open it perfectly well —
        that is what stops an agent talked into asking for the wrong document from getting it. The flip side
        is the mistake nearly everyone makes once: a spreadsheet you forgot to list simply will not work, no
        matter how correct the credentials are.
      </Callout>

      <Field data-invalid={!!errors.spreadsheetIdsText}>
        <FieldLabel htmlFor="gs-spreadsheet-ids">Allowed spreadsheet IDs</FieldLabel>
        <Controller
          control={control}
          name="spreadsheetIdsText"
          render={({ field }) => (
            <Textarea
              id="gs-spreadsheet-ids"
              rows={3}
              className="font-mono text-sm"
              placeholder={"1AbC...\n1XyZ..."}
              {...field}
            />
          )}
        />
        <FieldDescription>One spreadsheet ID per line.</FieldDescription>
        <FieldError errors={[errors.spreadsheetIdsText]} />
      </Field>

      <Field data-invalid={!!errors.driveFolderIdsText}>
        <FieldLabel htmlFor="gs-folder-ids">Allowed Drive folder IDs</FieldLabel>
        <Controller
          control={control}
          name="driveFolderIdsText"
          render={({ field }) => (
            <Textarea
              id="gs-folder-ids"
              rows={3}
              className="font-mono text-sm"
              placeholder={"0B1a..."}
              {...field}
            />
          )}
        />
        <FieldDescription>
          One Drive folder ID per line. At least one spreadsheet or folder must be listed above or here.
          Folders don&apos;t cascade: a file is reachable only if the folder listed here is its{" "}
          <em>direct</em> parent, so nested subfolders need listing too.
        </FieldDescription>
        <FieldError errors={[errors.driveFolderIdsText]} />
      </Field>

      <Field data-invalid={!!errors.headerRowsText}>
        <FieldLabel htmlFor="gs-header-rows">Header row overrides (optional)</FieldLabel>
        <Controller
          control={control}
          name="headerRowsText"
          render={({ field }) => (
            <Textarea
              id="gs-header-rows"
              rows={2}
              className="font-mono text-sm"
              placeholder={"1AbC...:3"}
              {...field}
            />
          )}
        />
        <FieldDescription>
          One &ldquo;spreadsheet_id:row_number&rdquo; per line, for a sheet whose real header isn&apos;t row 1
          (e.g. a title banner). Defaults to row 1 when omitted.
        </FieldDescription>
        <FieldError errors={[errors.headerRowsText]} />
      </Field>

      <Callout title="Prove it works before handing it to an agent">
        Saving stores the configuration; it does not test it. Run{" "}
        <span className="font-medium text-foreground">Run health check</span> from the connector&apos;s row on{" "}
        <Link href="/connectors" className="text-foreground underline underline-offset-4 hover:text-signal">
          connectors
        </Link>{" "}
        — it exchanges the credential for a live access token and reads one allowlisted document with it,
        then flips the connector to <span className="font-medium text-foreground">active</span>. It probes the
        first spreadsheet on the list (or the first folder, if you listed no spreadsheets), so a pass proves
        the credentials and that one ID — not every ID here.
      </Callout>
    </FieldGroup>
  );
}

/**
 * Standalone form for the `/connectors/[id]/google-sheets` edit page: its
 * own `<form>`, its own submit button, always starting from empty fields
 * (config is write-only — see the ground rule at the top of this file's
 * exports).
 */
export function GoogleSheetsForm({
  onSubmit,
  submitting,
  submitLabel = "Save",
}: {
  // Awaited: when it resolves, the fields are cleared, so a saved client
  // secret and refresh token don't linger in the inputs (and in react-hook-
  // form's state) on a page the user typically stays on after saving. A
  // rejection leaves the values in place so the submit can be retried — the
  // caller owns reporting the failure.
  onSubmit: (config: Record<string, unknown>) => void | Promise<unknown>;
  submitting: boolean;
  submitLabel?: string;
}) {
  const form = useForm<GoogleSheetsFieldValues>({
    resolver: zodResolver(googleSheetsConfigSchema),
    defaultValues: emptyDefaults,
  });

  return (
    <form
      onSubmit={form.handleSubmit(async (values) => {
        try {
          await onSubmit(toGoogleSheetsConfig(values));
          form.reset(emptyDefaults);
        } catch {
          // Reported by the caller; keep what was typed so it can be retried.
        }
      })}
      noValidate
      className="flex flex-col gap-5"
    >
      <GoogleSheetsFormFields control={form.control} errors={form.formState.errors} />
      <div className="flex justify-end">
        <Button type="submit" disabled={submitting}>
          {submitting ? "Saving…" : submitLabel}
        </Button>
      </div>
    </form>
  );
}
