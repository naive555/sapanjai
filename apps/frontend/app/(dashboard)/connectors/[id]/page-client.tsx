"use client";

import { useState, useSyncExternalStore } from "react";
import Link from "next/link";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon } from "lucide-react";
import { toast } from "sonner";

import { ActionToken, ActivityDetail } from "@/components/activity-row";
import { Callout } from "@/components/callout";
import { ConnectorSpan } from "@/components/connector-span";
import { CopyableCode } from "@/components/copyable-code";
import { CopyableId } from "@/components/copyable-id";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import {
  getAuditLogs,
  getConnector,
  healthCheckConnector,
  listMcpKeys,
  type AuditLogResponse,
} from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";
import { cn } from "@/lib/utils";

// The API caps `limit` at 100 (docs/02-api-contract.md) and there's no
// `since` or repeatable-`action` filter yet (that's Phase 4), so the biggest
// window available is fetched unfiltered and narrowed to this connector's
// own mcp.* rows below — same client-side-filter approach the "Gateway
// only" toggle on /activity already uses for the same reason.
const AUDIT_FETCH_LIMIT = 100;
const RECENT_ACTIVITY_ROWS = 15;

function StatusBadge({ status }: { status: string }) {
  if (status === "active") return <Badge variant="secondary">Active</Badge>;
  if (status === "error") return <Badge variant="destructive">Error</Badge>;
  return <Badge variant="outline">Inactive</Badge>;
}

function formatDate(value: string | null): string {
  return value ? new Date(value).toISOString().slice(0, 10) : "never";
}

function typeLabel(type: string): string {
  return type === "google_sheets" ? "Google Sheets" : "Generic";
}

function connectorIdOf(log: AuditLogResponse): string | undefined {
  const value = log.metadata?.["connector_id"];
  return typeof value === "string" ? value : undefined;
}

// window.location only exists on the client, so the fallback (unset
// GATEWAY_URL) can't be resolved during the server render. Same
// useSyncExternalStore shape lib/org/active-org.ts and theme-toggle.tsx
// already use for client-only values: the store never actually changes, so
// the snapshot differs only between the server render (empty) and every
// client render (the real origin) — no effect, no cascading setState.
const neverChanges = () => () => {};
function useBrowserOrigin(): string {
  return useSyncExternalStore(neverChanges, () => window.location.origin, () => "");
}

