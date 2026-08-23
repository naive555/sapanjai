"use client";

import { useState } from "react";
import Link from "next/link";
import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { Controller, useForm } from "react-hook-form";
import { toast } from "sonner";
import { z } from "zod";

import { Callout } from "@/components/callout";
import { CopyableCode } from "@/components/copyable-code";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { PermissionToken } from "@/components/permission-token";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import { copyToClipboard } from "@/lib/clipboard";
import {
  createMcpKey,
  listMcpKeys,
  revokeMcpKey,
  type CreateMcpKeyResponse,
  type McpKeyResponse,
} from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";

// Ten years. The ceiling is not a backend rule — it exists because an
// unbounded digit string parses to Infinity, which JSON.stringify writes as
// null, silently minting a never-expiring key instead of rejecting the input.
const MAX_EXPIRES_IN_DAYS = 3650;

// Mirrors permActionPattern in apps/backend/internal/server/validator.go —
// that file is the source of truth for what a valid RBAC action string looks
// like. This is a client-side echo of it so a malformed free-text scope
// surfaces as an inline field error instead of a round-trip 422.
const PERM_ACTION_PATTERN = /^(\*|[a-z][a-z0-9_]*:(\*|[a-z][a-z0-9_]*))$/;

// Mirrors the `max=64` on CreateRequest.Scopes (mcpkey/dto.go).
const MAX_SCOPES = 64;

// The actions the gateway actually gates today (docs/08-gateway-core.md §3):
// sapanjai_describe_connector on connector:read, the sheets_*/drive_* read
// tools on sheets:read/drive:read. The catalog grows with each new adapter,
// so anything else — a future FlowAccount action, say — goes through the
// free-text field below rather than waiting on this list.
const SCOPE_CHECKBOXES = [
  { scope: "connector:read", field: "scopeConnectorRead" },
  { scope: "sheets:read", field: "scopeSheetsRead" },
  { scope: "drive:read", field: "scopeDriveRead" },
] as const;

