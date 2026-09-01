"use client";

import { useQuery } from "@tanstack/react-query";

import { StatTile } from "@/components/admin/stat-tile";
import { ActionToken, ActivityDetail } from "@/components/activity-row";
import { DataTable } from "@/components/data-table";
import { PageHeader, Panel, TableMessage } from "@/components/page-header";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import { getAdminSystemStats, queryAdminAuditLogs } from "@/lib/api/endpoints";

const RECENT_ACTIVITY_LIMIT = 15;

export default function AdminOverviewPage() {
  const {
    data: stats,
    isLoading: statsLoading,
    isError: statsError,
    error: statsErrorObj,
  } = useQuery({
    queryKey: ["admin", "system", "stats"],
    queryFn: getAdminSystemStats,
    retry: false,
  });

  // "admin.*" is a prefix match (a trailing '*' on any ?action= entry —
  // execution plan Task 2.7), so this is every admin-console mutation
  // across every tenant, not this org's own activity.
  const { data: activity, isLoading: activityLoading } = useQuery({
    queryKey: ["admin", "audit-logs", "recent"],
    queryFn: () => queryAdminAuditLogs({ action: "admin.*", limit: RECENT_ACTIVITY_LIMIT }),
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="overview" description="Platform-wide counts and what staff have done recently." />

      {statsError ? (
        <Panel className="px-4 py-6 text-center text-sm text-muted-foreground">
          {statsErrorObj instanceof ApiError
            ? statsErrorObj.message
            : "Failed to load system stats."}
        </Panel>
      ) : (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
          <StatTile label="organizations" value={statsLoading ? "—" : (stats?.organizations ?? 0)} />
          <StatTile label="users" value={statsLoading ? "—" : (stats?.users ?? 0)} />
          <StatTile label="connectors" value={statsLoading ? "—" : (stats?.connectors ?? 0)} />
          <StatTile
            label="mcp keys"
            value={statsLoading ? "—" : (stats?.mcpKeysActive ?? 0)}
            hint={statsLoading ? undefined : `${stats?.mcpKeysTotal ?? 0} total`}
          />
          <StatTile label="active sessions" value={statsLoading ? "—" : (stats?.sessionsActive ?? 0)} />
        </div>
      )}

      <div className="grid gap-4 lg:grid-cols-2">
        <section className="space-y-2">
          <h2 className="font-heading text-base font-medium">Plan breakdown</h2>
          <Panel>
            <ul className="divide-y">
              {statsLoading ? (
                <li className="px-4 py-3 text-sm text-muted-foreground">Loading…</li>
              ) : stats?.planBreakdown.length ? (
                stats.planBreakdown.map((entry) => (
                  <li key={entry.planName} className="flex items-center justify-between px-4 py-2.5 text-sm">
                    <span className="font-mono">{entry.planName}</span>
                    <span className="text-muted-foreground">{entry.orgCount} orgs</span>
                  </li>
                ))
              ) : (
                <li className="px-4 py-3 text-sm text-muted-foreground">No subscribed organizations yet.</li>
              )}
            </ul>
          </Panel>
        </section>

        <section className="space-y-2">
          <h2 className="font-heading text-base font-medium">Email outbox health</h2>
          <Panel className="grid grid-cols-3 divide-x">
            <div className="flex flex-col gap-0.5 px-4 py-3">
              <span className="label-eyebrow text-muted-foreground">pending</span>
              <span className="font-mono text-lg">{statsLoading ? "—" : (stats?.emailOutbox.pending ?? 0)}</span>
            </div>
            <div className="flex flex-col gap-0.5 px-4 py-3">
              <span className="label-eyebrow text-muted-foreground">sent</span>
              <span className="font-mono text-lg text-wire">{statsLoading ? "—" : (stats?.emailOutbox.sent ?? 0)}</span>
            </div>
            <div className="flex flex-col gap-0.5 px-4 py-3">
              <span className="label-eyebrow text-muted-foreground">failed</span>
              {/* A rising count here is the single best early warning that
                  Resend or EMAIL_FROM is misconfigured (CLAUDE.md's
                  Background worker bullet) — worth the one spot of alarm
                  color on an otherwise neutral tile grid. */}
              <span
                className={
                  !statsLoading && (stats?.emailOutbox.failed ?? 0) > 0
                    ? "font-mono text-lg text-destructive"
                    : "font-mono text-lg"
                }
              >
                {statsLoading ? "—" : (stats?.emailOutbox.failed ?? 0)}
              </span>
            </div>
          </Panel>
        </section>
      </div>

      <section className="space-y-2">
        <h2 className="font-heading text-base font-medium">Recent admin activity</h2>
        <DataTable columns={["When", "Action", "Actor", { label: "Detail", className: "w-full" }]}>
          <TableBody>
            {activityLoading ? (
              <TableMessage colSpan={4}>Loading…</TableMessage>
            ) : activity?.items.length ? (
              activity.items.map((log) => {
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
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {log.userEmail ?? "—"}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      <ActivityDetail action={log.action} metadata={log.metadata ?? {}} />
                    </TableCell>
                  </TableRow>
                );
              })
            ) : (
              <TableMessage colSpan={4}>No admin actions recorded yet.</TableMessage>
            )}
          </TableBody>
        </DataTable>
      </section>
    </div>
  );
}
