"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { getAuditLogs, listMembers } from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";

// Recorded actions per docs/02-api-contract.md — role.created/role.assigned
// are defined in the contract but not currently written by the backend;
// kept in the filter list anyway since the contract documents them as valid
// `action` query values.
const KNOWN_ACTIONS = [
  "user.login",
  "user.register",
  "org.created",
  "org.member.invited",
  "org.member.removed",
  "role.created",
  "role.assigned",
];

const ALL_ACTIONS = "__all_actions__";
const ALL_USERS = "__all_users__";

// Audit actions are dotted paths — the namespace is context, the last segment
// is what happened. Splitting them lets the eye scan the verbs down the
// column instead of re-reading a shared prefix on every row.
function ActionToken({ action }: { action: string }) {
  const split = action.lastIndexOf(".");
  if (split === -1) return <span className="font-mono text-[0.8125rem]">{action}</span>;

  return (
    <span className="font-mono text-[0.8125rem] whitespace-nowrap">
      <span className="text-muted-foreground">{action.slice(0, split + 1)}</span>
      <span className="text-foreground">{action.slice(split + 1)}</span>
    </span>
  );
}

function Filter({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="label-eyebrow">{label}</span>
      {children}
    </div>
  );
}

export default function AuditPage() {
  const activeOrgId = useActiveOrgId();
  const [action, setAction] = useState(ALL_ACTIONS);
  const [userId, setUserId] = useState(ALL_USERS);
  const [limit, setLimit] = useState(50);

  const { data: members } = useQuery({
    queryKey: ["members", activeOrgId],
    queryFn: listMembers,
    enabled: activeOrgId !== null,
  });

  const filters = {
    action: action === ALL_ACTIONS ? undefined : action,
    userId: userId === ALL_USERS ? undefined : userId,
    limit,
  };

  const { data: logs, isLoading } = useQuery({
    queryKey: ["audit", activeOrgId, filters],
    queryFn: () => getAuditLogs(filters),
    enabled: activeOrgId !== null,
  });

  function memberLabel(id: string | null): string {
    if (!id) return "—";
    return members?.find((m) => m.userId === id)?.email ?? id;
  }

  const isFiltered = action !== ALL_ACTIONS || userId !== ALL_USERS;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="activity" description="Every call an agent made, and everything it was refused." />

      <div className="flex flex-wrap items-end gap-4">
        <Filter label="Action">
          <Select value={action} onValueChange={(value) => setAction(value ?? ALL_ACTIONS)}>
            <SelectTrigger className="w-56 font-mono text-xs">
              <SelectValue>{(value: string) => (value === ALL_ACTIONS ? "All actions" : value)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL_ACTIONS}>All actions</SelectItem>
              {KNOWN_ACTIONS.map((a) => (
                <SelectItem key={a} value={a} className="font-mono text-xs">
                  {a}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Filter>

        <Filter label="Actor">
          <Select value={userId} onValueChange={(value) => setUserId(value ?? ALL_USERS)}>
            <SelectTrigger className="w-56 font-mono text-xs">
              <SelectValue>
                {(value: string) => (value === ALL_USERS ? "All actors" : memberLabel(value))}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL_USERS}>All actors</SelectItem>
              {members?.map((m) => (
                <SelectItem key={m.userId} value={m.userId} className="font-mono text-xs">
                  {m.email}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Filter>

        <Filter label="Rows">
          <Input
            id="audit-limit"
            type="number"
            min={1}
            max={100}
            className="w-20 font-mono"
            aria-label="Maximum rows to show"
            value={limit}
            onChange={(e) => {
              const next = Number(e.target.value);
              if (Number.isFinite(next)) {
                setLimit(Math.min(100, Math.max(1, next)));
              }
            }}
          />
        </Filter>
      </div>

      <DataTable columns={["When", "Action", "Actor", "Detail"]}>
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={4}>Loading…</TableMessage>
          ) : logs?.length ? (
            logs.map((log, index) => {
              const at = new Date(log.createdAt);
              const day = at.toISOString().slice(0, 10);
              // A log is read top-down, so repeat the date only when it
              // changes — the eye tracks the time column, not the date.
              const previousDay =
                index > 0 ? new Date(logs[index - 1].createdAt).toISOString().slice(0, 10) : null;
              const showDay = day !== previousDay;
              const entries = Object.entries(log.metadata ?? {});

              return (
                <TableRow key={log.id}>
                  <TableCell className="font-mono text-xs whitespace-nowrap">
                    <span className={showDay ? "text-muted-foreground" : "invisible"}>{day}</span>
                    <span className="ml-2 text-foreground">
                      {at.toTimeString().slice(0, 8)}
                    </span>
                  </TableCell>
                  <TableCell>
                    <ActionToken action={log.action} />
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {memberLabel(log.userId)}
                  </TableCell>
                  <TableCell className="max-w-xs truncate font-mono text-xs text-muted-foreground">
                    {entries.length
                      ? entries.map(([key, value]) => (
                          <span key={key} className="mr-3">
                            <span className="text-muted-foreground/60">{key}=</span>
                            {String(value)}
                          </span>
                        ))
                      : "—"}
                  </TableCell>
                </TableRow>
              );
            })
          ) : (
            <TableMessage colSpan={4}>
              {isFiltered
                ? "No entries match these filters. Widen them to see more."
                : "Nothing recorded yet. Actions taken in this organization will appear here."}
            </TableMessage>
          )}
        </TableBody>
      </DataTable>
    </div>
  );
}