// Splits a free-form scope entry on commas and/or whitespace — same
// forgiving parse as parsePermissions on the roles page
// (app/(dashboard)/roles/page.tsx), applied to the same shape of input.
function parseCustomScopes(raw: string | undefined): string[] {
  if (!raw) return [];
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

const createKeySchema = z
  .object({
    name: z.string().min(1, "Name is required").max(100, "Name must be 100 characters or fewer"),
    // Kept as a string on the form (a plain text input), then parsed to a
    // number — or omitted entirely — in the mutation below. Blank means "never
    // expires", so it has to stay distinguishable from "0".
    expiresInDays: z
      .string()
      .optional()
      .refine(
        (value) =>
          !value || (/^\d+$/.test(value) && Number(value) >= 1 && Number(value) <= MAX_EXPIRES_IN_DAYS),
        { message: `Must be a whole number between 1 and ${MAX_EXPIRES_IN_DAYS}` },
      ),
    scopeConnectorRead: z.boolean(),
    scopeSheetsRead: z.boolean(),
    scopeDriveRead: z.boolean(),
    // Free text for anything outside SCOPE_CHECKBOXES. A single string field
    // can't carry a per-token check on its own, so it's parsed and validated
    // token-by-token in superRefine below.
    customScopes: z.string().optional(),
  })
  .superRefine((values, ctx) => {
    const tokens = parseCustomScopes(values.customScopes);
    const invalidToken = tokens.find((token) => !PERM_ACTION_PATTERN.test(token));
    if (invalidToken) {
      ctx.addIssue({
        code: "custom",
        path: ["customScopes"],
        message: `"${invalidToken}" isn't a valid action — expected "*", "resource:verb", or "resource:*".`,
      });
      return;
    }
    const checkboxCount = [values.scopeConnectorRead, values.scopeSheetsRead, values.scopeDriveRead].filter(
      Boolean,
    ).length;
    if (checkboxCount + tokens.length > MAX_SCOPES) {
      ctx.addIssue({
        code: "custom",
        path: ["customScopes"],
        message: `Too many scopes selected (max ${MAX_SCOPES}).`,
      });
    }
  });
type CreateKeyValues = z.infer<typeof createKeySchema>;

type KeyStatus = "active" | "expired" | "revoked";

// Derived, not stored: the API only ever gives back raw timestamps.
// Precedence matters — a revoked key that also happens to be past its
// expiry is reported as "revoked", since that's the action an owner took on
// purpose, not a side effect of the clock.
function deriveStatus(key: McpKeyResponse): KeyStatus {
  if (key.revokedAt) return "revoked";
  if (key.expiresAt && new Date(key.expiresAt).getTime() <= Date.now()) return "expired";
  return "active";
}

function StatusBadge({ status }: { status: KeyStatus }) {
  if (status === "revoked") return <Badge variant="destructive">Revoked</Badge>;
  if (status === "expired") return <Badge variant="outline">Expired</Badge>;
  return <Badge variant="secondary">Active</Badge>;
}

function formatDate(value: string | null): string {
  return value ? new Date(value).toISOString().slice(0, 10) : "—";
}

export default function McpKeysPage() {
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  // The raw key lives here and in createMutation's own cached `data` — no
  // localStorage, no sessionStorage, no query cache entry. Both are cleared
  // by dismissReveal below, the moment the reveal dialog is dismissed.
  const [revealKey, setRevealKey] = useState<CreateMcpKeyResponse | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<{ id: string; name: string } | null>(null);

  const {
    data: keys,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["mcp-keys", activeOrgId],
    queryFn: listMcpKeys,
    enabled: activeOrgId !== null,
    // A member without mcpkey:read gets a deterministic 403, not a
    // transient failure — retrying it three times with backoff would just
    // delay the denied state the table needs to render instead of spinning.
    retry: false,
  });

  const invalidateKeys = () => queryClient.invalidateQueries({ queryKey: ["mcp-keys", activeOrgId] });

  // ---- create key ----
  const createForm = useForm<CreateKeyValues>({
    resolver: zodResolver(createKeySchema),
    defaultValues: {
      name: "",
      expiresInDays: "",
      scopeConnectorRead: false,
      scopeSheetsRead: false,
      scopeDriveRead: false,
      customScopes: "",
    },
  });

  const createMutation = useMutation({
    // The response `data` is the one and only copy of the raw key, so the
    // cache entry has to go when dismissReveal resets the mutation —
    // reset() alone just detaches the hook and leaves the entry sitting in
    // the MutationCache for gcTime (5 minutes by default).
    gcTime: 0,
    mutationFn: (values: CreateKeyValues) => {
      // Deduped: checking connector:read *and* typing it into the free-text
      // field is one scope, not two — the backend would otherwise store the
      // repeat verbatim in the text[] column and the table would render the
      // same token twice. Set preserves insertion order, so the checkbox
      // actions stay ahead of the free-text ones.
      const scopes = [
        ...new Set([
          ...(values.scopeConnectorRead ? ["connector:read"] : []),
          ...(values.scopeSheetsRead ? ["sheets:read"] : []),
          ...(values.scopeDriveRead ? ["drive:read"] : []),
          ...parseCustomScopes(values.customScopes),
        ]),
      ];
      return createMcpKey({
        name: values.name,
        expiresInDays: values.expiresInDays ? Number(values.expiresInDays) : undefined,
        // Invariant, load-bearing: an empty selection must omit `scopes`
        // from the request body entirely, never send `scopes: []`. The
        // backend rejects an empty list with a 422 by design
        // (docs/08-gateway-core.md §4, Q4) — a key that permits nothing is
        // a configuration mistake, not "unrestricted". Omitting the field is
        // how "ride the creator's live permissions" is spelled on the wire.
        ...(scopes.length > 0 ? { scopes } : {}),
      });
    },
    onSuccess: (created) => {
      invalidateKeys();
      createForm.reset();
      setCreateOpen(false);
      // Hand off straight into the reveal state — this response is the only
      // place the raw key will ever appear.
      setRevealKey(created);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to create MCP key.");
    },
  });

  // ---- revoke key ----
  const revokeMutation = useMutation({
    mutationFn: revokeMcpKey,
    onSuccess: () => {
      invalidateKeys();
      toast.success("Key revoked.");
      setRevokeTarget(null);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to revoke key.");
      setRevokeTarget(null);
    },
  });

  // Dismissing the reveal has to clear both copies of the raw key: the state
  // var the dialog renders from, and the mutation hook's cached `data`, which
  // otherwise keeps the plaintext token in memory until the page unmounts.
  function dismissReveal() {
    setRevealKey(null);
    createMutation.reset();
  }

  async function handleCopy(value: string) {
    const copied = await copyToClipboard(value);
    if (copied) {
      toast.success("Copied to clipboard.");
    } else {
      toast.error("Couldn't copy automatically — select the key above and copy it manually.");
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="mcp keys"
        description="API keys that let an MCP client (Claude Code, Claude Desktop, ...) act as this organization."
      >
        <Dialog open={createOpen} onOpenChange={setCreateOpen}>
          <DialogTrigger render={<Button size="sm" />}>
            <PlusIcon className="size-4" /> Create key
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Create MCP key</DialogTitle>
              <DialogDescription>
                The raw key is shown once, right after creation. It cannot be recovered afterward.
              </DialogDescription>
            </DialogHeader>
            <form onSubmit={createForm.handleSubmit((values) => createMutation.mutate(values))} noValidate>
              <FieldGroup>
                <Field data-invalid={!!createForm.formState.errors.name}>
                  <FieldLabel htmlFor="key-name">Name</FieldLabel>
                  <Controller
                    control={createForm.control}
                    name="name"
                    render={({ field }) => (
                      <Input id="key-name" placeholder="Claude Code — laptop" {...field} />
                    )}
                  />
                  <FieldError errors={[createForm.formState.errors.name]} />
                </Field>
                <Field data-invalid={!!createForm.formState.errors.expiresInDays}>
                  <FieldLabel htmlFor="key-expires">Expires in (days)</FieldLabel>
                  <Controller
                    control={createForm.control}
                    name="expiresInDays"
                    render={({ field }) => (
                      <Input
                        id="key-expires"
                        type="number"
                        min={1}
                        max={MAX_EXPIRES_IN_DAYS}
                        step={1}
                        placeholder="Never expires"
                        {...field}
                      />
                    )}
                  />
                  <FieldDescription>Leave blank for a key that never expires.</FieldDescription>
                  <FieldError errors={[createForm.formState.errors.expiresInDays]} />
                </Field>
                <FieldSet>
                  <FieldLegend variant="label">Scopes</FieldLegend>
                  <FieldDescription>
                    Leave everything below untouched and the key rides your current permissions, re-checked
                    on every call. Selecting scopes narrows the key to that subset instead — narrows only,
                    never widens.
                  </FieldDescription>
                  <div className="flex flex-col gap-2">
                    {SCOPE_CHECKBOXES.map(({ scope, field: fieldName }) => {
                      const inputId = `key-scope-${scope.replace(":", "-")}`;
                      return (
                        <Field key={scope} orientation="horizontal" className="gap-2">
                          <Controller
                            control={createForm.control}
                            name={fieldName}
                            render={({ field }) => (
                              <Checkbox
                                id={inputId}
                                checked={field.value}
                                onCheckedChange={field.onChange}
                              />
                            )}
                          />
                          <FieldLabel htmlFor={inputId} className="font-mono text-sm font-normal">
                            {scope}
                          </FieldLabel>
                        </Field>
                      );
                    })}
                  </div>
                  <Field data-invalid={!!createForm.formState.errors.customScopes}>
                    <FieldLabel htmlFor="key-scope-custom">Other scopes</FieldLabel>
                    <Controller
                      control={createForm.control}
                      name="customScopes"
                      render={({ field }) => (
                        <Input id="key-scope-custom" placeholder="billing:read, invoice:write" {...field} />
                      )}
                    />
                    <FieldDescription>Comma- or space-separated. Only needed for an action outside the list above.</FieldDescription>
                    <FieldError errors={[createForm.formState.errors.customScopes]} />
                  </Field>
                </FieldSet>
              </FieldGroup>
              <DialogFooter>
                <Button type="submit" disabled={createMutation.isPending}>
                  {createMutation.isPending ? "Creating…" : "Create"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </PageHeader>

      <DataTable
        columns={["Name", "Scopes", "Status", "Last used", "Expires", "Created", { label: "", align: "end" }]}
      >
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={7}>Loading…</TableMessage>
          ) : isError ? (
            <TableMessage colSpan={7}>
              {error instanceof ApiError && error.status === 403
                ? "You don't have permission to view MCP keys in this organization."
                : "Failed to load MCP keys."}
            </TableMessage>
          ) : keys?.length ? (
            keys.map((key) => {
              const status = deriveStatus(key);
              // Revoking an already-revoked key is a no-op the backend
              // happily accepts, but offering the action again reads as
              // broken, so it's hidden once revoked. An expired key can
              // still be revoked deliberately — it's not access control
              // (expiry already blocks calls) but bookkeeping, so an owner
              // can still record it as intentionally retired.
              const canRevoke = status !== "revoked";

              return (
                <TableRow key={key.id}>
                  <TableCell className="font-medium">{key.name}</TableCell>
                  <TableCell>
                    {key.scopes === null ? (
                      <PermissionToken action="*" />
                    ) : key.scopes.length ? (
                      <div className="flex flex-wrap gap-1">
                        {key.scopes.map((scope) => (
                          <PermissionToken key={scope} action={scope} />
                        ))}
                      </div>
                    ) : (
                      <span className="text-xs text-muted-foreground">No scopes</span>
                    )}
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={status} />
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {formatDate(key.lastUsedAt)}
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {key.expiresAt ? formatDate(key.expiresAt) : "Never"}
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {formatDate(key.createdAt)}
                  </TableCell>
                  <TableCell className="text-right">
                    {canRevoke && (
                      <Button
                        variant="ghost"
                        size="xs"
                        className="text-muted-foreground hover:text-destructive"
                        onClick={() => setRevokeTarget({ id: key.id, name: key.name })}
                      >
                        Revoke
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              );
            })
          ) : (
            <TableMessage colSpan={7}>
              No MCP keys yet. Create one to let an agent connect to this organization.
            </TableMessage>
          )}
        </TableBody>
      </DataTable>

      {/* Revoke confirmation — matches the members page's remove-member dialog. */}
      <Dialog open={revokeTarget !== null} onOpenChange={(open) => !open && setRevokeTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke key</DialogTitle>
            <DialogDescription>
              Revoke <span className="font-mono text-foreground">{revokeTarget?.name}</span>? Any client using it
              loses access immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRevokeTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={revokeMutation.isPending}
              onClick={() => revokeTarget && revokeMutation.mutate(revokeTarget.id)}
            >
              {revokeMutation.isPending ? "Revoking…" : "Revoke"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/*
       * Raw-key reveal. onOpenChange is deliberately a no-op: with `open`
       * pinned to `revealKey !== null`, ignoring the callback means Escape
       * and backdrop clicks can't close this dialog, so the only way out is
       * the explicit confirm button below — which is also the one place
       * revealKey is cleared from state.
       */}
      <Dialog open={revealKey !== null} onOpenChange={() => {}}>
        <DialogContent showCloseButton={false} className="sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>Key created — copy it now</DialogTitle>
            <DialogDescription>
              This is the only time the raw key is shown. Once this dialog is closed, Sapanjai cannot
              display it again.
            </DialogDescription>
          </DialogHeader>
          {revealKey && (
            <div className="flex max-h-[60vh] flex-col gap-3 overflow-y-auto">
              <div className="rounded-md border bg-muted/40 p-3">
                <code className="block w-full overflow-x-auto font-mono text-sm break-all select-all">
                  {revealKey.apiKey}
                </code>
              </div>
              <Button type="button" variant="outline" onClick={() => void handleCopy(revealKey.apiKey)}>
                Copy to clipboard
              </Button>
              <p className="text-sm font-medium text-destructive">
                You will not see this key again. Store it somewhere safe before closing this dialog.
              </p>

              {/*
                * The wiring snippet is here, and only here, on purpose: this
                * is the one moment the raw key exists to be pasted into it.
                * A copy of this on a docs page would have a placeholder where
                * the token is, which is exactly the step people get wrong.
                */}
              <div className="space-y-2 border-t pt-3">
                <p className="text-sm font-medium">Point a client at a connector</p>
                <p className="text-sm text-muted-foreground">
                  The endpoint is one per connector. Replace the host with your Sapanjai API address (the API,
                  not this dashboard) and <span className="font-mono">CONNECTOR_ID</span> with the ID shown on
                  the connector&apos;s row on{" "}
                  <Link
                    href="/connectors"
                    className="text-foreground underline underline-offset-4 hover:text-signal"
                  >
                    connectors
                  </Link>
                  .
                </p>
                <CopyableCode
                  label="the client wiring command"
                  value={`claude mcp add sapanjai --scope local --transport http \\
  https://YOUR_SAPANJAI_HOST/mcp/CONNECTOR_ID \\
  --header "Authorization: Bearer ${revealKey.apiKey}"`}
                />
                <p className="text-sm text-muted-foreground">
                  The URL has to come before <span className="font-mono">--header</span> — that flag is
                  variadic and will otherwise swallow the address. Calling the endpoint directly is a{" "}
                  <span className="font-mono">POST</span> to the same URL with that{" "}
                  <span className="font-mono">Authorization</span> header.
                </p>
              </div>

              <Callout variant="boundary" title="Missing a tool? It was filtered, not broken">
                An agent only ever sees the tools this key is permitted to call — anything else is absent from
                its tool list entirely, rather than failing when called. The one that catches people out:{" "}
                <span className="font-mono">drive:read</span> is a separate permission, and{" "}
                <span className="font-mono">sheets:read</span> never implies it. A key whose holder has only{" "}
                <span className="font-mono">sheets:read</span> will not show{" "}
                <span className="font-mono">drive_list_folder</span> or{" "}
                <span className="font-mono">drive_get_file</span> at all. Grants are re-read on every call, so
                fixing the role takes effect on the agent&apos;s next request — no need to reconnect it.
              </Callout>
            </div>
          )}
          <DialogFooter>
            <Button type="button" onClick={dismissReveal}>
              I&apos;ve saved this key — close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
