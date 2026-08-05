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
import { PermissionToken } from "@/components/permission-token";
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
import { Textarea } from "@/components/ui/textarea";
import { ApiError } from "@/lib/api/client";
import {
  assignRole,
  createRole,
  listMembers,
  listRoles,
  updatePermissions,
  type RoleResponse,
} from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";

// Splits a free-form textarea (one permission per line, commas also
// accepted) into the string[] the contract expects. Deliberately does not
// dedupe: a duplicate action hits the backend's unique constraint and
// surfaces as a plain 500, kept bug-for-bug per docs/03's Phase 4 decision.
function parsePermissions(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

const createRoleSchema = z.object({
  name: z.string().min(1, "Name is required"),
  description: z.string().optional(),
  permissionsText: z.string().optional(),
});
type CreateRoleValues = z.infer<typeof createRoleSchema>;

const editPermissionsSchema = z.object({
  permissionsText: z.string().optional(),
});
type EditPermissionsValues = z.infer<typeof editPermissionsSchema>;

const assignRoleSchema = z.object({
  userId: z.string().min(1, "Select a member"),
  roleId: z.string().min(1, "Select a role"),
});
type AssignRoleValues = z.infer<typeof assignRoleSchema>;

// The precedence the backend's RequirePermission middleware applies. Shown
// beside the permission editor because the ordering is the whole reason a
// wildcard grant is worth thinking twice about.
function PrecedenceLegend() {
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-md border border-dashed bg-muted/40 px-3 py-2.5">
      <span className="label-eyebrow">Matches</span>
      <PermissionToken action="*" />
      <span aria-label="then" className="text-xs text-muted-foreground">
        ›
      </span>
      <PermissionToken action="billing:read" />
      <span aria-label="then" className="text-xs text-muted-foreground">
        ›
      </span>
      <PermissionToken action="billing:*" />
    </div>
  );
}

