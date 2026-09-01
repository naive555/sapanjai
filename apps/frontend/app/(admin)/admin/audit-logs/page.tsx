"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { Pager } from "@/components/admin/pager";
import { ActionToken, ActivityDetail } from "@/components/activity-row";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { Input } from "@/components/ui/input";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import { queryAdminAuditLogs } from "@/lib/api/endpoints";

const PAGE_SIZE = 50;

function Filter({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="label-eyebrow">{label}</span>
      {children}
    </div>
  );
}

export default function AdminAuditLogsPage() {
  const [organizationId, setOrganizationId] = useState("");
  const [userId, setUserId] = useState("");
  const [action, setAction] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [offset, setOffset] = useState(0);

  // action accepts a comma-separated list here (each entry may end in '*'
  // for a prefix match, e.g. "admin.*") — split client-side into the
  // repeatable ?action= the endpoint expects (queryAdminAuditLogs/
  // client.ts's buildPath).
  const actions = action
    .split(",")
    .map((a) => a.trim())
    .filter(Boolean);

  const filters = {
    organizationId: organizationId.trim() || undefined,
    userId: userId.trim() || undefined,
    action: actions.length ? actions : undefined,
    from: from ? new Date(from).toISOString() : undefined,
    to: to ? new Date(to).toISOString() : undefined,
    limit: PAGE_SIZE,
    offset,
  };

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["admin", "audit-logs", filters],
    queryFn: () => queryAdminAuditLogs(filters),
  });

  function resetOffset<T>(setter: (v: T) => void) {
    return (v: T) => {
      setter(v);
      setOffset(0);
    };
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="audit logs" description="Every recorded action across every organization." />

      <div className="flex flex-wrap items-end gap-4">
        <Filter label="Organization ID">
          <Input
            className="w-56 font-mono text-xs"
            placeholder="uuid"
            value={organizationId}
            onChange={(e) => resetOffset(setOrganizationId)(e.target.value)}
          />
        </Filter>
        <Filter label="User ID">
          <Input
            className="w-56 font-mono text-xs"
            placeholder="uuid"
            value={userId}
            onChange={(e) => resetOffset(setUserId)(e.target.value)}
          />
        </Filter>
        <Filter label="Action">
          <Input
            className="w-56 font-mono text-xs"
            placeholder="admin.*, mcp.tool.called"
            value={action}
            onChange={(e) => resetOffset(setAction)(e.target.value)}
          />
        </Filter>
        <Filter label="From">
          <Input
            type="datetime-local"
            className="font-mono text-xs"
            value={from}
            onChange={(e) => resetOffset(setFrom)(e.target.value)}
          />
        </Filter>
        <Filter label="To">
          <Input
            type="datetime-local"
            className="font-mono text-xs"
            value={to}
            onChange={(e) => resetOffset(setTo)(e.target.value)}
          />
        </Filter>
      </div>

      <DataTable columns={["When", "Action", "Org", "User", { label: "Detail", className: "w-full" }]}>
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={5}>Loading…</TableMessage>
          ) : isError ? (
            <TableMessage colSpan={5}>
              {error instanceof ApiError ? error.message : "Failed to load audit logs."}
            </TableMessage>
          ) : data?.items.length ? (
            data.items.map((log) => {
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
                  <TableCell className="text-xs text-muted-foreground">{log.organizationName ?? "—"}</TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">{log.userEmail ?? "—"}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    <ActivityDetail action={log.action} metadata={log.metadata ?? {}} />
                  </TableCell>
                </TableRow>
              );
            })
          ) : (
            <TableMessage colSpan={5}>No entries match these filters.</TableMessage>
          )}
        </TableBody>
      </DataTable>

      {data && data.total > 0 && (
        <Pager offset={offset} limit={PAGE_SIZE} total={data.total} onOffsetChange={setOffset} />
      )}
    </div>
  );
}
