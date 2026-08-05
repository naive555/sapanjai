"use client";

import { useState } from "react";
import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery } from "@tanstack/react-query";
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
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import { createOrganization, listOrganizations } from "@/lib/api/endpoints";
import { useActiveOrgId, useSelectOrg } from "@/lib/org/active-org";

const createOrgSchema = z.object({
  name: z.string().min(1, "Name is required"),
  slug: z
    .string()
    .min(2, "Slug must be at least 2 characters")
    .regex(/^[a-z0-9-]+$/, "Lowercase letters, numbers, and hyphens only"),
});

type CreateOrgValues = z.infer<typeof createOrgSchema>;

export default function OrganizationsPage() {
  const [open, setOpen] = useState(false);
  const activeOrgId = useActiveOrgId();
  const selectOrg = useSelectOrg();

  const { data: memberships, isLoading } = useQuery({
    queryKey: ["organizations"],
    queryFn: listOrganizations,
  });

  const {
    control,
    handleSubmit,
    reset,
    formState: { errors, isSubmitting },
  } = useForm<CreateOrgValues>({
    resolver: zodResolver(createOrgSchema),
    defaultValues: { name: "", slug: "" },
  });

  const createMutation = useMutation({
    mutationFn: createOrganization,
    onSuccess: (org) => {
      // Invalidates every org-scoped query, including the org list itself.
      selectOrg(org.id);
      toast.success(`Organization "${org.name}" created.`);
      reset();
      setOpen(false);
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to create organization.");
    },
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="organizations" description="Every tenant you belong to, and your role in each.">
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogTrigger render={<Button size="sm" />}>
            <PlusIcon className="size-4" /> Create organization
          </DialogTrigger>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Create organization</DialogTitle>
              <DialogDescription>You&apos;ll be the owner of the new organization.</DialogDescription>
            </DialogHeader>
            <form onSubmit={handleSubmit((values) => createMutation.mutate(values))} noValidate>
              <FieldGroup>
                <Field data-invalid={!!errors.name}>
                  <FieldLabel htmlFor="org-name">Name</FieldLabel>
                  <Controller
                    control={control}
                    name="name"
                    render={({ field }) => <Input id="org-name" placeholder="Acme Inc." {...field} />}
                  />
                  <FieldError errors={[errors.name]} />
                </Field>
                <Field data-invalid={!!errors.slug}>
                  <FieldLabel htmlFor="org-slug">Slug</FieldLabel>
                  <Controller
                    control={control}
                    name="slug"
                    render={({ field }) => (
                      <Input id="org-slug" className="font-mono" placeholder="acme-inc" {...field} />
                    )}
                  />
                  <FieldError errors={[errors.slug]} />
                </Field>
              </FieldGroup>
              <DialogFooter>
                <Button type="submit" disabled={isSubmitting}>
                  {isSubmitting ? "Creating…" : "Create"}
                </Button>
              </DialogFooter>
            </form>
          </DialogContent>
        </Dialog>
      </PageHeader>

      <DataTable
        columns={["Organization", "Your role", { label: "Scope", align: "end" }]}
      >
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={3}>Loading…</TableMessage>
          ) : memberships?.length ? (
            memberships.map((m) => {
              const isActive = m.organizationId === activeOrgId;
              return (
                <TableRow key={m.organizationId}>
                  <TableCell>
                    <div className="flex flex-col gap-0.5">
                      <span className="font-medium">{m.organization.name}</span>
                      <span className="font-mono text-xs text-muted-foreground">
                        {m.organization.slug}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell>
                    <RoleBadge role={m.role} />
                  </TableCell>
                  <TableCell className="text-right">
                    {isActive ? (
                      <span
                        className="inline-flex items-center gap-1.5 font-mono text-[0.6875rem]
                          tracking-[0.08em] text-signal uppercase"
                      >
                        <span aria-hidden className="size-1.5 rounded-full bg-signal" />
                        active
                      </span>
                    ) : (
                      <Button variant="outline" size="xs" onClick={() => selectOrg(m.organizationId)}>
                        Switch to
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              );
            })
          ) : (
            <TableMessage colSpan={3}>
              You don&apos;t belong to an organization yet. Create one to get started.
            </TableMessage>
          )}
        </TableBody>
      </DataTable>
    </div>
  );
}
