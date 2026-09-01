"use client";

import { useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeftIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
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
import { Field, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { ApiError } from "@/lib/api/client";
import { changeAdminUserPlatformRole, getAdminUser, impersonateAdminUser, setAdminUserBan } from "@/lib/api/endpoints";
import { useSession } from "@/lib/auth/use-session";

function formatDate(value: string | null): string {
  return value ? new Date(value).toISOString().slice(0, 10) : "—";
}

const NO_ROLE = "__none__";

export function AdminUserDetailClient({ userId }: { userId: string }) {
  const { platformRole, startImpersonation } = useSession();
  const canMutate = platformRole === "superadmin";
  const queryClient = useQueryClient();
  const router = useRouter();

  const { data: user, isLoading, isError, error } = useQuery({
    queryKey: ["admin", "users", userId],
    queryFn: () => getAdminUser(userId),
    retry: false,
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["admin", "users", userId] });

  // ---- platform role ----
  const [roleOpen, setRoleOpen] = useState(false);
  const [roleValue, setRoleValue] = useState<string>(NO_ROLE);
  const [rolePassword, setRolePassword] = useState("");
  const roleMutation = useMutation({
    mutationFn: () =>
      changeAdminUserPlatformRole(userId, {
        role: roleValue === NO_ROLE ? null : (roleValue as "superadmin" | "support"),
        password: rolePassword,
      }),
    onSuccess: () => {
      invalidate();
      toast.success("Platform role updated.");
      setRoleOpen(false);
      setRolePassword("");
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Failed to update platform role."),
  });

  // ---- ban ----
  const [banOpen, setBanOpen] = useState(false);
  const [banReason, setBanReason] = useState("");
  const [banPassword, setBanPassword] = useState("");
  const banMutation = useMutation({
    mutationFn: (banned: boolean) =>
      setAdminUserBan(userId, { banned, reason: banned ? banReason || undefined : undefined, password: banPassword }),
    onSuccess: (_result, banned) => {
      invalidate();
      toast.success(banned ? "User banned." : "User unbanned.");
      setBanOpen(false);
      setBanPassword("");
      setBanReason("");
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Failed to update ban status."),
  });

  // ---- impersonate ----
  const [impersonateOpen, setImpersonateOpen] = useState(false);
  const [reason, setReason] = useState("");
  const impersonateMutation = useMutation({
    mutationFn: () => impersonateAdminUser(userId, reason),
    onSuccess: (response) => {
      startImpersonation(response);
      setImpersonateOpen(false);
      setReason("");
      toast.success(`Viewing as ${response.user.email}.`);
      router.push("/overview");
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Failed to start impersonation."),
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="user" description="Full detail for one account, across every organization it belongs to.">
        <Button variant="outline" size="sm" render={<Link href="/admin/users" />}>
          <ArrowLeftIcon className="size-4" /> Back to users
        </Button>
      </PageHeader>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : isError || !user ? (
        <Panel className="px-4 py-6 text-center text-sm text-muted-foreground">
          {error instanceof ApiError && error.status === 404
            ? "This user doesn't exist."
            : error instanceof ApiError
              ? error.message
              : "Failed to load this user."}
        </Panel>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-4 py-3">
            <span className="font-mono text-sm">{user.email}</span>
            {user.displayName && <span className="text-sm text-muted-foreground">{user.displayName}</span>}
            <CopyableId value={user.id} label="user ID" />
            {user.platformRole && (
              <Badge variant="outline" className="uppercase">
                {user.platformRole}
              </Badge>
            )}
            {user.bannedAt && <Badge variant="destructive">Banned</Badge>}
            {user.isVerified ? (
              <Badge variant="outline" className="border-wire/40 bg-wire/10 text-wire">
                Verified
              </Badge>
            ) : (
              <Badge variant="outline">Unverified</Badge>
            )}
            <span className="ml-auto text-xs text-muted-foreground">
              {user.activeSessions} active session{user.activeSessions === 1 ? "" : "s"} · created{" "}
              {formatDate(user.createdAt)}
            </span>
          </div>

          {user.bannedAt && user.banReason && (
            <Panel className="px-4 py-3 text-sm text-muted-foreground">
              <span className="label-eyebrow mr-2">Ban reason</span>
              {user.banReason}
            </Panel>
          )}

          <section className="flex flex-wrap gap-2">
            {canMutate && (
              <Dialog
                open={roleOpen}
                onOpenChange={(open) => {
                  setRoleOpen(open);
                  if (open) setRoleValue(user.platformRole ?? NO_ROLE);
                  else setRolePassword("");
                }}
              >
                <DialogTrigger render={<Button variant="outline" size="sm" />}>Change platform role</DialogTrigger>
                <DialogContent>
                  <DialogHeader>
                    <DialogTitle>Change platform role</DialogTitle>
                    <DialogDescription>
                      Also revokes every session this user holds — a demotion ends their tenant sessions
                      immediately, not at token expiry.
                    </DialogDescription>
                  </DialogHeader>
                  <Field>
                    <FieldLabel htmlFor="user-role-select">Role</FieldLabel>
                    <Select value={roleValue} onValueChange={(v) => setRoleValue(v ?? NO_ROLE)}>
                      <SelectTrigger id="user-role-select" className="w-full">
                        <SelectValue>
                          {(v: string) => (v === NO_ROLE ? "Not platform staff" : v)}
                        </SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value={NO_ROLE}>Not platform staff</SelectItem>
                        <SelectItem value="superadmin">Superadmin</SelectItem>
                        <SelectItem value="support">Support</SelectItem>
                      </SelectContent>
                    </Select>
                  </Field>
                  <Field>
                    <FieldLabel htmlFor="user-role-password">Your password</FieldLabel>
                    <Input
                      id="user-role-password"
                      type="password"
                      value={rolePassword}
                      onChange={(e) => setRolePassword(e.target.value)}
                    />
                  </Field>
                  <DialogFooter>
                    <Button
                      disabled={roleMutation.isPending || !rolePassword}
                      onClick={() => roleMutation.mutate()}
                    >
                      {roleMutation.isPending ? "Saving…" : "Save"}
                    </Button>
                  </DialogFooter>
                </DialogContent>
              </Dialog>
            )}

            {canMutate && (
              <Dialog
                open={banOpen}
                onOpenChange={(open) => {
                  setBanOpen(open);
                  if (!open) {
                    setBanPassword("");
                    setBanReason("");
                  }
                }}
              >
                <DialogTrigger
                  render={<Button variant={user.bannedAt ? "outline" : "destructive"} size="sm" />}
                >
                  {user.bannedAt ? "Unban" : "Ban"}
                </DialogTrigger>
                <DialogContent>
                  <DialogHeader>
                    <DialogTitle>{user.bannedAt ? "Unban" : "Ban"} this user</DialogTitle>
                    <DialogDescription>
                      {user.bannedAt
                        ? "Restores login, session refresh, and their MCP keys."
                        : "Ends every session immediately and blocks login, refresh, and MCP key use."}
                    </DialogDescription>
                  </DialogHeader>
                  {!user.bannedAt && (
                    <Field>
                      <FieldLabel htmlFor="ban-reason">Reason (optional)</FieldLabel>
                      <Textarea id="ban-reason" value={banReason} onChange={(e) => setBanReason(e.target.value)} />
                    </Field>
                  )}
                  <Field>
                    <FieldLabel htmlFor="ban-password">Your password</FieldLabel>
                    <Input
                      id="ban-password"
                      type="password"
                      value={banPassword}
                      onChange={(e) => setBanPassword(e.target.value)}
                    />
                  </Field>
                  <DialogFooter>
                    <Button
                      variant={user.bannedAt ? "default" : "destructive"}
                      disabled={banMutation.isPending || !banPassword}
                      onClick={() => banMutation.mutate(!user.bannedAt)}
                    >
                      {banMutation.isPending ? "Saving…" : user.bannedAt ? "Unban" : "Ban"}
                    </Button>
                  </DialogFooter>
                </DialogContent>
              </Dialog>
            )}

            {/* Support may impersonate too (docs/11-admin-panel.md §4's role
                matrix) — this button is deliberately not gated on canMutate. */}
            {!user.platformRole && !user.bannedAt && (
              <Dialog
                open={impersonateOpen}
                onOpenChange={(open) => {
                  setImpersonateOpen(open);
                  if (!open) setReason("");
                }}
              >
                <DialogTrigger render={<Button variant="outline" size="sm" />}>Impersonate</DialogTrigger>
                <DialogContent>
                  <DialogHeader>
                    <DialogTitle>Impersonate {user.email}</DialogTitle>
                    <DialogDescription>
                      Read-only, expires in 10 minutes, and is audited with this reason attached
                      (docs/11-admin-panel.md §5).
                    </DialogDescription>
                  </DialogHeader>
                  <Field>
                    <FieldLabel htmlFor="impersonate-reason">Reason (minimum 10 characters)</FieldLabel>
                    <Textarea
                      id="impersonate-reason"
                      value={reason}
                      onChange={(e) => setReason(e.target.value)}
                      placeholder="e.g. investigating a support ticket about a stuck MCP key"
                    />
                  </Field>
                  <DialogFooter>
                    <Button
                      disabled={impersonateMutation.isPending || reason.trim().length < 10}
                      onClick={() => impersonateMutation.mutate()}
                    >
                      {impersonateMutation.isPending ? "Starting…" : "Start impersonation"}
                    </Button>
                  </DialogFooter>
                </DialogContent>
              </Dialog>
            )}
          </section>

          <section className="space-y-2">
            <h2 className="font-heading text-base font-medium">Memberships</h2>
            <DataTable columns={["Organization", "Role", "Joined"]}>
              <TableBody>
                {user.memberships.length ? (
                  user.memberships.map((membership) => (
                    <TableRow key={membership.organizationId}>
                      <TableCell>
                        <Link
                          href={`/admin/organizations/${membership.organizationId}`}
                          className="underline-offset-4 hover:underline"
                        >
                          {membership.organizationName}
                        </Link>
                        <span className="ml-2 font-mono text-xs text-muted-foreground">
                          {membership.organizationSlug}
                        </span>
                      </TableCell>
                      <TableCell>
                        <RoleBadge role={membership.role} />
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground">
                        {formatDate(membership.joinedAt)}
                      </TableCell>
                    </TableRow>
                  ))
                ) : (
                  <TableMessage colSpan={3}>Not a member of any organization.</TableMessage>
                )}
              </TableBody>
            </DataTable>
          </section>
        </>
      )}
    </div>
  );
}
