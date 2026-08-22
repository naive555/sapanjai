"use client";

import { useState } from "react";
import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { Controller, useForm } from "react-hook-form";
import { toast } from "sonner";
import { z } from "zod";

import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { PermissionToken } from "@/components/permission-token";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
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

const createKeySchema = z.object({
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

// Copies the raw key to the clipboard. The Clipboard API is absent in jsdom
// and in any non-secure-context browser tab, so this has to degrade to a
// manual-copy nudge rather than throwing past the caller.
async function copyToClipboard(value: string): Promise<boolean> {
  try {
    if (!navigator.clipboard?.writeText) return false;
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
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
    defaultValues: { name: "", expiresInDays: "" },
  });

  const createMutation = useMutation({
    mutationFn: (values: CreateKeyValues) =>
      createMcpKey({
        name: values.name,
        expiresInDays: values.expiresInDays ? Number(values.expiresInDays) : undefined,
      }),
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
        <DialogContent showCloseButton={false}>
          <DialogHeader>
            <DialogTitle>Key created — copy it now</DialogTitle>
            <DialogDescription>
              This is the only time the raw key is shown. Once this dialog is closed, Sapanjai cannot
              display it again.
            </DialogDescription>
          </DialogHeader>
          {revealKey && (
            <div className="flex flex-col gap-3">
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
