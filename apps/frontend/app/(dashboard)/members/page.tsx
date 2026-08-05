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
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import { invite, listMembers, listOrganizations, removeMember } from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";

const inviteSchema = z.object({
  email: z.email("Enter a valid email address"),
  role: z.enum(["admin", "member"]),
});

type InviteValues = z.infer<typeof inviteSchema>;

export default function MembersPage() {
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();
  const [inviteOpen, setInviteOpen] = useState(false);
  const [removeTarget, setRemoveTarget] = useState<{ userId: string; email: string } | null>(null);

  const { data: memberships } = useQuery({ queryKey: ["organizations"], queryFn: listOrganizations });
  const callerRole = memberships?.find((m) => m.organizationId === activeOrgId)?.role;
  const canManage = callerRole !== undefined && callerRole !== "member";

  const { data: members, isLoading } = useQuery({
    queryKey: ["members", activeOrgId],
    queryFn: listMembers,
    enabled: activeOrgId !== null,
  });

  const {
    control,
    handleSubmit,
    reset,
    formState: { errors, isSubmitting },
  } = useForm<InviteValues>({
    resolver: zodResolver(inviteSchema),
    defaultValues: { email: "", role: "member" },
  });

  const inviteMutation = useMutation({
    mutationFn: invite,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["members", activeOrgId] });
      toast.success("Member invited.");
      reset();
      setInviteOpen(false);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to invite member.");
    },
  });

  const removeMutation = useMutation({
    mutationFn: removeMember,
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["members", activeOrgId] });
      toast.success("Member removed.");
      setRemoveTarget(null);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to remove member.");
      setRemoveTarget(null);
    },
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="members" description="Who belongs to this organization, and what they can do.">
        <Dialog open={inviteOpen} onOpenChange={setInviteOpen}>
          <DialogTrigger
            render={
              <Button
                size="sm"
                disabled={!canManage}
                title={canManage ? undefined : "Only owners and admins can invite members"}
              />
            }
          >
            <PlusIcon className="size-4" /> Invite member
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Invite member</DialogTitle>
              <DialogDescription>Invite an existing user to this organization.</DialogDescription>
            </DialogHeader>
            <form onSubmit={handleSubmit((values) => inviteMutation.mutate(values))} noValidate>
              <FieldGroup>
                <Field data-invalid={!!errors.email}>
                  <FieldLabel htmlFor="invite-email">Email</FieldLabel>
                  <Controller
                    control={control}
                    name="email"
                    render={({ field }) => (
                      <Input id="invite-email" type="email" placeholder="teammate@example.com" {...field} />
                    )}
                  />
                  <FieldError errors={[errors.email]} />
                </Field>
                <Field data-invalid={!!errors.role}>
                  <FieldLabel htmlFor="invite-role">Role</FieldLabel>
                  <Controller
                    control={control}
                    name="role"
                    render={({ field }) => (
                      <Select value={field.value} onValueChange={field.onChange}>
                        <SelectTrigger id="invite-role" className="w-full">
                          <SelectValue>
                            {(value: string) => (value === "admin" ? "Admin" : "Member")}
                          </SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="member">Member</SelectItem>
                          <SelectItem value="admin">Admin</SelectItem>
                        </SelectContent>
                      </Select>
                    )}
                  />
                  <FieldError errors={[errors.role]} />
                </Field>
              </FieldGroup>
              <DialogFooter>
                <Button type="submit" disabled={isSubmitting}>
                  {isSubmitting ? "Inviting…" : "Invite"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </PageHeader>

      <DataTable columns={["Member", "Role", "Joined", { label: "", align: "end" }]}>
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={4}>Loading…</TableMessage>
          ) : members?.length ? (
            members.map((m) => (
              <TableRow key={m.userId}>
                <TableCell>
                  <div className="flex flex-col gap-0.5">
                    <span className="font-mono text-[0.8125rem]">{m.email}</span>
                    {m.displayName && (
                      <span className="text-xs text-muted-foreground">{m.displayName}</span>
                    )}
                  </div>
                </TableCell>
                <TableCell>
                  <RoleBadge role={m.role} />
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">
                  {new Date(m.joinedAt).toISOString().slice(0, 10)}
                </TableCell>
                <TableCell className="text-right">
                  {m.role !== "owner" && canManage && (
                    <Button
                      variant="ghost"
                      size="xs"
                      className="text-muted-foreground hover:text-destructive"
                      onClick={() => setRemoveTarget({ userId: m.userId, email: m.email })}
                    >
                      Remove
                    </Button>
                  )}
                </TableCell>
              </TableRow>
            ))
          ) : (
            <TableMessage colSpan={4}>
              {canManage
                ? "No members yet. Invite a teammate to share this organization."
                : "No members yet."}
            </TableMessage>
          )}
        </TableBody>
      </DataTable>

      <Dialog open={removeTarget !== null} onOpenChange={(open) => !open && setRemoveTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Remove member</DialogTitle>
            <DialogDescription>
              Remove <span className="font-mono text-foreground">{removeTarget?.email}</span> from this
              organization? They lose access immediately.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRemoveTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={removeMutation.isPending}
              onClick={() => removeTarget && removeMutation.mutate(removeTarget.userId)}
            >
              {removeMutation.isPending ? "Removing…" : "Remove"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
