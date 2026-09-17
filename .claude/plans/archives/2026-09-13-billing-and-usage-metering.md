# Billing + usage metering — Implementation Plan

> **Status: ✅ complete — planned 2026-09-13, shipped 2026-09-17. 10 / 10 steps.**
> Archived; [`docs/12-billing-and-metering.md`](../../docs/12-billing-and-metering.md)
> is the maintained state from here on — read that, not this, for how billing
> and metering actually behave.
> **All four decisions confirmed by the owner 2026-09-13**; none are blocking
> (§Decisions). The one consequential resolution: a tool call is a **cap, not a
> charge**, so no usage-based billing vendor enters the stack and Stripe stays a
> flat per-tier subscription. One sub-question stays open and is *not* a
> blocker — whether the selling entity is a VAT-registered Thai company or an
> individual (§Decisions, note under 3).
>
> **What this closes.** `plans` today is `name` + `limits` jsonb and nothing
> else (`migrations/00005`); there is no price, no currency, no interval, and no
> payment integration anywhere in `apps/`. "Subscription" currently means
> entitlement tiers a superadmin assigns by hand via
> `POST /admin/organizations/:orgId/plan`. This plan makes the tiers cost money
> and makes the gateway's own billable unit — the MCP tool call — countable.
>
> **What it deliberately does not do.** It does not change how entitlements are
> *resolved*. `subscription.Service.EffectiveLimits` stays the single answer to
> "what may this org do", Stripe never becomes the entitlement store, and
> `custom_limits` keeps winning over plan limits so an admin grant survives a
> billing event. See §Invariants.
>
> **Reference:** Stripe Billing + Checkout, API version `2026-08-26.dahlia`,
> Go SDK `v86.4.0`. Guidance drawn from the `stripe:stripe-best-practices`
> skill on 2026-09-13; re-read it before writing step 6, since its routing
> advice moves.
>
> Target executor: **Sonnet for steps 1-5 and 9-10** (mechanical, the codebase
> already has the pattern). **Opus for steps 6-8** — the Stripe surface has
> several traps that silently produce a working-looking integration that
> undercharges or double-charges, and they are enumerated in the step detail
> rather than left to be rediscovered.

---

## Decisions — ✅ all four confirmed 2026-09-13

Confirmed by the owner; no longer blocking.

| # | Decision | Resolution | Gates |
| - | -------- | ---------- | ----- |
| 1 | Is a tool call a cap or a charge? | **Cap.** "Pro includes N calls/month", refused past it, via the existing `EnforceLimit` / `EffectiveLimits` path. Stripe stays a flat per-tier subscription and is told nothing about usage. **Metronome is out of scope.** | 3, 4, 5, 6 |
| 2 | Who may start a checkout? | **`RequirePermission("billing:write")`** on both `/billing/checkout` and `/billing/portal`; **`billing:read`** for usage and invoice views. Not owner-only, not `RequireOrg`. | 6, 9 |
| 3 | Currency and tax posture | **THB only, one Price per plan, Stripe Tax OFF at launch.** Thai VAT handled through the entity's own registration, not `automatic_tax`. | 1, 6 |
| 4 | Free plan mechanics | **Stripe Customer yes, Subscription no — and the Customer is created lazily, on first billing interaction, not at org creation.** | 1, 7 |

### Why, in one line each — and what would reopen them

**1 — Cap.** Not a "start simple" call. Invariant 3 forbids billing from failing
a `tools/call`, and the upstream request has already succeeded by the time the
call is counted, so the counter is *structurally* approximate. A cap tolerates
that (a customer occasionally gets slightly more than they paid for; nobody
complains). An invoice line does not — undercounting is lost revenue,
overcounting is a dispute and a refund. Accurate usage billing cannot be built
on a counter that is designed to be droppable. Compounding it: with zero
customers there is no observed usage distribution to price overage against, and
a wrong unit price is hard to walk back once invoiced.
*Reopen when:* the rollups show call volume varies enough between customers
that a flat tier is visibly mispriced — that is the data this plan's steps 2-4
exist to produce. Steps 2-4 are a strict prefix of the charge model, so nothing
built here is thrown away if it reopens.

**2 — `billing:write`.** Composes with the RBAC engine already present, owner
bypass included, so an owner needs no special case; the frontend already
renders `billing:read` and `billing:*` as example tokens
(`apps/frontend/app/(dashboard)/roles/page.tsx:80,84`); and it lets a customer
delegate billing to a finance person without making them an org owner, which is
a real B2B requirement that owner-only gets wrong. Note the Portal is arguably
the *more* dangerous of the two routes — it can cancel the subscription, change
the payment method, and expose invoices carrying a billing address — so it gets
the same guard as checkout, never a weaker one.
*Reopen when:* never, in the permissive direction. `RequireOrg` on a
money-changing route is the exact bug that got `POST /subscription/assign`
deleted.

