"use client";

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ApiError } from "@/lib/api/client";
import { assignSubscription, getSubscription, listPlans } from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";

// Limit keys arrive as snake_case identifiers; they're the only strings on
// this page meant to be read as words rather than as data.
function humanize(key: string): string {
  return key.replace(/_/g, " ");
}

function LimitCell({ label, value }: { label: string; value: unknown }) {
  const unlimited = value === -1;
  return (
    <div className="flex flex-col gap-1.5 border-l px-4 py-3 first:border-l-0 first:pl-0">
      <span className="label-eyebrow">{humanize(label)}</span>
      <span className={`font-mono text-xl leading-none ${unlimited ? "text-signal" : "text-foreground"}`}>
        {unlimited ? "∞" : String(value)}
      </span>
    </div>
  );
}

export default function SubscriptionPage() {
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();
  const [selectedPlanId, setSelectedPlanId] = useState("");

  const { data: subscription, isLoading } = useQuery({
    queryKey: ["subscription", activeOrgId],
    queryFn: getSubscription,
    enabled: activeOrgId !== null,
  });

  // Plans are global, not org-scoped — no activeOrgId in the query key.
  const { data: plans } = useQuery({ queryKey: ["plans"], queryFn: listPlans });

  // The contract has no admin/permission check on assign — any org member
  // can change the plan. The UI doesn't add a client-side gate either, for
  // parity with the source app's behavior (see lib/api/endpoints.ts).
  const assignMutation = useMutation({
    mutationFn: (planId: string) => assignSubscription(planId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["subscription", activeOrgId] });
      toast.success("Plan assigned.");
      setSelectedPlanId("");
    },
    onError: (err) => {
      toast.error(err instanceof ApiError ? err.message : "Failed to assign plan.");
    },
  });

  const limits = Object.entries(subscription?.plan.limits ?? {});

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader title="subscription" description="The plan this organization runs on, and its limits." />

      <section className="rounded-lg border bg-card">
        <div className="flex flex-wrap items-baseline justify-between gap-x-6 gap-y-2 border-b px-5 py-4">
          <div className="flex items-baseline gap-3">
            <span className="label-eyebrow">Current plan</span>
            <h2 className="font-display text-lg leading-none">
              {isLoading ? "…" : (subscription?.plan.name ?? "none")}
            </h2>
          </div>
          {!isLoading && !subscription && (
            <p className="text-sm text-muted-foreground">
              This organization has no plan yet, so limits are unenforced.
            </p>
          )}
        </div>

        {limits.length > 0 && (
          <div className="flex flex-wrap px-5 py-4">
            {limits.map(([key, value]) => (
              <LimitCell key={key} label={key} value={value} />
            ))}
          </div>
        )}
      </section>

      <section className="flex flex-col gap-3">
        <h2 className="label-eyebrow">Change plan</h2>
        <div className="flex flex-wrap items-center gap-2">
          <Select value={selectedPlanId} onValueChange={(value) => setSelectedPlanId(value ?? "")}>
            <SelectTrigger className="w-60">
              <SelectValue placeholder="Select a plan">
                {(value: string) => plans?.find((p) => p.id === value)?.name ?? "Select a plan"}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              {plans?.map((p) => (
                <SelectItem key={p.id} value={p.id}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button
            disabled={!selectedPlanId || assignMutation.isPending}
            onClick={() => assignMutation.mutate(selectedPlanId)}
          >
            {assignMutation.isPending ? "Assigning…" : "Assign plan"}
          </Button>
        </div>
      </section>
    </div>
  );
}
