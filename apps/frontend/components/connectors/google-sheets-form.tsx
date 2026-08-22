"use client";

import { zodResolver } from "@hookform/resolvers/zod";
import { Controller, useForm, type Control } from "react-hook-form";
import { z } from "zod";

import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";

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

// The plain field shape, with no cross-field rule yet — exported so the
// connectors list page's create-connector form can `.merge()` these fields
// into its own (name + type + these) schema without duplicating them.
export const googleSheetsConfigFieldsSchema = z.object({
  clientId: z.string().optional(),
  clientSecret: z.string().optional(),
  refreshToken: z.string().optional(),
  spreadsheetIdsText: z.string().optional(),
  driveFolderIdsText: z.string().optional(),
  headerRowsText: z.string().optional(),
});

export type GoogleSheetsFieldValues = z.infer<typeof googleSheetsConfigFieldsSchema>;

// The one place the google_sheets config rule lives (mirrors ParseConfig in
// internal/adapter/googlesheets/config.go): all three OAuth fields
// required, at least one allowlist non-empty, header_rows optional but must
// parse. Both call sites — this file's standalone GoogleSheetsForm (the
// edit page) and the connectors list page's create-connector form — run
// values through this same function, so the rule can't drift between them.
export function refineGoogleSheetsConfig(values: GoogleSheetsFieldValues, ctx: z.RefinementCtx) {
  // All three trimmed, not just checked for emptiness: these are pasted by
  // hand, and the backend's requiredString only rejects the literal empty
  // string — so a lone space would sail through both sides and only surface
  // as a health check that fails with a reason the API deliberately never
  // returns.
  if (!values.clientId?.trim()) {
    ctx.addIssue({ code: "custom", path: ["clientId"], message: "Client ID is required." });
  }
  if (!values.clientSecret?.trim()) {
    ctx.addIssue({ code: "custom", path: ["clientSecret"], message: "Client secret is required." });
  }
  if (!values.refreshToken?.trim()) {
    ctx.addIssue({ code: "custom", path: ["refreshToken"], message: "Refresh token is required." });
  }

  const spreadsheetIds = parseIdList(values.spreadsheetIdsText);
  const driveFolderIds = parseIdList(values.driveFolderIdsText);
  if (spreadsheetIds.length === 0 && driveFolderIds.length === 0) {
    ctx.addIssue({
      code: "custom",
      path: ["spreadsheetIdsText"],
      message:
        "Allowlist at least one spreadsheet or Drive folder — this is the adapter's security boundary, enforced independently of whatever the OAuth account can otherwise reach.",
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
  return (
    <FieldGroup>
      <div className="rounded-md border border-dashed bg-muted/40 px-3 py-2.5 text-sm text-muted-foreground">
        {mode === "replace"
          ? "Submitting replaces the entire stored configuration — both the OAuth credentials and the allowlist — since nothing here can be pre-filled: no endpoint ever returns a stored config back."
          : "Credentials are sealed at rest and never returned by the API, so this form can't be pre-filled later — a future edit replaces the whole configuration rather than merging into it."}
      </div>

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
          Pasted manually for now — there is no OAuth consent flow in the dashboard yet.
        </FieldDescription>
        <FieldError errors={[errors.refreshToken]} />
      </Field>

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