**3 — THB only, tax off.** Stripe is **generally available in Thailand** with
direct registration (verified against `stripe.com/global`, 2026-09-13), so no
Atlas entity or local PSP workaround is needed. Multi-currency multiplies Price
objects and forces pricing-parity and FX decisions with no customers to justify
them. Stripe Tax stays off because it calculates nothing *and errors nothing*
without an active registration — on is strictly worse than off, since it looks
handled and is not.
*Reopen when:* the first non-Thai customer appears. Cheap by construction —
`plan_prices` (step 1) already models currency and interval, so adding USD is
one row plus one Stripe Price, not a migration.
> ⚠️ **Open sub-question, not blocking:** whether the selling entity is a
> VAT-registered Thai company or an individual. It changes the Thai VAT answer,
> and it is the same question that decides whether the commercial license in
> [`LICENSING.md`](../../LICENSING.md) is sellable by a person or a company.
> Resolve before step 6 ships to production, not before it is written.

**4 — Lazy Customer, no free Subscription.** A zero-price Subscription for every
free org would give a uniform state machine, but it creates Stripe objects for
every signup including spam. Creating nothing forces a null branch on every read
path and pushes Customer creation into the checkout flow, adding a place to fail
mid-payment. Lazy creation keeps `stripe_subscription_id IS NULL` as a clean
"not paying" signal, keeps junk out of the Stripe dashboard, and means an
upgrade is a Checkout Session against a Customer that already exists.
*Reopen when:* never expected to; backfilling Customers later is a script.

---

## Invariants

These are the properties the whole design exists to preserve. A step that
breaks one is wrong even if its tests pass.

1. **Stripe is the billing record. `org_subscriptions.plan_id` is the
   entitlement record.** The webhook reconciles the first into the second.
   Nothing in `subscription.Service` ever imports a Stripe type or makes a
   Stripe call — `EffectiveLimits` must answer from Postgres alone, at gateway
   latency, with Stripe down.
2. **`custom_limits` still wins.** It is the admin override, applied over plan
   limits in `subscription/service.go:EffectiveLimits`. A plan change from a
   webhook must not clear it.
3. **Billing never blocks the gateway.** A failed usage write, a Stripe
   outage, or an unreachable webhook must never fail a `tools/call`. The
   product's job is proxying data; billing is bookkeeping around it.
4. **A checkout route is not `RequireOrg`.** `RequireOrg` is membership-only.
   This is exactly why `POST /subscription/assign` was deleted from the
   template — any `member` could move their own org onto any plan and lift
   `max_members`/`max_roles`/`max_connectors` on themselves. A route that
   changes what an org pays for is at least as sensitive.
5. **`billing` owns no upsert of its own.** It calls
   `subscription.Service.AssignPlan`, following the seam
   `internal/module/admin` already established: admin imports nothing from
   `internal/module/`, declares a narrow `subscriptionResolver` interface
   (`admin/service.go:167`) whose `AssignPlan` member exists precisely so the
   upsert is not reimplemented, and takes it injected from `server.go`.

---

## The metering problem, stated plainly

`mcp.tool.called` audit rows look like a ready-made usage ledger. **They are
not one**, and this is the single most important finding behind this plan.

`auditlog.Service.Record` (`internal/module/auditlog/service.go:113-123`) logs
its error and returns nothing — by design, per CLAUDE.md's "audit-log writes
are best-effort, never fail the request" rule. A dropped insert is therefore
**silently unbilled usage with no reconciliation path**. `audit_logs` also has
no retention job today but will need one as it grows, and pruning it would
destroy billing history; and there is nowhere to record "this row has been
reported to Stripe for period P" without polluting the audit domain with
billing state.

So: a dedicated `usage_events` ledger (step 2), written on the hot path next
to the audit call but with its own error handling and a failure counter (step
3), and a rollup job that **cross-checks its counts against the `audit_logs`
count for the same window and alerts on drift** (step 4). Two independent
counters disagreeing is the detection mechanism. Neither can fail the call —
invariant 3 — so the goal is not perfect counting, it is counting that is
*wrong loudly* rather than wrong silently.

---

## Steps