export function ConnectorDetailClient({
  connectorId,
  gatewayUrl,
}: {
  connectorId: string;
  /** Server-resolved GATEWAY_URL, or null if unset in this deployment. */
  gatewayUrl: string | null;
}) {
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();

  // Unset GATEWAY_URL can only be resolved to something useful in the
  // browser — see useBrowserOrigin above.
  const browserOrigin = useBrowserOrigin();
  const resolvedGatewayUrl = gatewayUrl ?? browserOrigin;

  const {
    data: connector,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["connectors", activeOrgId, connectorId],
    queryFn: () => getConnector(connectorId),
    enabled: activeOrgId !== null && !!connectorId,
    // A wrong-org / nonexistent id is a deterministic 404 — render the
    // not-found state immediately rather than retrying it.
    retry: false,
  });

  // Org-wide, not scoped to this connector — ConnectorSpan says so in its
  // own caption, but the fetch itself is the same "every key this org has"
  // list the /mcp-keys page reads.
  const { data: mcpKeys } = useQuery({
    queryKey: ["mcp-keys", activeOrgId],
    queryFn: listMcpKeys,
    enabled: activeOrgId !== null,
  });

  const { data: auditLogs } = useQuery({
    queryKey: ["audit", activeOrgId, { limit: AUDIT_FETCH_LIMIT }],
    queryFn: () => getAuditLogs({ limit: AUDIT_FETCH_LIMIT }),
    enabled: activeOrgId !== null,
  });

  // This connector's own mcp.* rows, newest first (the API's own order),
  // capped for display — and the exact same set ConnectorSpan reads its
  // "last seen" / call / denial figures from, so the span and the table
  // beneath it never disagree about what "recent" means.
  const connectorLogs = (auditLogs ?? [])
    .filter((log) => log.action.startsWith("mcp.") && connectorIdOf(log) === connectorId)
    .slice(0, RECENT_ACTIVITY_ROWS);

  const [healthCheckResult, setHealthCheckResult] = useState<{ ok: boolean; message: string } | null>(null);

  const healthCheckMutation = useMutation({
    mutationFn: () => healthCheckConnector(connectorId),
    onSuccess: (updated) => {
      queryClient.setQueryData(["connectors", activeOrgId, connectorId], updated);
      queryClient.invalidateQueries({ queryKey: ["connectors", activeOrgId] });
      const message = `Status: ${updated.status}.`;
      setHealthCheckResult({ ok: updated.status === "active", message });
      toast.success(`Health check complete — ${message.toLowerCase()}`);
    },
    onError: (err) => {
      // "generic" has no adapter registered at all — every check against one
      // is a 501, by contract, not a probe that happened to fail. Mirrors
      // connectors/page.tsx's handling of the same error.
      if (err instanceof ApiError && err.status === 501) {
        const message = "This connector type has no health check yet.";
        setHealthCheckResult({ ok: false, message });
        toast.error(message);
      } else {
        const message = "Health check failed — the probe's own error is never returned; check the config.";
        setHealthCheckResult({ ok: false, message });
        toast.error(err instanceof ApiError ? message : "Failed to run health check.");
      }
    },
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="connector"
        description="Where this connector's endpoint lives, who's used it, and whether it's currently reachable."
      >
        {connector?.type === "google_sheets" && (
          <Button variant="outline" size="sm" render={<Link href={`/connectors/${connectorId}/google-sheets`} />}>
            Google Sheets configuration
          </Button>
        )}
        <Button variant="outline" size="sm" render={<Link href="/connectors" />}>
          <ArrowLeftIcon className="size-4" /> Back to connectors
        </Button>
      </PageHeader>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : isError || !connector ? (
        <div className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          {error instanceof ApiError && error.status === 404
            ? "This connector doesn't exist, or belongs to a different organization."
            : error instanceof ApiError && error.status === 403
              ? "You don't have permission to view connectors in this organization."
              : "Failed to load this connector."}
        </div>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-4 py-3">
            <span className="font-medium">{connector.name}</span>
            <span className="font-mono text-xs text-muted-foreground">{typeLabel(connector.type)}</span>
            <CopyableId value={connector.id} label="connector ID" />
          </div>

          <ConnectorSpan connector={connector} mcpKeys={mcpKeys ?? []} recentLogs={connectorLogs} />

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">Endpoint</h2>
            {resolvedGatewayUrl ? (
              <CopyableCode label="the MCP endpoint" value={`POST ${resolvedGatewayUrl}/mcp/${connectorId}`} />
            ) : (
              <p className="text-sm text-muted-foreground">Resolving endpoint…</p>
            )}
            {!gatewayUrl && (
              <Callout variant="note" title="Endpoint host is a guess">
                <span className="font-mono">GATEWAY_URL</span> isn&apos;t set for this deployment, so the host
                above is guessed from the page you&apos;re viewing right now — it should be your Sapanjai{" "}
                <span className="font-medium text-foreground">API</span> host, not this dashboard&apos;s. Set{" "}
                <span className="font-mono">GATEWAY_URL</span> to make it exact.
              </Callout>
            )}
          </section>

          <section className="space-y-2 border-t pt-4">
            <h2 className="font-heading text-base font-medium">Wiring snippet</h2>
            <p className="text-sm text-muted-foreground">
              Replace <span className="font-mono">&lt;paste your key&gt;</span> with a raw key from{" "}
              <Link href="/mcp-keys" className="text-foreground underline underline-offset-4 hover:text-signal">
                MCP keys
              </Link>{" "}
              — the token is shown once, at creation, and can&apos;t be retrieved again afterward.
            </p>
            <CopyableCode
              label="the client wiring command"
              value={
                resolvedGatewayUrl
                  ? `claude mcp add sapanjai --scope local --transport http \\
  ${resolvedGatewayUrl}/mcp/${connectorId} \\
  --header "Authorization: Bearer <paste your key>"`
                  : "Resolving endpoint…"
              }
            />
            <p className="text-sm text-muted-foreground">
              The URL has to come before <span className="font-mono">--header</span> — that flag is variadic and
              will otherwise swallow the address. Calling the endpoint directly is a{" "}
              <span className="font-mono">POST</span> to the same URL with that{" "}
              <span className="font-mono">Authorization</span> header.
            </p>
          </section>

          {connector.type === "google_sheets" && (
            <Callout variant="boundary" title="Missing a tool? It was filtered, not broken">
              An agent only ever sees the tools this key is permitted to call — anything else is absent from its
              tool list entirely, rather than failing when called. The one that catches people out:{" "}
              <span className="font-mono">drive:read</span> is a separate permission, and{" "}
              <span className="font-mono">sheets:read</span> never implies it. A key whose holder has only{" "}
              <span className="font-mono">sheets:read</span> will not show{" "}
              <span className="font-mono">drive_list_folder</span> or{" "}
              <span className="font-mono">drive_get_file</span> at all. Grants are re-read on every call, so
              fixing the role takes effect on the agent&apos;s next request — no need to reconnect it.
            </Callout>
          )}

          <section className="space-y-3 border-t pt-4">
            <h2 className="font-heading text-base font-medium">Health</h2>
            <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-4 py-3">
              <StatusBadge status={connector.status} />
              <span className="text-xs text-muted-foreground">
                Last health check: {formatDate(connector.lastHealthCheckAt)}
              </span>
              <Button
                variant="outline"
                size="sm"
                disabled={healthCheckMutation.isPending}
                onClick={() => healthCheckMutation.mutate()}
              >
                {healthCheckMutation.isPending ? "Checking…" : "Run health check"}
              </Button>
            </div>
            {healthCheckResult && (
              <p className={cn("text-sm", healthCheckResult.ok ? "text-wire" : "text-muted-foreground")}>
                {healthCheckResult.message}
              </p>
            )}
          </section>

          <section className="space-y-3 border-t pt-4">
            <h2 className="font-heading text-base font-medium">Recent activity</h2>
            <DataTable columns={["When", "Action", { label: "Detail", className: "w-full" }]}>
              <TableBody>
                {connectorLogs.length ? (
                  connectorLogs.map((log) => {
                    const at = new Date(log.createdAt);
                    return (
                      <TableRow key={log.id}>
                        <TableCell className="font-mono text-xs whitespace-nowrap text-muted-foreground">
                          {at.toISOString().slice(0, 10)}
                          <span className="ml-2 text-foreground">{at.toTimeString().slice(0, 8)}</span>
                        </TableCell>
                        <TableCell>
                          <ActionToken action={log.action} />
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          <ActivityDetail action={log.action} metadata={log.metadata ?? {}} />
                        </TableCell>
                      </TableRow>
                    );
                  })
                ) : (
                  <TableMessage colSpan={3}>No gateway activity recorded for this connector yet.</TableMessage>
                )}
              </TableBody>
            </DataTable>
          </section>
        </>
      )}
    </div>
  );
}
