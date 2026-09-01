"use client";

import { useQuery } from "@tanstack/react-query";

import { PageHeader } from "@/components/page-header";
import { getSubscription, listPlans } from "@/lib/api/endpoints";
import { useActiveOrgId } from "@/lib/org/active-org";
import { cn } from "@/lib/utils";

// Limit keys arrive as snake_case identifiers; they're the only strings on
// this page meant to be read as words rather than as data.
function humanize(key: string): string {
  return key.replace(/_/g, " ");
}

function formatLimit(value: unknown): string {
  return value === -1 ? "∞" : String(value);
}

function LimitCell({ label, value }: { label: string; value: unknown }) {
  const unlimited = value === -1;
  return (
    <div className="flex flex-col gap-1.5 border-l px-4 py-3 first:border-l-0 first:pl-0">
      <span className="label-eyebrow">{humanize(label)}</span>
      <span className={`font-mono text-xl leading-none ${unlimited ? "text-signal" : "text-foreground"}`}>
        {formatLimit(value)}
      </span>
    </div>
  );
}

export default function SubscriptionPage() {
  const activeOrgId = useActiveOrgId();

  const { data: subscription, isLoading } = useQuery({
    queryKey: ["subscription", activeOrgId],
    queryFn: getSubscription,
    enabled: activeOrgId !== null,
  });

  // Plans are global, not org-scoped — no activeOrgId in the query key.
  // Read-only: there is no tenant-facing way to change a plan (see
  // lib/api/endpoints.ts's listPlans comment), so this is a catalogue the
  // org can read, not a picker it can act on.
  const { data: plans } = useQuery({ queryKey: ["plans"], queryFn: listPlans });

  const limits = Object.entries(subscription?.plan.limits ?? {});

  // Every limit key across all plans, in first-seen order, so the catalogue
  // below renders one aligned column per key even when a plan omits one.
  const limitKeys = Array.from(new Set((plans ?? []).flatMap((p) => Object.keys(p.limits ?? {}))));

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

      {plans && plans.length > 0 && (
        <section className="flex flex-col gap-3">
          <h2 className="label-eyebrow">Available plans</h2>
          <div className="overflow-x-auto rounded-lg border bg-card">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left">
                  <th className="px-4 py-2.5 font-medium">Plan</th>
                  {limitKeys.map((key) => (
                    <th key={key} className="label-eyebrow px-4 py-2.5 font-normal whitespace-nowrap">
                      {humanize(key)}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {plans.map((plan) => {
                  const current = plan.id === subscription?.planId;
                  return (
                    <tr key={plan.id} className={cn("border-b last:border-b-0", current && "bg-muted/40")}>
                      <td className="px-4 py-2.5 whitespace-nowrap">
                        <span className={cn(current && "font-medium")}>{plan.name}</span>
                        {current && <span className="label-eyebrow ml-2 text-signal">current</span>}
                      </td>
                      {limitKeys.map((key) => (
                        <td key={key} className="px-4 py-2.5 font-mono whitespace-nowrap">
                          {key in (plan.limits ?? {}) ? formatLimit(plan.limits[key]) : "—"}
                        </td>
                      ))}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <p className="text-sm text-muted-foreground">
            Plan changes are handled by the Sapanjai team — get in touch and we&apos;ll move your organization
            over.
          </p>
        </section>
      )}
    </div>
  );
}
