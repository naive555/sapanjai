"use client";

import { useState } from "react";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";

import { ActionToken, ActivityDetail } from "@/components/activity-row";
import { ConnectorSpan } from "@/components/connector-span";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import {
  getAuditLogs,
  listConnectors,
  listMcpKeys,
  listRoles,
  type AuditLogResponse,
  type RoleResponse,
} from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";
import { cn } from "@/lib/utils";

// The contract caps `limit` at 100 (docs/02-api-contract.md) — the same
// ceiling the connector detail page's own recent-activity fetch runs into.
// Paired with `since`, one call over exactly this window is what "last 24
// hours" is built from below; there is deliberately no second call to fill
// in what the cap cuts off (plan §3 Phase 5: "one call, stated honestly").
const SINCE_WINDOW_MS = 24 * 60 * 60 * 1000;
const AUDIT_FETCH_LIMIT = 100;
const REFUSED_ROWS = 5;

function str(metadata: Record<string, unknown> | null | undefined, key: string): string | undefined {
  const value = metadata?.[key];
  return typeof value === "string" ? value : undefined;
}

function num(metadata: Record<string, unknown> | null | undefined, key: string): number | undefined {
  const value = metadata?.[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function connectorIdOf(log: AuditLogResponse): string | undefined {
  return str(log.metadata, "connector_id");
}

function formatTimestamp(value: string): string {
  const d = new Date(value);
  // Same UTC-date / local-time split connector-span.tsx and activity/page.tsx
  // already use for a row timestamp — matched for consistency, not because
  // the split itself is ideal.
  return `${d.toISOString().slice(0, 10)} ${d.toTimeString().slice(0, 5)}`;
}

// The backend's RequirePermission precedence, restated client-side (docs/02;
// see also PermissionToken's own doc comment): `*` beats an exact
// `resource:verb` match, which beats a `resource:*` wildcard on the same
// resource. There is no endpoint that answers "does this permission cover
// that action", so this is a client-side re-derivation of a rule the backend
// already enforces — not a new rule of its own.
function permissionCovers(granted: string, required: string): boolean {
  if (granted === "*" || granted === required) return true;
  const sep = granted.indexOf(":");
  if (sep === -1 || granted.slice(sep + 1) !== "*") return false;
  const resource = granted.slice(0, sep);
  const requiredSep = required.indexOf(":");
  return requiredSep !== -1 && required.slice(0, requiredSep) === resource;
}

function rolesGranting(action: string, roles: RoleResponse[]): RoleResponse[] {
  return roles.filter((role) => role.permissions.some((p) => permissionCovers(p.action, action)));
}

/**
 * The post-login landing page (app/page.tsx, app/(auth)/layout.tsx, and the
 * sidebar wordmark all point here — see Phase 5 of the gateway-console
 * plan). Its job is the one question every other screen in the app makes
 * you go hunting for: is the door open, what walked through it today, and
 * what got turned away.
 */
export default function OverviewPage() {
  const activeOrgId = useActiveOrgId();

  const { data: connectors, isLoading: connectorsLoading } = useQuery({
    queryKey: ["connectors", activeOrgId],
    queryFn: listConnectors,
    enabled: activeOrgId !== null,
  });

  // Org-wide, not scoped to any one connector — the same list ConnectorSpan
  // already reads on the connector detail page, fetched once here and
  // shared across every stacked Span rather than refetched per connector.
  const { data: mcpKeys } = useQuery({
    queryKey: ["mcp-keys", activeOrgId],
    queryFn: listMcpKeys,
    enabled: activeOrgId !== null,
  });

  // Only used to resolve "which role would fix this denial" below — never
  // rendered as its own roles table here, that's what /roles is for.
  const { data: roles } = useQuery({
    queryKey: ["roles", activeOrgId],
    queryFn: listRoles,
    enabled: activeOrgId !== null,
  });

  // Computed once, on mount — not read fresh on every render, because a
  // `since` boundary that moved on every re-render would change the query
  // key and refetch in a loop for no reason. Calling Date.now() inside a
  // useState initializer (rather than at the top of the component body) is
  // the same purity workaround connector-span.tsx's isUsableKey comment
  // explains: the initializer runs exactly once.
  const [since] = useState(() => new Date(Date.now() - SINCE_WINDOW_MS).toISOString());

  const { data: recentLogs } = useQuery({
    queryKey: ["audit", activeOrgId, "overview", since],
    queryFn: () => getAuditLogs({ since, limit: AUDIT_FETCH_LIMIT }),
    enabled: activeOrgId !== null,
  });

  const rows = recentLogs ?? [];

  // ---- last 24 hours ----
  // mcp.session.started is deliberately absent from every count below: it
  // fires on every agent reconnect and would dominate a "calls" figure that
  // is supposed to mean tool traffic. This is the one place that action is
  // hidden — /activity keeps showing it by default, per the owner's call.
  const calledLogs = rows.filter((log) => log.action === "mcp.tool.called");
  const deniedLogs = rows.filter((log) => log.action === "mcp.tool.denied");
  const rateLimitLogs = rows.filter((log) => log.action === "mcp.ratelimit.hit");

  const toolCounts = new Map<string, number>();
  for (const log of calledLogs) {
    const tool = str(log.metadata, "tool");
    if (!tool) continue;
    toolCounts.set(tool, (toolCounts.get(tool) ?? 0) + 1);
  }
  let busiest: { tool: string; count: number } | null = null;
  for (const [tool, count] of toolCounts) {
    if (!busiest || count > busiest.count) busiest = { tool, count };
  }

  let slowest: { tool: string; durationMs: number } | null = null;
  for (const log of calledLogs) {
    const tool = str(log.metadata, "tool");
    const durationMs = num(log.metadata, "duration_ms");
    if (!tool || durationMs === undefined) continue;
    if (!slowest || durationMs > slowest.durationMs) slowest = { tool, durationMs };
  }

  // The contract caps `limit` at 100 — a response that comes back exactly at
  // that cap means the 24-hour window could hold more rows than this one
  // call can see, so every count above becomes a floor rather than a total.
  // Said here rather than papered over with a second request.
  const atCap = rows.length >= AUDIT_FETCH_LIMIT;

  const stats: { label: string; value: string; caption?: string; tone?: "wire" | "signal" | "destructive" }[] = [
    { label: "calls", value: String(calledLogs.length), tone: "wire" },
    { label: "denied", value: String(deniedLogs.length), tone: deniedLogs.length ? "signal" : undefined },
    {
      label: "rate-limited",
      value: String(rateLimitLogs.length),
      tone: rateLimitLogs.length ? "destructive" : undefined,
    },
    {
      label: "busiest tool",
      value: busiest ? busiest.tool : "—",
      caption: busiest ? `${busiest.count} call${busiest.count === 1 ? "" : "s"}` : undefined,
    },
    {
      label: "slowest tool",
      value: slowest ? slowest.tool : "—",
      caption: slowest ? `${slowest.durationMs}ms` : undefined,
    },
  ];

  // ---- refused recently ----
  // Newest first is the API's own order (docs/02-api-contract.md), so the
  // first few denials in the window already fetched above are the most
  // recent ones — no separate request or client-side sort needed.
  const refusals = deniedLogs.slice(0, REFUSED_ROWS);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="overview"
        description="What's connected, what an agent did with it in the last day, and what it was refused."
      />

      {/*
       * Every other org-scoped page is unreachable without an active org —
       * the nav disables those links and explains why. This one is the
       * landing, so it is the single org route a user can arrive at with
       * nothing selected (a fresh browser has no stored selection, even for
       * someone who belongs to several orgs). Falling through to the
       * connector onboarding Span here would name the wrong next step: the
       * door can't be opened before there's a building to put it in.
       */}
      {activeOrgId === null ? (
        <div className="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
          No organization selected.{" "}
          <Link href="/organizations" className="text-foreground underline underline-offset-4 hover:text-signal">
            Pick one, or create your first
          </Link>{" "}
          — everything below is scoped to it.
        </div>
      ) : (
        <>
          <div className="flex flex-col gap-3">
            {connectorsLoading ? (
              <p className="text-sm text-muted-foreground">Loading…</p>
            ) : connectors && connectors.length > 0 ? (
              // One connector renders full-width and alone; two or more stack —
              // both cases are just this map, since the Span is already
              // full-width on its own (owner decision: skip a separate
              // single-connector layout).
              connectors.map((connector) => (
                <ConnectorSpan
                  key={connector.id}
                  connector={connector}
                  mcpKeys={mcpKeys ?? []}
                  recentLogs={rows.filter(
                    (log) => log.action.startsWith("mcp.") && connectorIdOf(log) === connector.id,
                  )}
                />
              ))
            ) : (
              // Zero connectors: the same component, rendered fully dashed, as
              // the onboarding path — not a separate empty-state design.
              <ConnectorSpan connector={null} mcpKeys={mcpKeys ?? []} recentLogs={[]} />
            )}
          </div>

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">Last 24 hours</h2>
            {/* A hairline-ruled row in the data face, not a row of gradient stat
                cards — the thing plan §2/§4 both name to avoid. gap-px over a
                bg-border container is the same hairline the rest of the app
                already draws with border/border-dashed, just applied as a grid
                seam instead of an edge. */}
            <div className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border bg-border sm:grid-cols-5">
              {stats.map((stat) => (
                <div key={stat.label} className="space-y-1 bg-card px-4 py-3">
                  <div className="label-eyebrow">{stat.label}</div>
                  <div
                    title={stat.value}
                    className={cn(
                      "truncate font-mono text-lg",
                      stat.tone === "wire" && "text-wire",
                      stat.tone === "signal" && "text-signal",
                      stat.tone === "destructive" && "text-destructive",
                    )}
                  >
                    {stat.value}
                  </div>
                  {stat.caption && <div className="truncate text-xs text-muted-foreground">{stat.caption}</div>}
                </div>
              ))}
            </div>
            {atCap && (
              <p className="text-xs text-muted-foreground">
                {AUDIT_FETCH_LIMIT} rows is the most one <span className="font-mono">/audit-logs</span> call can
                return, and this window hit that cap — the counts above are a lower bound, not a total.
              </p>
            )}
          </section>

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">Refused recently</h2>
            <DataTable columns={["When", "Action", { label: "Detail", className: "w-full" }, "Fix"]}>
              <TableBody>
                {refusals.length ? (
                  refusals.map((log) => {
                    const missingPermission = str(log.metadata, "missing_permission");
                    const granting = missingPermission ? rolesGranting(missingPermission, roles ?? []) : [];

                    return (
                      <TableRow key={log.id}>
                        <TableCell className="font-mono text-xs whitespace-nowrap text-muted-foreground">
                          {formatTimestamp(log.createdAt)}
                        </TableCell>
                        <TableCell>
                          <ActionToken action={log.action} />
                        </TableCell>
                        <TableCell className="text-xs text-muted-foreground">
                          <ActivityDetail action={log.action} metadata={log.metadata ?? {}} />
                        </TableCell>
                        <TableCell className="text-xs whitespace-nowrap">
                          {!missingPermission ? (
                            <span className="text-muted-foreground">—</span>
                          ) : granting.length ? (
                            <span className="text-muted-foreground">
                              granted by{" "}
                              <span className="text-foreground">{granting.map((r) => r.name).join(", ")}</span>
                            </span>
                          ) : (
                            <span className="text-signal">
                              no role grants <span className="font-mono">{missingPermission}</span>
                            </span>
                          )}{" "}
                          <Link href="/roles" className="underline underline-offset-4 hover:text-signal">
                            Roles
                          </Link>
                        </TableCell>
                      </TableRow>
                    );
                  })
                ) : (
                  <TableMessage colSpan={4}>Nothing refused in the last 24 hours.</TableMessage>
                )}
              </TableBody>
            </DataTable>
          </section>
        </>
      )}
    </div>
  );
}