- [x] **1. Schema: pricing + Stripe linkage** — migration `00013`, additive.
      `plans` gains `stripe_product_id`, `is_public`, `sort_order`. New
      `plan_prices` (plan_id, `stripe_price_id`, `unit_amount`, `currency`,
      `interval`, `active`) rather than price columns on `plans`, because the
      Stripe skill is explicit that each plan is its own **Product** and
      monthly/annual/multi-currency are **Prices under it** — putting tiers on
      one Product makes every invoice line item read the same name.
      `org_subscriptions` gains `stripe_customer_id`, `stripe_subscription_id`,
      `status`, `current_period_end`, `cancel_at_period_end`. New
      `stripe_events` (event id PK, `received_at`, `type`) for webhook
      idempotency. `make sqlc`. **Do not touch `limits` or `custom_limits`.**
- [x] **2. Schema: usage ledger** — migration `00014`. `usage_events`
      (org, connector, mcp_key, tool, `occurred_at`, `quantity`) and
      `usage_rollups` (org, period start/end, tool, count, `reported_at`).
      Index `usage_events (organization_id, occurred_at)` — the rollup's only
      access pattern. Decide retention here, not later.
- [x] **3. Record usage on the hot path** — `internal/module/mcp/service.go`,
      immediately alongside `auditToolCalled` (~line 285). Same principal, same
      connector, no tool arguments — the existing "column names are recorded;
      the values filtered on are not" rule applies unchanged. Failure logs at
      **error** and bumps a counter; the call still succeeds.
- [x] **4. `internal/job/usagerollup`** — a `worker.Job` registered in
      `cmd/worker/main.go` beside the existing three, folding `usage_events`
      into `usage_rollups`, pruning rolled-up events past retention, and
      emitting the `audit_logs` cross-check drift number. Reuses the Redis
      lock and per-run timeout for free.
- [x] **5. Enforce a call quota** *(decision 1: this is the billing model,
      not an option)* — add
      `max_tool_calls_per_month` to the seeded plan limits
      (`cmd/seed/main.go:23-25`) and call the existing
      `subscription.Service.EnforceLimit` from the gateway before dispatch,
      next to the existing rate-limit check. Note `-1` already means unlimited
      and `EnforceLimit` already treats a missing subscription as unlimited —
      no new semantics.
- [x] **6. `internal/module/billing`** — handler → service → sqlc, the standard
      shape. `POST /billing/checkout` (Checkout Session, `mode: "subscription"`)
      and `POST /billing/portal` (Customer Portal, which buys upgrade,
      downgrade, cancel, and payment-method management without building any of
      it). Both guarded by `RequirePermission("billing:write")` per decision 2.
      The org's Stripe Customer is created **here, lazily on first billing
      interaction** (decision 4) — not at org creation, and never a
      zero-price Subscription for `free`. One THB Price per plan; no
      `automatic_tax` (decision 3). See §Step detail for the traps.
- [x] **7. `POST /billing/webhook`** — outside `RequireAuth`/`RequireOrg`
      (Stripe presents no JWT), signature-verified, idempotent via
      `stripe_events`, reconciling `customer.subscription.*`, `invoice.paid`,
      and `invoice.payment_failed` into `org_subscriptions` by calling
      `subscription.Service.AssignPlan`. `stripe_subscription_id IS NULL`
      is the canonical "not paying" signal (decision 4), so a downgrade to
      `free` clears it rather than pointing at a cancelled Stripe object.
      See §Step detail.
- [x] **8. Admin surface** — extend the existing superadmin-only plan CRUD in
      `internal/module/admin` to cover `plan_prices` and the new `plans`
      columns. The admin non-goal test
      (`internal/server/admin_integration_test.go`) must grow to assert no
      admin response carries a Stripe secret or a customer's payment details.
- [x] **9. Frontend** — `/subscription` gains a plan picker that POSTs to
      `/billing/checkout` and redirects, a "Manage billing" button hitting
      `/billing/portal`, and a usage meter reading the rollups. Hosted Checkout
      is a **redirect**, so Stripe.js is not loaded and no CSP work is needed —
      keep it that way.
- [x] **10. Docs** — new `docs/12-billing-and-metering.md` (this plan's
      decisions, minus the checklist), the new routes into
      [`docs/02-api-contract.md`](../../docs/02-api-contract.md) **in the same
      change that adds them**, plus CLAUDE.md, `.env.example`, and the README
      status table.

---

## Step detail — the Stripe traps (steps 6 and 7)

Each of these produces an integration that looks like it works.

- **Never pass `payment_method_types`.** Omitting it enables dynamic payment
  methods configured from the Dashboard. Hardcoding `["card"]` silently locks
  out PromptPay and every other local method — which for Thai customers is not
  a rounding error.
- **Webhooks are not optional and not a follow-up.** Renewals, failed
  payments, dunning, and cancellations all happen asynchronously, long after
  Checkout returns. An integration that only reads the success page cannot see
  any of them. Handle `checkout.session.completed`,
  `customer.subscription.created/updated/deleted`, `invoice.paid`,
  `invoice.payment_failed`.
