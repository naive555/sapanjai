"use client";

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { PlusIcon } from "lucide-react";
import { toast } from "sonner";

import { PageHeader, Panel } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { ApiError } from "@/lib/api/client";
import {
  createAdminPlan,
  deleteAdminPlan,
  getAdminSystemStats,
  listAdminPlans,
  updateAdminPlan,
  type AdminPlan,
} from "@/lib/api/endpoints";
import { useSession } from "@/lib/auth/use-session";

function parseLimitsJson(text: string): { ok: true; value: Record<string, unknown> } | { ok: false; error: string } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { ok: false, error: "Not valid JSON." };
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return { ok: false, error: "Must be a JSON object." };
  }
  return { ok: true, value: parsed as Record<string, unknown> };
}

interface PlanFormState {
  open: boolean;
  editing: AdminPlan | null;
  name: string;
  limitsJson: string;
  error: string | null;
}

const EMPTY_FORM: PlanFormState = { open: false, editing: null, name: "", limitsJson: "{}", error: null };

export default function AdminPlansPage() {
  const { platformRole } = useSession();
  const canMutate = platformRole === "superadmin";
  const queryClient = useQueryClient();

  const { data: plans, isLoading, isError, error } = useQuery({
    queryKey: ["admin", "plans"],
    queryFn: listAdminPlans,
  });

  // Best-effort "in use" signal — GET /admin/plans carries no such field, so
  // this cross-references GET /admin/system/stats' planBreakdown by name.
  // The actual enforcement is server-side (DELETE 409s PLAN_IN_USE
  // regardless); this is only what decides whether the button is disabled
  // up front.
  const { data: stats } = useQuery({ queryKey: ["admin", "system", "stats"], queryFn: getAdminSystemStats });
  const inUseNames = new Set((stats?.planBreakdown ?? []).filter((p) => p.orgCount > 0).map((p) => p.planName));

  const [form, setForm] = useState<PlanFormState>(EMPTY_FORM);
  const [deleteTarget, setDeleteTarget] = useState<AdminPlan | null>(null);

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["admin", "plans"] });
    queryClient.invalidateQueries({ queryKey: ["admin", "system", "stats"] });
  };

  const saveMutation = useMutation({
    mutationFn: (input: { name: string; limits: Record<string, unknown> }) =>
      form.editing ? updateAdminPlan(form.editing.id, input) : createAdminPlan(input),
    onSuccess: () => {
      invalidate();
      toast.success(form.editing ? "Plan updated." : "Plan created.");
      setForm(EMPTY_FORM);
    },
    onError: (err) => toast.error(err instanceof ApiError ? err.message : "Failed to save plan."),
  });

  const deleteMutation = useMutation({
    mutationFn: (planId: string) => deleteAdminPlan(planId),
    onSuccess: () => {
      invalidate();
      toast.success("Plan deleted.");
      setDeleteTarget(null);
    },
    onError: (err) => {
      // PLAN_IN_USE is already human-readable server text.
      toast.error(err instanceof ApiError ? err.message : "Failed to delete plan.");
      setDeleteTarget(null);
    },
  });

  function openCreate() {
    setForm({ open: true, editing: null, name: "", limitsJson: "{\n  \"max_members\": 5,\n  \"max_roles\": 5,\n  \"max_connectors\": 3\n}", error: null });
  }

  function openEdit(plan: AdminPlan) {
    setForm({ open: true, editing: plan, name: plan.name, limitsJson: JSON.stringify(plan.limits, null, 2), error: null });
  }

  function submitForm() {
    const parsed = parseLimitsJson(form.limitsJson);
    if (!parsed.ok) {
      setForm((f) => ({ ...f, error: parsed.error }));
      return;
    }
    if (!form.name.trim()) {
      setForm((f) => ({ ...f, error: "Name is required." }));
      return;
    }
    saveMutation.mutate({ name: form.name.trim(), limits: parsed.value });
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="plans" description="Subscription plans every organization is assigned against.">
        {canMutate && (
          <Button size="sm" onClick={openCreate}>
            <PlusIcon className="size-4" /> Create plan
          </Button>
        )}
      </PageHeader>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : isError ? (
        <Panel className="px-4 py-6 text-center text-sm text-muted-foreground">
          {error instanceof ApiError ? error.message : "Failed to load plans."}
        </Panel>
      ) : plans?.items.length ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {plans.items.map((plan) => {
            const inUse = inUseNames.has(plan.name);
            return (
              <Panel key={plan.id} className="flex flex-col gap-3 p-4">
                <div className="flex items-center justify-between">
                  <span className="font-heading text-base font-medium">{plan.name}</span>
                  {canMutate && (
                    <Button variant="ghost" size="xs" onClick={() => openEdit(plan)}>
                      Edit
                    </Button>
                  )}
                </div>
                <ul className="space-y-1 text-sm text-muted-foreground">
                  {Object.entries(plan.limits).map(([key, value]) => (
                    <li key={key} className="flex items-center justify-between font-mono text-xs">
                      <span>{key}</span>
                      <span>{String(value) === "-1" ? "unlimited" : String(value)}</span>
                    </li>
                  ))}
                </ul>
                {canMutate && (
                  <Button
                    variant="ghost"
                    size="xs"
                    disabled={inUse}
                    title={inUse ? "Plan has active subscriptions" : undefined}
                    className="mt-auto self-start text-muted-foreground hover:text-destructive disabled:opacity-40"
                    onClick={() => setDeleteTarget(plan)}
                  >
                    Delete
                  </Button>
                )}
              </Panel>
            );
          })}
        </div>
      ) : (
        <Panel className="px-4 py-6 text-center text-sm text-muted-foreground">No plans yet.</Panel>
      )}

      <Dialog open={form.open} onOpenChange={(open) => setForm(open ? form : EMPTY_FORM)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{form.editing ? `Edit ${form.editing.name}` : "Create plan"}</DialogTitle>
            <DialogDescription>
              Limits are free-form JSON — values must be whole numbers, and{" "}
              <span className="font-mono">-1</span> means unlimited (per{" "}
              <span className="font-mono">cmd/seed</span>).
            </DialogDescription>
          </DialogHeader>
          <Field>
            <FieldLabel htmlFor="plan-name">Name</FieldLabel>
            <Input
              id="plan-name"
              value={form.name}
              onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
            />
          </Field>
          <Field data-invalid={!!form.error}>
            <FieldLabel htmlFor="plan-limits">Limits (JSON)</FieldLabel>
            <Textarea
              id="plan-limits"
              rows={8}
              className="font-mono text-sm"
              value={form.limitsJson}
              onChange={(e) => setForm((f) => ({ ...f, limitsJson: e.target.value }))}
            />
            {form.error && <FieldError errors={[{ message: form.error }]} />}
          </Field>
          <DialogFooter>
            <Button disabled={saveMutation.isPending} onClick={submitForm}>
              {saveMutation.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={deleteTarget !== null} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete plan</DialogTitle>
            <DialogDescription>
              Delete <span className="font-mono text-foreground">{deleteTarget?.name}</span>? This is refused if
              any organization still subscribes to it.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={deleteMutation.isPending}
              onClick={() => deleteTarget && deleteMutation.mutate(deleteTarget.id)}
            >
              {deleteMutation.isPending ? "Deleting…" : "Delete"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
