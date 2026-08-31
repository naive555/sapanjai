"use client";

import { useState } from "react";
import Link from "next/link";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon } from "lucide-react";
import { toast } from "sonner";

import { ActionToken, ActivityDetail } from "@/components/activity-row";
import { ConnectorStatus } from "@/components/connector-status";
import { CopyableId } from "@/components/copyable-id";
import { DataTable } from "@/components/data-table";
import { PageHeader, Panel, TableMessage } from "@/components/page-header";
import { RoleBadge } from "@/components/role-badge";
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
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { ApiError } from "@/lib/api/client";
import {
  assignAdminOrgPlan,
  deleteAdminOrganization,
  getAdminOrganization,
  listAdminPlans,
  setAdminOrgLimits,
} from "@/lib/api/endpoints";
import { useSession } from "@/lib/auth/use-session";

function formatDate(value: string | null): string {
  return value ? new Date(value).toISOString().slice(0, 10) : "—";
}

// Renders `{ max_members: 10, max_connectors: -1 }` as "max_members: 10 ·
// max_connectors: unlimited" — -1 means unlimited by the same convention
// cmd/seed and the plan editor use.
function formatLimit(value: number): string {
  return value === -1 ? "unlimited" : String(value);
}

export function AdminOrganizationDetailClient({ orgId }: { orgId: string }) {
  const { platformRole } = useSession();
  const canMutate = platformRole === "superadmin";
  const queryClient = useQueryClient();

  const { data: org, isLoading, isError, error } = useQuery({
    queryKey: ["admin", "organizations", orgId],
    queryFn: () => getAdminOrganization(orgId),
    retry: false,
  });

  const { data: plans } = useQuery({
    queryKey: ["admin", "plans"],
    queryFn: listAdminPlans,
    enabled: canMutate,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["admin", "organizations", orgId] });

  // ---- plan assignment ----
  const [planId, setPlanId] = useState<string | null>(null);
  const assignPlanMutation = useMutation({
    mutationFn: (id: string) => assignAdminOrgPlan(orgId, id),
    onSuccess: () => {
      invalidate();
      toast.success("Plan assigned.");
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Failed to assign plan."),
  });

  // ---- custom limits ----
  // A plain JSON textarea, same shape as the connectors page's "generic"
  // config field — customLimits is a partial overlay (no required keys,
  // unlike a plan's own limits), so a form with fixed fields would either
  // hide keys the backend already enforces or invent ones that don't exist.
  const [limitsOpen, setLimitsOpen] = useState(false);
  const [limitsJson, setLimitsJson] = useState("");
  const setLimitsMutation = useMutation({
    mutationFn: (customLimits: Record<string, unknown> | null) => setAdminOrgLimits(orgId, customLimits),
    onSuccess: () => {
      invalidate();
      toast.success("Limits updated.");
      setLimitsOpen(false);
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Failed to update limits."),
  });
  const [limitsJsonError, setLimitsJsonError] = useState<string | null>(null);

  // ---- danger zone: delete ----
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [confirmSlug, setConfirmSlug] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const deleteMutation = useMutation({
    mutationFn: () => deleteAdminOrganization(orgId, { confirm: confirmSlug, password: confirmPassword }),
    onSuccess: () => {
      toast.success("Organization deleted.");
      window.location.href = "/admin/organizations";
    },
    onError: (err) => {
      // REAUTH_FAILED / ORG_CONFIRM_MISMATCH / TOO_MANY_ATTEMPTS are all
      // already human-readable server text — no need to special-case them.
      toast.error(err instanceof ApiError ? err.message : "Failed to delete organization.");
    },
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="organization" description="Full detail for one tenant, across everything it owns.">
        <Button variant="outline" size="sm" render={<Link href="/admin/organizations" />}>
          <ArrowLeftIcon className="size-4" /> Back to organizations
        </Button>
      </PageHeader>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : isError || !org ? (
        <Panel className="px-4 py-6 text-center text-sm text-muted-foreground">
          {error instanceof ApiError && error.status === 404
            ? "This organization doesn't exist."
            : error instanceof ApiError
              ? error.message
              : "Failed to load this organization."}
        </Panel>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-4 py-3">
            <span className="font-medium">{org.name}</span>
            <span className="font-mono text-xs text-muted-foreground">{org.slug}</span>
            <CopyableId value={org.id} label="organization ID" />
            <span className="ml-auto text-xs text-muted-foreground">Created {formatDate(org.createdAt)}</span>
          </div>

          <section className="grid gap-4 lg:grid-cols-2">
            <div className="space-y-2">
              <h2 className="font-heading text-base font-medium">Plan</h2>
              <Panel className="flex flex-wrap items-center gap-3 px-4 py-3">
                <span className="text-sm">{org.planName ?? "No plan assigned"}</span>
                {canMutate && (
                  <Select
                    value={planId ?? undefined}
                    onValueChange={(value) => {
                      if (!value) return;
                      setPlanId(value);
                      assignPlanMutation.mutate(value);
                    }}
                  >
                    <SelectTrigger className="ml-auto w-48" disabled={assignPlanMutation.isPending}>
                      <SelectValue placeholder="Change plan…" />
                    </SelectTrigger>
                    <SelectContent>
                      {plans?.items.map((plan) => (
                        <SelectItem key={plan.id} value={plan.id}>
                          {plan.name}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              </Panel>
            </div>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <h2 className="font-heading text-base font-medium">Effective limits</h2>
                {canMutate && (
                  <Dialog
                    open={limitsOpen}
                    onOpenChange={(open) => {
                      setLimitsOpen(open);
                      setLimitsJsonError(null);
                      if (open) setLimitsJson(JSON.stringify(org.effectiveLimits, null, 2));
                    }}
                  >
                    <DialogTrigger render={<Button size="xs" variant="outline" />}>Edit override</DialogTrigger>
                    <DialogContent>
                      <DialogHeader>
                        <DialogTitle>Custom limits</DialogTitle>
                        <DialogDescription>
                          Replaces the org&apos;s override whole — this is not merged with what&apos;s stored.
                          Values must be whole numbers; <span className="font-mono">-1</span> means unlimited.
                        </DialogDescription>
                      </DialogHeader>
                      <Field data-invalid={!!limitsJsonError}>
                        <FieldLabel htmlFor="org-limits-json">customLimits (JSON)</FieldLabel>
                        <Textarea
                          id="org-limits-json"
                          rows={8}
                          className="font-mono text-sm"
                          value={limitsJson}
                          onChange={(e) => setLimitsJson(e.target.value)}
                        />
                        {limitsJsonError && <FieldError errors={[{ message: limitsJsonError }]} />}
                      </Field>
                      <DialogFooter className="justify-between">
                        <Button
                          type="button"
                          variant="ghost"
                          className="text-muted-foreground hover:text-destructive"
                          disabled={setLimitsMutation.isPending}
                          onClick={() => setLimitsMutation.mutate(null)}
                        >
                          Clear override
                        </Button>
                        <Button
                          type="button"
                          disabled={setLimitsMutation.isPending}
                          onClick={() => {
                            try {
                              const parsed: unknown = JSON.parse(limitsJson || "{}");
                              if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
                                setLimitsJsonError("Must be a JSON object.");
                                return;
                              }
                              setLimitsJsonError(null);
                              setLimitsMutation.mutate(parsed as Record<string, unknown>);
                            } catch {
                              setLimitsJsonError("Not valid JSON.");
                            }
                          }}
                        >
                          {setLimitsMutation.isPending ? "Saving…" : "Save"}
                        </Button>
                      </DialogFooter>
                    </DialogContent>
                  </Dialog>
                )}
              </div>
              <Panel>
                <ul className="divide-y">
                  {Object.entries(org.effectiveLimits).length ? (
                    Object.entries(org.effectiveLimits).map(([key, value]) => (
                      <li key={key} className="flex items-center justify-between px-4 py-2 text-sm">
                        <span className="font-mono text-xs text-muted-foreground">{key}</span>
                        <span className="font-mono">{formatLimit(value)}</span>
                      </li>
                    ))
                  ) : (
                    <li className="px-4 py-3 text-sm text-muted-foreground">No subscription — no limits set.</li>
                  )}
                </ul>
              </Panel>
            </div>
          </section>

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">Members</h2>
            <DataTable columns={["Member", "Role", "Joined"]}>
              <TableBody>
                {org.members.length ? (
                  org.members.map((member) => (
                    <TableRow key={member.userId}>
                      <TableCell>
                        <Link
                          href={`/admin/users/${member.userId}`}
                          className="font-mono text-[0.8125rem] underline-offset-4 hover:underline"
                        >
                          {member.email}
                        </Link>
                        {member.displayName && (
                          <span className="ml-2 text-xs text-muted-foreground">{member.displayName}</span>
                        )}
                      </TableCell>
                      <TableCell>
                        <RoleBadge role={member.role} />
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {formatDate(member.joinedAt)}
                      </TableCell>
                    </TableRow>
                  ))
                ) : (
                  <TableMessage colSpan={3}>No members.</TableMessage>
                )}
              </TableBody>
            </DataTable>
          </section>

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">Connectors</h2>
            <DataTable columns={["Name", "Type", "Status", "Last health check"]}>
              <TableBody>
                {org.connectors.length ? (
                  org.connectors.map((connector) => (
                    <TableRow key={connector.id}>
                      <TableCell className="font-medium">{connector.name}</TableCell>
                      <TableCell className="text-sm text-muted-foreground">{connector.type}</TableCell>
                      <TableCell>
                        <ConnectorStatus status={connector.status} />
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {formatDate(connector.lastHealthCheckAt)}
                      </TableCell>
                    </TableRow>
                  ))
                ) : (
                  <TableMessage colSpan={4}>No connectors.</TableMessage>
                )}
              </TableBody>
            </DataTable>
          </section>

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">MCP keys</h2>
            <DataTable columns={["Name", "Owner", "Scopes", "Expires", "Revoked"]}>
              <TableBody>
                {org.mcpKeys.length ? (
                  org.mcpKeys.map((key) => (
                    <TableRow key={key.id}>
                      <TableCell className="font-medium">{key.name}</TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">{key.userEmail}</TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {key.scopes?.join(", ") ?? "full grant"}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {formatDate(key.expiresAt)}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {formatDate(key.revokedAt)}
                      </TableCell>
                    </TableRow>
                  ))
                ) : (
                  <TableMessage colSpan={5}>No MCP keys.</TableMessage>
                )}
              </TableBody>
            </DataTable>
          </section>

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">Recent audit</h2>
            <DataTable columns={["When", "Action", { label: "Detail", className: "w-full" }]}>
              <TableBody>
                {org.recentAuditLogs.length ? (
                  org.recentAuditLogs.map((log) => {
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
                  <TableMessage colSpan={3}>No activity recorded for this organization.</TableMessage>
                )}
              </TableBody>
            </DataTable>
          </section>

          {canMutate && (
            <section className="space-y-2 rounded-lg border border-destructive/40 p-4">
              <h2 className="font-heading text-base font-medium text-destructive">Danger zone</h2>
              <p className="text-sm text-muted-foreground">
                Deleting cascades every membership, connector, and MCP key this organization owns. Audit history
                survives — <span className="font-mono">audit_logs.organization_id</span> has no foreign key.
              </p>
              <Dialog
                open={deleteOpen}
                onOpenChange={(open) => {
                  setDeleteOpen(open);
                  if (!open) {
                    setConfirmSlug("");
                    setConfirmPassword("");
                  }
                }}
              >
                <DialogTrigger render={<Button variant="destructive" size="sm" />}>Delete organization</DialogTrigger>
                <DialogContent>
                  <DialogHeader>
                    <DialogTitle>Delete {org.name}</DialogTitle>
                    <DialogDescription>
                      Type the slug <span className="font-mono text-foreground">{org.slug}</span> and confirm your
                      own password. This cannot be undone.
                    </DialogDescription>
                  </DialogHeader>
                  <Field>
                    <FieldLabel htmlFor="org-delete-slug">Slug</FieldLabel>
                    <Input
                      id="org-delete-slug"
                      value={confirmSlug}
                      onChange={(e) => setConfirmSlug(e.target.value)}
                      placeholder={org.slug}
                    />
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="org-delete-password">Your password</FieldLabel>
                    <Input
                      id="org-delete-password"
                      type="password"
                      value={confirmPassword}
                      onChange={(e) => setConfirmPassword(e.target.value)}
                    />
                  </Field>
                  <DialogFooter>
                    <Button
                      variant="destructive"
                      disabled={deleteMutation.isPending || confirmSlug !== org.slug || !confirmPassword}
                      onClick={() => deleteMutation.mutate()}
                    >
                      {deleteMutation.isPending ? "Deleting…" : "Delete permanently"}
                    </Button>
                  </DialogFooter>
                </DialogContent>
              </Dialog>
            </section>
          )}
        </>
      )}
    </div>
  );
}
