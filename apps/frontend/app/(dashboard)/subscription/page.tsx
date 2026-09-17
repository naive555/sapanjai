"use client";

import { Suspense, useEffect, useRef } from "react";
import { useSearchParams } from "next/navigation";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { Callout } from "@/components/callout";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api/client";
import {
  getSubscription,
  getUsage,
  listPlans,
  openBillingPortal,
  startCheckout,
  type PlanPriceResponse,
} from "@/lib/api/endpoints";
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

// unitAmount is Stripe's minor-unit integer (satang, for THB) — divide by
// 100 before formatting, same as every other Stripe amount.
function formatPrice(price: PlanPriceResponse): string {
  const amount = price.unitAmount / 100;
  const formatted = new Intl.NumberFormat(undefined, {
    style: "currency",
    currency: price.currency.toUpperCase(),
  }).format(amount);
  return `${formatted} / ${price.interval === "month" ? "mo" : "yr"}`;
}

function billingErrorMessage(err: unknown, fallback: string): string {
  return err instanceof ApiError ? err.message : fallback;
}

// Reads ?checkout=success|cancelled (billing/service.go's SuccessURL/
// CancelURL, billing/dto.go's frontendURL) and invalidates the queries a
// completed checkout affects. Isolated behind its own component + Suspense
// boundary because useSearchParams forces client-side rendering of
// everything below it — see node_modules/next/dist/docs' useSearchParams
// reference and the same pattern already used by
// app/(auth)/verify-email/page.tsx and app/(auth)/reset-password/page.tsx.
function CheckoutStatusBanner() {
  const status = useSearchParams().get("checkout");
  const activeOrgId = useActiveOrgId();
  const queryClient = useQueryClient();

  // Invalidate at most once per landing on this page with the param
  // present — React StrictMode double-invokes effects in dev, and
  // there's nothing wrong with invalidating twice, but there's no reason
  // to either.
  const invalidated = useRef(false);
  useEffect(() => {
    if (status !== "success" || invalidated.current) return;
    invalidated.current = true;
    void queryClient.invalidateQueries({ queryKey: ["subscription", activeOrgId] });
    void queryClient.invalidateQueries({ queryKey: ["billing-usage", activeOrgId] });
  }, [status, activeOrgId, queryClient]);

  if (status === "success") {
    return (
      <Callout title="Checkout complete">
        Stripe has the payment. The plan below updates once its webhook lands — usually a few
        seconds — not the moment this page loads, so it may still show the old plan for a
        moment.
      </Callout>
    );
  }
  if (status === "cancelled") {
    return <Callout title="Checkout cancelled">No changes were made to this organization&apos;s plan.</Callout>;
  }
  return null;
}