export default function RolesPage() {
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [assignOpen, setAssignOpen] = useState(false);
  const [editingRole, setEditingRole] = useState<RoleResponse | null>(null);

  const { data: roles, isLoading } = useQuery({
    queryKey: ["roles", activeOrgId],
    queryFn: listRoles,
    enabled: activeOrgId !== null,
  });

  const { data: members } = useQuery({
    queryKey: ["members", activeOrgId],
    queryFn: listMembers,
    enabled: activeOrgId !== null,
  });

  const invalidateRoles = () => queryClient.invalidateQueries({ queryKey: ["roles", activeOrgId] });

  // ---- create role ----
  const createForm = useForm<CreateRoleValues>({
    resolver: zodResolver(createRoleSchema),
    defaultValues: { name: "", description: "", permissionsText: "" },
  });

  const createMutation = useMutation({
    mutationFn: (values: CreateRoleValues) =>
      createRole({
        name: values.name,
        description: values.description?.trim() || undefined,
        permissions: parsePermissions(values.permissionsText ?? ""),
      }),
    onSuccess: (role) => {
      invalidateRoles();
      toast.success(`Role "${role.name}" created.`);
      createForm.reset();
      setCreateOpen(false);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to create role.");
    },
  });

  // ---- edit permissions ----
  const editForm = useForm<EditPermissionsValues>({
    resolver: zodResolver(editPermissionsSchema),
    defaultValues: { permissionsText: "" },
  });

  const editMutation = useMutation({
    mutationFn: (values: EditPermissionsValues) => {
      if (!editingRole) throw new Error("no role selected");
      return updatePermissions(editingRole.id, parsePermissions(values.permissionsText ?? ""));
    },
    onSuccess: () => {
      invalidateRoles();
      toast.success("Permissions updated.");
      setEditingRole(null);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to update permissions.");
    },
  });

  function openEdit(role: RoleResponse) {
    editForm.reset({ permissionsText: role.permissions.map((p) => p.action).join("\n") });
    setEditingRole(role);
  }

  // ---- assign role ----
  const assignForm = useForm<AssignRoleValues>({
    resolver: zodResolver(assignRoleSchema),
    defaultValues: { userId: "", roleId: "" },
  });

  const assignMutation = useMutation({
    mutationFn: (values: AssignRoleValues) => assignRole(values),
    onSuccess: () => {
      toast.success("Role assigned.");
      assignForm.reset();
      setAssignOpen(false);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to assign role.");
    },
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="roles" description="Custom roles and the actions they grant in this organization.">
        <Dialog open={assignOpen} onOpenChange={setAssignOpen}>
          <DialogTrigger render={<Button variant="outline" size="sm" />}>Assign role</DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Assign role</DialogTitle>
              <DialogDescription>Assign a custom role to a member.</DialogDescription>
            </DialogHeader>
            <form onSubmit={assignForm.handleSubmit((values) => assignMutation.mutate(values))} noValidate>
              <FieldGroup>
                <Field data-invalid={!!assignForm.formState.errors.userId}>
                  <FieldLabel htmlFor="assign-user">Member</FieldLabel>
                  <Controller
                    control={assignForm.control}
                    name="userId"
                    render={({ field }) => (
                      <Select value={field.value} onValueChange={field.onChange}>
                        <SelectTrigger id="assign-user" className="w-full">
                          <SelectValue placeholder="Select a member">
                            {(value: string) =>
                              members?.find((m) => m.userId === value)?.email ?? "Select a member"
                            }
                          </SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          {members?.map((m) => (
                            <SelectItem key={m.userId} value={m.userId}>
                              {m.email}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    )}
                  />
                  <FieldError errors={[assignForm.formState.errors.userId]} />
                </Field>
                <Field data-invalid={!!assignForm.formState.errors.roleId}>
                  <FieldLabel htmlFor="assign-role">Role</FieldLabel>
                  <Controller
                    control={assignForm.control}
                    name="roleId"
                    render={({ field }) => (
                      <Select value={field.value} onValueChange={field.onChange}>
                        <SelectTrigger id="assign-role" className="w-full">
                          <SelectValue placeholder="Select a role">
                            {(value: string) => roles?.find((r) => r.id === value)?.name ?? "Select a role"}
                          </SelectValue>
                        </SelectTrigger>
                        <SelectContent>
                          {roles?.map((r) => (
                            <SelectItem key={r.id} value={r.id}>
                              {r.name}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    )}
                  />
                  <FieldError errors={[assignForm.formState.errors.roleId]} />
                </Field>
              </FieldGroup>
              <DialogFooter>
                <Button type="submit" disabled={assignMutation.isPending}>
                  {assignMutation.isPending ? "Assigning…" : "Assign"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>

        <Dialog open={createOpen} onOpenChange={setCreateOpen}>
          <DialogTrigger render={<Button size="sm" />}>
            <PlusIcon className="size-4" /> Create role
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Create role</DialogTitle>
              <DialogDescription>
                One permission per line. Broader grants win over narrower ones.
              </DialogDescription>
            </DialogHeader>
            <form onSubmit={createForm.handleSubmit((values) => createMutation.mutate(values))} noValidate>
              <FieldGroup>
                <Field data-invalid={!!createForm.formState.errors.name}>
                  <FieldLabel htmlFor="role-name">Name</FieldLabel>
                  <Controller
                    control={createForm.control}
                    name="name"
                    render={({ field }) => <Input id="role-name" placeholder="Billing admin" {...field} />}
                  />
                  <FieldError errors={[createForm.formState.errors.name]} />
                </Field>
                <Field data-invalid={!!createForm.formState.errors.description}>
                  <FieldLabel htmlFor="role-description">Description (optional)</FieldLabel>
                  <Controller
                    control={createForm.control}
                    name="description"
                    render={({ field }) => <Input id="role-description" {...field} />}
                  />
                  <FieldError errors={[createForm.formState.errors.description]} />
                </Field>
                <Field data-invalid={!!createForm.formState.errors.permissionsText}>
                  <FieldLabel htmlFor="role-permissions">Permissions</FieldLabel>
                  <PrecedenceLegend />
                  <Controller
                    control={createForm.control}
                    name="permissionsText"
                    render={({ field }) => (
                      <Textarea
                        id="role-permissions"
                        rows={4}
                        className="font-mono text-sm"
                        placeholder={"org:read\nrbac:*"}
                        {...field}
                      />
                    )}
                  />
                  <FieldError errors={[createForm.formState.errors.permissionsText]} />
                </Field>
              </FieldGroup>
              <DialogFooter>
                <Button type="submit" disabled={createMutation.isPending}>
                  {createMutation.isPending ? "Creating…" : "Create"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </PageHeader>

      <DataTable columns={["Role", "Grants", { label: "", align: "end" }]}>
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={3}>Loading…</TableMessage>
          ) : roles?.length ? (
            roles.map((role) => (
              <TableRow key={role.id}>
                <TableCell className="align-top">
                  <div className="flex flex-col gap-0.5">
                    <span className="font-medium">{role.name}</span>
                    {role.description && (
                      <span className="text-xs text-muted-foreground">{role.description}</span>
                    )}
                  </div>
                </TableCell>
                <TableCell className="align-top whitespace-normal">
                  {role.permissions.length ? (
                    <div className="flex flex-wrap gap-1">
                      {role.permissions.map((p) => (
                        <PermissionToken key={p.id} action={p.action} />
                      ))}
                    </div>
                  ) : (
                    <span className="text-xs text-muted-foreground">Grants nothing yet</span>
                  )}
                </TableCell>
                <TableCell className="text-right align-top">
                  <Button variant="outline" size="xs" onClick={() => openEdit(role)}>
                    Edit grants
                  </Button>
                </TableCell>
              </TableRow>
            ))
          ) : (
            <TableMessage colSpan={3}>
              No custom roles yet. Create one to grant actions beyond owner, admin, and member.
            </TableMessage>
          )}
        </TableBody>
      </DataTable>

      <Dialog open={editingRole !== null} onOpenChange={(open) => !open && setEditingRole(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Edit grants — {editingRole?.name}</DialogTitle>
            <DialogDescription>Replaces the role&apos;s entire permission set.</DialogDescription>
          </DialogHeader>
          <form onSubmit={editForm.handleSubmit((values) => editMutation.mutate(values))} noValidate>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="edit-permissions">Permissions</FieldLabel>
                <PrecedenceLegend />
                <Controller
                  control={editForm.control}
                  name="permissionsText"
                  render={({ field }) => (
                    <Textarea id="edit-permissions" rows={4} className="font-mono text-sm" {...field} />
                  )}
                />
              </Field>
            </FieldGroup>
            <DialogFooter>
              <Button type="submit" disabled={editMutation.isPending}>
                {editMutation.isPending ? "Saving…" : "Save"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}
