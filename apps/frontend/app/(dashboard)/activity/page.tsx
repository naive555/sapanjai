"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { ActionToken, ActivityDetail } from "@/components/activity-row";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { getAuditLogs, listMembers } from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";
import { cn } from "@/lib/utils";

// Recorded actions, grouped by namespace. Mirrors the "Recorded actions"
// line in docs/02-api-contract.md's Audit logs section (now including the
// five mcp.* actions and three connector.* actions, folded back into that
// summary line as part of Phase 4) — kept in sync with the backend's
// action set by hand, since there's no shared source of truth to codegen
// this list from.
const ACTION_GROUPS: { label: string; actions: string[] }[] = [
  { label: "user", actions: ["user.login", "user.register"] },
  { label: "org", actions: ["org.created", "org.member.invited", "org.member.removed"] },
  { label: "role", actions: ["role.created", "role.assigned"] },
  { label: "connector", actions: ["connector.created", "connector.updated", "connector.deleted"] },
  {
    label: "mcp",
    actions: [
      "mcp.session.started",
      "mcp.tool.called",
      "mcp.tool.denied",
      "mcp.ratelimit.hit",
      "mcp.file.downloaded",
    ],
  },
];

const MCP_ACTIONS = ACTION_GROUPS.find((g) => g.label === "mcp")!.actions;

const ALL_ACTIONS = "__all_actions__";
const ALL_USERS = "__all_users__";

function Filter({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="label-eyebrow">{label}</span>
      {children}
    </div>
  );
}

export default function ActivityPage() {
  const activeOrgId = useActiveOrgId();
  const [action, setAction] = useState(ALL_ACTIONS);
  const [userId, setUserId] = useState(ALL_USERS);
  const [limit, setLimit] = useState(50);
  const [gatewayOnly, setGatewayOnly] = useState(false);

  const { data: members } = useQuery({
    queryKey: ["members", activeOrgId],
    queryFn: listMembers,
    enabled: activeOrgId !== null,
  });

  // "Gateway only" now requests the five mcp.* actions directly — one call,
  // filtered server-side via GET /audit-logs' repeatable `action` param
  // (Phase 4). Overriding rather than ANDing with the Action select: a
  // specific action outside the mcp.* namespace combined with "gateway
  // only" would otherwise silently render zero rows with no indication why.
  // Disabling the select while the toggle is on makes the override visible
  // instead of surprising, and its own selection is preserved so turning
  // the toggle back off restores it.
  const filters = {
    action: gatewayOnly ? MCP_ACTIONS : action === ALL_ACTIONS ? undefined : action,
    userId: userId === ALL_USERS ? undefined : userId,
    limit,
  };

  const { data: rows, isLoading } = useQuery({
    queryKey: ["audit", activeOrgId, filters],
    queryFn: () => getAuditLogs(filters),
    enabled: activeOrgId !== null,
  });

  function memberLabel(id: string | null): string {
    if (!id) return "—";
    return members?.find((m) => m.userId === id)?.email ?? id;
  }

  const isFiltered = gatewayOnly || action !== ALL_ACTIONS || userId !== ALL_USERS;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="activity" description="Every call an agent made, and everything it was refused." />

      <div className="flex flex-wrap items-end gap-4">
        <Filter label="Gateway">
          <Button
            type="button"
            variant="outline"
            size="sm"
            aria-pressed={gatewayOnly}
            className={cn(
              "font-mono text-xs",
              gatewayOnly && "border-wire/40 bg-wire/10 text-wire hover:bg-wire/15",
            )}
            onClick={() => setGatewayOnly((v) => !v)}
          >
            Gateway only
          </Button>
        </Filter>

        <Filter label="Action">
          <Select
            value={action}
            onValueChange={(value) => setAction(value ?? ALL_ACTIONS)}
            disabled={gatewayOnly}
          >
            <SelectTrigger className="w-56 font-mono text-xs">
              <SelectValue>{(value: string) => (value === ALL_ACTIONS ? "All actions" : value)}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL_ACTIONS}>All actions</SelectItem>
              {ACTION_GROUPS.map((group) => (
                <SelectGroup key={group.label}>
                  <SelectSeparator />
                  <SelectLabel>{group.label}</SelectLabel>
                  {group.actions.map((a) => (
                    <SelectItem key={a} value={a} className="font-mono text-xs">
                      {a}
                    </SelectItem>
                  ))}
                </SelectGroup>
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

      <DataTable columns={["When", "Action", "Actor", { label: "Detail", className: "w-full" }]}>
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={4}>Loading…</TableMessage>
          ) : rows?.length ? (
            rows.map((log, index) => {
              const at = new Date(log.createdAt);
              const day = at.toISOString().slice(0, 10);
              // A log is read top-down, so repeat the date only when it
              // changes — the eye tracks the time column, not the date.
              const previousDay =
                index > 0 ? new Date(rows[index - 1].createdAt).toISOString().slice(0, 10) : null;
              const showDay = day !== previousDay;

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
                  <TableCell className="text-xs text-muted-foreground">
                    <ActivityDetail action={log.action} metadata={log.metadata ?? {}} />
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