export default function SubscriptionPage() {
  const activeOrgId = useActiveOrgId();

  const { data: subscription, isLoading } = useQuery({
    queryKey: ["subscription", activeOrgId],
    queryFn: getSubscription,
    enabled: activeOrgId !== null,
  });

  // Plans are global, not org-scoped — no activeOrgId in the query key.
  const { data: plans } = useQuery({ queryKey: ["plans"], queryFn: listPlans });

  // billing:read-gated, unlike the two queries above — a member with no
  // grant at all gets a deterministic 403, so retry: false the same way
  // the mcp-keys page treats its own permission-gated read.
  const {
    data: usage,
    isError: usageIsError,
    error: usageError,
  } = useQuery({
    queryKey: ["billing-usage", activeOrgId],
    queryFn: getUsage,
    enabled: activeOrgId !== null,
    retry: false,
  });

  const checkoutMutation = useMutation({
    mutationFn: startCheckout,
    onSuccess: (data) => {
      window.location.assign(data.url);
    },
    onError: (err) => {
      toast.error(billingErrorMessage(err, "Couldn't start checkout."));
    },
  });

  const portalMutation = useMutation({
    mutationFn: openBillingPortal,
    onSuccess: (data) => {
      window.location.assign(data.url);
    },
    onError: (err) => {
      toast.error(billingErrorMessage(err, "Couldn't open the billing portal."));
    },
  });

  const limits = Object.entries(subscription?.plan.limits ?? {});

  // Every limit key across all plans, in first-seen order, so the catalogue
  // below renders one aligned column per key even when a plan omits one.
  const limitKeys = Array.from(new Set((plans ?? []).flatMap((p) => Object.keys(p.limits ?? {}))));

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader title="subscription" description="The plan this organization runs on, its limits, and its usage.">
        {subscription?.hasActiveSubscription && (
          <Button
            size="sm"
            variant="outline"
            disabled={portalMutation.isPending}
            onClick={() => portalMutation.mutate()}
          >
            {portalMutation.isPending ? "Opening…" : "Manage billing"}
          </Button>
        )}
      </PageHeader>

      <Suspense fallback={null}>
        <CheckoutStatusBanner />
      </Suspense>

      <section className="rounded-lg border bg-card">
        <div className="flex flex-wrap items-baseline justify-between gap-x-6 gap-y-2 border-b px-5 py-4">
          <div className="flex items-baseline gap-3">
            <span className="label-eyebrow">Current plan</span>
            <h2 className="font-display text-lg leading-none">
              {isLoading ? "…" : (subscription?.plan.name ?? "none")}
            </h2>
            {subscription?.status && (
              <span className="label-eyebrow text-muted-foreground">
                {subscription.status.replace(/_/g, " ")}
                {subscription.cancelAtPeriodEnd && " · cancels at period end"}
              </span>
            )}
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

      <section className="rounded-lg border bg-card">
        <div className="border-b px-5 py-4">
          <span className="label-eyebrow">Usage this month</span>
        </div>

        {usageIsError ? (
          <p className="px-5 py-4 text-sm text-muted-foreground">
            {usageError instanceof ApiError && usageError.status === 403
              ? "You don't have permission to view usage in this organization."
              : "Failed to load usage."}
          </p>
        ) : usage ? (
          <>
            <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 px-5 py-4">
              <span className="font-mono text-xl leading-none text-foreground">{usage.callCount}</span>
              <span className="text-sm text-muted-foreground">
                {usage.limit === null ? "calls this period — unlimited" : `/ ${usage.limit} calls this period`}
              </span>
            </div>
            {usage.byTool.length > 0 && (
              <div className="flex flex-wrap gap-x-6 gap-y-2 border-t px-5 py-4">
                {usage.byTool.map((t) => (
                  <div key={t.tool} className="flex items-baseline gap-2">
                    <span className="font-mono text-xs text-muted-foreground">{t.tool}</span>
                    <span className="font-mono text-sm text-foreground">{t.callCount}</span>
                  </div>
                ))}
              </div>
            )}
            <p className="border-t px-5 py-2.5 text-xs text-muted-foreground">
              The total above is live; the per-tool breakdown can lag by a few minutes.
            </p>
          </>
        ) : (
          <p className="px-5 py-4 text-sm text-muted-foreground">Loading…</p>
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
                  <th className="px-4 py-2.5 font-medium">{""}</th>
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
                      <td className="px-4 py-2.5 text-right whitespace-nowrap">
                        {current ? (
                          <span className="text-xs text-muted-foreground">current plan</span>
                        ) : plan.prices.length > 0 ? (
                          <div className="flex flex-wrap justify-end gap-1.5">
                            {plan.prices.map((price) => {
                              const pending =
                                checkoutMutation.isPending &&
                                checkoutMutation.variables?.planId === plan.id &&
                                checkoutMutation.variables?.interval === price.interval;
                              return (
                                <Button
                                  key={price.interval + price.currency}
                                  size="xs"
                                  variant="outline"
                                  disabled={checkoutMutation.isPending}
                                  onClick={() =>
                                    checkoutMutation.mutate({ planId: plan.id, interval: price.interval })
                                  }
                                >
                                  {pending ? "Starting…" : `Subscribe · ${formatPrice(price)}`}
                                </Button>
                              );
                            })}
                          </div>
                        ) : (
                          <span className="text-xs text-muted-foreground">not purchasable</span>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  );
}
