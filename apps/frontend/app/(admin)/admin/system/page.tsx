"use client";

import { useQuery } from "@tanstack/react-query";

import { StatTile } from "@/components/admin/stat-tile";
import { PageHeader, Panel } from "@/components/page-header";
import { ApiError } from "@/lib/api/client";
import { getAdminSystemStats } from "@/lib/api/endpoints";

// 30s per the execution plan — this page is meant to sit open on a second
// monitor during an incident, not be manually refreshed.
const REFETCH_INTERVAL_MS = 30_000;

export default function AdminSystemPage() {
  const { data: stats, isLoading, isError, error, dataUpdatedAt } = useQuery({
    queryKey: ["admin", "system", "stats"],
    queryFn: getAdminSystemStats,
    refetchInterval: REFETCH_INTERVAL_MS,
    retry: false,
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="system"
        description={
          dataUpdatedAt
            ? `Refreshes every 30s — last updated ${new Date(dataUpdatedAt).toTimeString().slice(0, 8)}.`
            : "Refreshes every 30s."
        }
      />

      {isError ? (
        <Panel className="px-4 py-6 text-center text-sm text-muted-foreground">
          {error instanceof ApiError ? error.message : "Failed to load system stats."}
        </Panel>
      ) : (
        <>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
            <StatTile label="organizations" value={isLoading ? "—" : (stats?.organizations ?? 0)} hint={isLoading ? undefined : `+${stats?.organizationsLast7d ?? 0} in 7d`} />
            <StatTile label="users" value={isLoading ? "—" : (stats?.users ?? 0)} hint={isLoading ? undefined : `+${stats?.usersLast7d ?? 0} in 7d`} />
            <StatTile label="connectors" value={isLoading ? "—" : (stats?.connectors ?? 0)} />
            <StatTile label="active sessions" value={isLoading ? "—" : (stats?.sessionsActive ?? 0)} />
            <StatTile
              label="mcp keys"
              value={isLoading ? "—" : (stats?.mcpKeysActive ?? 0)}
              hint={isLoading ? undefined : `${stats?.mcpKeysTotal ?? 0} total`}
            />
            <StatTile label="audit logs" value={isLoading ? "—" : (stats?.auditLogs ?? 0)} />
            <StatTile
              label="redis memory"
              value={isLoading ? "—" : (stats?.redisUsedMemoryHuman ?? "—")}
            />
          </div>

          <div className="grid gap-4 lg:grid-cols-2">
            <section className="space-y-2">
              <h2 className="font-heading text-base font-medium">Plan breakdown</h2>
              <Panel>
                <ul className="divide-y">
                  {isLoading ? (
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
              <h2 className="font-heading text-base font-medium">Email outbox</h2>
              <Panel className="grid grid-cols-3 divide-x">
                <div className="flex flex-col gap-0.5 px-4 py-3">
                  <span className="label-eyebrow text-muted-foreground">pending</span>
                  <span className="font-mono text-lg">{isLoading ? "—" : (stats?.emailOutbox.pending ?? 0)}</span>
                </div>
                <div className="flex flex-col gap-0.5 px-4 py-3">
                  <span className="label-eyebrow text-muted-foreground">sent</span>
                  <span className="font-mono text-lg text-wire">{isLoading ? "—" : (stats?.emailOutbox.sent ?? 0)}</span>
                </div>
                <div className="flex flex-col gap-0.5 px-4 py-3">
                  <span className="label-eyebrow text-muted-foreground">failed</span>
                  <span
                    className={
                      !isLoading && (stats?.emailOutbox.failed ?? 0) > 0
                        ? "font-mono text-lg text-destructive"
                        : "font-mono text-lg"
                    }
                  >
                    {isLoading ? "—" : (stats?.emailOutbox.failed ?? 0)}
                  </span>
                </div>
              </Panel>
            </section>
          </div>
        </>
      )}
    </div>
  );
}