- **Verify the signature against the raw body.** In Echo the trap is
  concrete: any binding or middleware that consumes the request body first
  makes verification fail. Read and retain the raw bytes before anything else
  touches them.
- **Stripe retries and can deliver out of order.** `stripe_events` gives
  idempotency; additionally ignore an event whose subscription state is older
  than what is already stored, rather than assuming arrival order.
- **Use a restricted key (`rk_`), not `sk_`**, with only the Checkout,
  Billing, and Customer permissions it needs. Note the divergence this forces
  from the `RESEND_API_KEY` precedent, where the secret is read *only* by
  `cmd/worker` to keep it off the internet-facing service: Checkout creation is
  request-driven and the webhook endpoint must be public, so the **API** holds
  this one. The RAK is what bounds the blast radius instead. Also allowlist
  Stripe's published IPs on the webhook route — `ADMIN_IP_ALLOWLIST` is the
  existing pattern to copy.
- **Instantiate a `StripeClient`**; the global `stripe.Key = …` pattern is
  deprecated across all current SDKs.
- **Don't use the deprecated `plan` object** — Prices only.
- **Pass `integration_identifier`** on Checkout Session creation (supported
  from `2026-03-25.dahlia`) with an 8-random-letter suffix, so flows are
  comparable in the Dashboard later.
- **Stripe Tax collects nothing, and errors nothing, without an active
  registration.** Setting `automatic_tax: {enabled: true}` and assuming VAT is
  handled is the most common Stripe Tax mistake. **Decision 3 resolved this:
  leave it off.** Thai VAT rides the entity's own registration. Do not switch it
  on later without confirming an active registration exists first — on without
  one is strictly worse than off, because it looks handled and collects
  nothing.

---

## Testing expectations

Per CLAUDE.md, and beyond the usual per-service unit tests with mocked infra:

- **Webhook**: replayed event is a no-op; out-of-order `subscription.updated`
  does not regress state; bad signature → 400 and no state change; unknown
  event type → 200 and ignored.
- **Entitlement**: a webhook-driven plan change does **not** clear
  `custom_limits`; `EffectiveLimits` still resolves with Stripe unreachable.
- **Isolation**: one org's checkout session, portal link, or usage rows are
  unreachable from another org's session — the tenant-isolation class named
  first in [`SECURITY.md`](../../SECURITY.md).
- **Guards**: a plain `member` cannot start a checkout (invariant 4). This is
  the regression test for the deleted `POST /subscription/assign`.
- **Metering**: a usage-write failure does not fail the `tools/call`
  (invariant 3); the rollup is idempotent across two runs over the same
  window; the audit cross-check reports drift when rows are deleted underneath
  it.

---

## Risks

- **~~Metronome as a second billing platform~~ — retired by decision 1.** The
  cap model needs no usage-billing vendor at all. Guard against it creeping
  back: if a step starts reporting usage *to Stripe*, that is scope drift, not
  progress.
- **A cap is a customer-visible refusal.** Call 50,001 fails, and it fails
  inside somebody's agent mid-task. The error must say what happened and what
  to do about it — `apperror.LimitExceeded` reaching an MCP client as a clean
  `IsError` result, never a panic or a raw Go error, per CLAUDE.md's gateway
  edge cases. Worth a friendly warning email at ~80% of quota, which is a
  natural fourth job for the worker but is **not** in this plan's ten steps.
- **`usage_events` write volume** is one row per tool call, on the gateway's
  latency path. If that proves hot, the mitigation is a buffered writer in the
  worker, not dropping the ledger — but measure before building it.
- **Undercounting is inherent**, not a bug to be fixed: the upstream call has
  already happened by the time the call is counted, and invariant 3 forbids
  failing it. The drift check makes it visible; it cannot make it zero.
- **Entity type is still unresolved** (decision 3's open sub-question). Stripe
  availability is no longer a risk — Thailand is generally available with
  direct registration, verified 2026-09-13 — but whether the seller is a
  VAT-registered company or an individual decides the Thai VAT treatment, and
  the same answer decides who can sign the commercial license. Resolve before
  step 6 reaches production.
- **This plan touches `internal/module/mcp/service.go`**, the file the sheets
  adapter plan warned is easy to destabilize. Step 3 should be a
  behaviour-preserving addition; if it grows past ~20 lines there, that is a
  signal to move the work behind an injected interface instead.

---

## When this is done

Archive this file to `.claude/plans/archives/` and let
`docs/12-billing-and-metering.md` carry the maintained state — the same
convention every completed plan in that directory followed.
