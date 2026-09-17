# 12 — Billing and usage metering

How Sapanjai charges for a plan and counts what a plan actually gets used
for. This document records the design — the four decisions, the invariants
that constrain every change to this area, and why the gateway's own metering
counter is a second, independent ledger rather than a reading of
`audit_logs`. The execution script that built it is
[`.claude/plans/archives/2026-09-13-billing-and-usage-metering.md`](../.claude/plans/archives/2026-09-13-billing-and-usage-metering.md),
a throwaway now that it has shipped; this file is the maintained one.

Route shapes, status codes, and error messages live in
[`02-api-contract.md`](02-api-contract.md) (`### Billing` and the `plan_prices`
rows of `### Admin console`), which stays the source of truth for all of
that — this document does not repeat the route table, it explains why the
routes are shaped the way they are.

## 1. What this closes

Before this feature, `plans` was `name` + `limits` jsonb and nothing else:
no price, no currency, no payment integration anywhere in `apps/`.
"Subscription" meant an entitlement tier a superadmin assigned by hand via
`POST /admin/organizations/:orgId/plan`. This closes two gaps at once —
tiers now cost money (Stripe Checkout/Portal, `internal/module/billing`), and
the gateway's own billable unit, the MCP tool call, is now countable
(`usage_events`/`usage_rollups`, written from `internal/module/mcp/service.go`
and folded by `internal/job/usagerollup`).

**What it deliberately does not do.** It does not change how entitlements
are *resolved*. `subscription.Service.EffectiveLimits` is still the single
answer to "what may this org do" — Stripe never becomes the entitlement
store, and `custom_limits` still wins over plan limits, so an admin grant
survives a billing event. See §3, Invariants.

## 2. Decisions

Owner-confirmed 2026-09-13. Do not re-litigate without the owner; each entry
below says what would reopen it.

| # | Decision | Resolution |
| - | -------- | ---------- |
| 1 | Is a tool call a cap or a charge? | **Cap.** "Pro includes N calls/month", refused past it, through the existing `EnforceLimit`/`EffectiveLimits` path. Stripe is told nothing about usage — no usage-based billing vendor entered the stack. |
| 2 | Who may start a checkout? | **`RequirePermission("billing:write")`** on both `POST /billing/checkout` and `POST /billing/portal`; **`billing:read`** for `GET /billing/usage`. Not owner-only, not `RequireOrg`. |
| 3 | Currency and tax posture | **THB only, one active `plan_prices` row per plan per currency/interval, Stripe Tax OFF at launch.** Thai VAT is handled through the selling entity's own registration, not `automatic_tax`. |
| 4 | Free plan mechanics | **Stripe Customer yes, Subscription no — created lazily, on first billing interaction, not at org creation.** |

### Why, in one line each — and what would reopen them

**1 — Cap.** Invariant 3 (§3) forbids billing from ever failing a
`tools/call`, and the call has already succeeded upstream by the time it is
counted, so the counter is *structurally* approximate. A cap tolerates that —
a customer occasionally gets slightly more than they paid for, and nobody
complains. An invoice line does not: undercounting is lost revenue,
overcounting is a dispute and a refund. Accurate usage billing cannot be
built on a counter that is designed to be droppable. Compounding it: at
launch there was no observed usage distribution to price overage against,
and a wrong unit price is hard to walk back once invoiced.
*Reopen when:* the rollups show call volume varies enough between customers
that a flat tier is visibly mispriced — that is the data `usage_rollups`
exists to produce. The metering ledger is a strict prefix of a charge model,
so nothing here would be thrown away if this reopens.

**2 — `billing:write`.** Composes with the RBAC engine already in place,
owner bypass included, so an owner needs no special case, and it lets a
customer delegate billing to a finance person without making them an org
owner — a real B2B requirement `owner`-only gets wrong. The Portal is
arguably the *more* dangerous of the two guarded routes — it can cancel the
subscription, change the payment method, and expose invoices carrying a
billing address — so it never gets a weaker guard than Checkout; both take
the identical action, not a read/write split between them.
*Reopen when:* never, in the permissive direction. `RequireOrg` is
membership-only, and putting a money-changing route on it is exactly the bug
that got the template's `POST /subscription/assign` deleted (see `02-api-contract.md`'s
"There is no tenant-facing plan write route").

**3 — THB only, tax off.** Stripe is generally available in Thailand with
direct registration, so no Atlas entity or local PSP workaround was needed.
Multi-currency multiplies `plan_prices` rows and forces pricing-parity and FX
decisions with no customers yet to justify them. Stripe Tax stays off
because `automatic_tax` calculates nothing *and errors nothing* without an
active tax registration — on is strictly worse than off, since it looks
handled and is not.
*Reopen when:* the first non-Thai customer appears. Cheap by construction:
`plan_prices` already models currency and interval, so adding USD is one row
plus one Stripe Price, not a migration.

> **Open sub-question, still not blocking:** whether the selling entity is a
> VAT-registered Thai company or an individual. It changes the Thai VAT
> answer, and it is the same question that decides whether the commercial
> license in [`LICENSING.md`](../LICENSING.md) is sellable by a person or a
> company. This was explicitly *not* resolved before building shipped —
> resolve it before relying on the current tax posture in production.

**4 — Lazy Customer, no free Subscription.** A zero-price Subscription for
every free org would give a uniform state machine, but it creates a Stripe
object for every signup including spam. Creating nothing forces a null
branch on every read path and pushes Customer creation into the checkout
flow, adding a place to fail mid-payment. Lazy creation keeps
`org_subscriptions.stripe_subscription_id IS NULL` a clean "not paying"
signal, keeps junk out of the Stripe dashboard, and means an upgrade is a
Checkout Session against a Customer that already exists.
*Reopen when:* not expected to; backfilling Customers later is a script.

## 3. Invariants

These are the properties the whole design exists to preserve. A change that
breaks one is wrong even if its tests pass.

1. **Stripe is the billing record. `org_subscriptions.plan_id` is the
   entitlement record.** `POST /billing/webhook` reconciles the first into
   the second. Nothing in `internal/module/subscription` imports a Stripe
   type or makes a Stripe call — `EffectiveLimits` answers from Postgres
   alone, at gateway latency, with Stripe down. Mechanically enforced by the
   import graph, not just a rule: `internal/module/billing/stripe.go` is the
   only file in the repository that imports `github.com/stripe/stripe-go`,
   and everything it hands the reconciler (`webhook.go`) is plain Go —
   uuids, strings, times — so `subscription.Service` could not import a
   Stripe type if it wanted to.
2. **`custom_limits` still wins.** It is the admin override, applied over
   plan limits in `subscription.Service.EffectiveLimits`. A webhook-driven
   plan change must not clear it — `applyEvent`'s one entitlement write goes
   through `AssignPlanTx`, whose `ON CONFLICT` sets `plan_id` and
   `updated_at` and deliberately never touches `custom_limits`.
3. **Billing never blocks the gateway.** A failed usage write, a Stripe
   outage, or an unreachable webhook must never fail a `tools/call`. The
   product's job is proxying data; billing is bookkeeping around it. This is
   why a quota-count failure or an `EnforceLimit` lookup failure in
   `internal/module/mcp/service.go` logs loudly and **lets the call through**
   rather than refusing it — a failure to *determine* the quota is not a
   *genuinely exhausted* quota, and the two must not be conflated. It is
   also why `recordUsage`'s failure path never returns an error, panics, or
   touches the tool result (see §4).
4. **A checkout route is not `RequireOrg`.** `RequireOrg` is
   membership-only, which is exactly why `POST /subscription/assign` was
   deleted from the template in the first place: any `member` could move
   their own org onto any plan and lift `max_members`/`max_roles`/
   `max_connectors` on themselves. A route that changes what an org pays for
   is at least as sensitive, so `POST /billing/checkout` and
   `POST /billing/portal` sit on `RequirePermission("billing:write")`.
5. **`billing` owns no upsert of its own.** It calls
   `subscription.Service.AssignPlanTx`, following the seam
   `internal/module/admin` already established: a narrow interface
   (`billing.planAssigner`) declared by the consumer, injected from
   `server.go`, satisfied by the one real `*subscription.Service`. The
   webhook's single write to `org_subscriptions` outside that seam
   (`ClaimOrgStripeCustomer`) touches exactly one column, `stripe_customer_id`,
   on a row that already exists — it does not create or re-point the
   entitlement.

## 4. The metering problem, stated plainly

`mcp.tool.called` audit rows look like a ready-made usage ledger. **They are
not one**, and this is the single most important finding behind this design.

`auditlog.Service.Record` logs its own error and returns nothing — by
design, per this repo's "audit-log writes are best-effort, never fail the
request" rule (CLAUDE.md). A dropped insert there is therefore silently
unbilled usage with no reconciliation path. `audit_logs` also has no
retention job of its own and would eventually need one as it grows, and
pruning it would destroy billing history; there is also nowhere in that
table to record "this row has been reported to Stripe for period P" without
polluting the audit domain with billing state.

So: a dedicated `usage_events` ledger, written on the hot path right next to
the audit call but with its own error handling and a failure counter, and a
rollup job (`internal/job/usagerollup`) that folds it into `usage_rollups`
and **cross-checks its count against the `audit_logs` count for the same
window**, logging the drift. Two independently-written counters disagreeing
is the detection mechanism. Neither can ever fail the call — invariant 3 —
so the goal was never perfect counting; it is counting that is *wrong
loudly* rather than wrong silently.

### The hot path

`internal/module/mcp/service.go`'s `tools/call` interceptor, in order, once a
call has passed the permission check:

1. **Quota check** (`EnforceLimit` against `max_tool_calls_per_month`,
   resolved the same custom-over-plan way every other limit is). Runs
   *before* the rate limiter, so a call refused for quota never spends
   rate-limit budget either. A count or lookup failure fails **open** (log
   and let the call through) — the opposite direction from the rate
   limiter's own infra-failure branch just below it, and deliberately so:
   the rate limiter protects the upstream (Google's API) from us and fails
   closed; the quota check protects our own revenue bookkeeping and fails
   open, per invariant 3.
2. **Rate limiter** (unrelated to billing; protects the upstream API).
3. **Dispatch**, then — from a `defer`, so it still runs if the handler
   panics, and on a `context.WithoutCancel`'d context, so a client hanging
   up mid-scan doesn't drop the record for the longest-running calls —
   **both** `auditToolCalled` and `recordUsage` run, unconditionally,
   **not filtered by whether the result was an error**. The rollup's drift
   check compares `usage_events`' count against `audit_logs`' for the same
   window, so the two must record the same set of calls or the drift number
   means nothing.

`recordUsage` (`internal/module/mcp/service.go`) is deliberately **not**
best-effort the way `recordAudit`/`auditlog.Service.Record` is: a silently
dropped audit row is acceptable, a silently dropped usage row is unrecorded
revenue. A write failure logs at `error` (one level louder than the audit
path's) and increments an in-process `usageWriteFailures` counter
(`Service.UsageWriteFailures()`), but still never returns an error, panics,
or touches the `tools/call` result. `mcp_key_id` is always left `NULL` on
the row — the authenticated PAT's own id is not reachable at that call site
today (`rbac.Principal` is documented to never carry a credential field), a
pre-existing exclusion this feature chose not to fight rather than an
oversight.

### The rollup job

`internal/job/usagerollup` (`cmd/worker/main.go:131`, the fourth job — see
CLAUDE.md's Background worker bullet) runs on `USAGE_ROLLUP_INTERVAL`
(default 15m) and, in this order every run:

1. **Rolls up** `usage_events` into `usage_rollups` — a single idempotent
   `UPSERT` on the natural key `(organization_id, period_start, tool)`,
   bucketed by UTC calendar month (`date_trunc('month', occurred_at)`,
   normalized to UTC before it ever reaches SQL, since `usage_events` and
   `usage_rollups` are both `timestamp without time zone`).
2. **Cross-checks drift**: compares `usage_events`' count against
   `audit_logs`' `mcp.tool.called` count over the same window and logs the
   result — `warn` when nonzero, `info` when zero. This is a signal to
   watch, not an alarm to page on: both ledgers are written independently
   and neither may ever block a `tools/call`, so a dropped write on either
   side is possible by design.
3. **Prunes** `usage_events` rows past `USAGE_EVENTS_RETENTION` (default
   2160h/90 days — roughly three monthly billing cycles of investigation
   headroom past any plausible job outage), batched by
   `USAGE_ROLLUP_BATCH_SIZE`.

Roll-up always runs before prune, and only ever recomputes a window that
begins at or after the prune's own retention cutoff — never a fixed
lookback alone. The reason is a correctness trap worth stating plainly: the
roll-up recomputes a period's count from scratch and `UPSERT`s it, which is
correct only while every one of that period's raw events is still present.
Recomputing a period the prune has already partially eaten destroys durable
billing history by silently overwriting a complete rollup with a partial
one — not a loud failure, a quiet regression. Clamping the recompute window
against the *actual configured* retention (not a hardcoded month count) is
what makes the job correct for any `USAGE_EVENTS_RETENTION` an operator
sets, rather than merely for the shipped default.

`usage_rollups.reported_at` exists as a column and is deliberately left
`NULL` by this job — decision 1 means nothing here is ever reported to
Stripe. It is the hook a future usage-billing model would use, not
something this feature populates.

### `GET /billing/usage`

Reads only, no writes, no Stripe call (invariant 1 applies here too:
`billing.Service.Usage` answers from Postgres alone). Three independent
reads: the **live** `usage_events` count for the current UTC month (the same
number the gateway's own quota check enforces against — not a
`usage_rollups` sum, which can lag by up to `USAGE_ROLLUP_INTERVAL`), the
resolved `max_tool_calls_per_month` cap via `subscription.Service.GetLimit`,
and a per-tool breakdown from `usage_rollups` (allowed to lag, since it's a
breakdown rather than the enforced number). The UTC-calendar-month boundary
is computed independently in three places — here, in the gateway's quota
check, and in the rollup job — rather than shared through an exported
helper, on purpose: three call sites agreeing by convention forces every one
of them to be updated, deliberately, if the boundary ever needs to move,
rather than one shared import edge nobody notices connects them.

## 5. Stripe integration notes

`internal/module/billing/stripe.go` is the only file that imports
`github.com/stripe/stripe-go`. What it does, and the traps it exists to
avoid re-introducing:

- **Never `PaymentMethodTypes`.** Omitting it enables dynamic payment
  methods configured from the Stripe Dashboard. Hardcoding `["card"]` would
  silently remove PromptPay, which for Thai customers is not a rounding
  error.
- **`AutomaticTax` is never set.** Decision 3: THB only, tax off at launch,
  Thai VAT handled through the selling entity's own registration.
- **Signature verification runs on the raw body.** `POST /billing/webhook`
  reads and retains the exact bytes Stripe sent before anything else can
  touch the request — no `c.Bind`, no `httpx.BindAndValidate` — because a
  consumed body leaves an empty reader and the HMAC comes back over
  nothing. This also depends on the global Echo middleware stack staying
  body-blind (Recover, RequestID, the request logger all are; nothing that
  reads a body may be added globally).
- **Idempotency and out-of-order delivery** are two different problems with
  two different fixes. `stripe_events` (event id as primary key, claimed
  inside the same transaction as the state change it guards) stops an
  *exact* replay. It does not stop a *stale* `customer.subscription.updated`
  arriving after a newer one — a different event id entirely — which is
  what `org_subscriptions.stripe_event_at` (migration `00016`) is for: the
  Stripe-clock `created` timestamp of the newest subscription-state event
  applied to that row, checked before applying the next one. Strictly older
  is rejected; equal is applied (Stripe's `created` is whole seconds, so a
  tie is genuinely ambiguous and last-writer-wins is the only answer
  available without a monotonic version Stripe doesn't publish).
- **A restricted key (`rk_...`)**, scoped to only Checkout/Billing/Customer
  write — never an account-wide `sk_...` — is what `STRIPE_SECRET_KEY`
  should hold. `config.Load` rejects a `pk_...` (publishable key pasted into
  the wrong slot) at boot with a specific error, and accepts either `rk_`
  or `sk_` since a deployment's operator, not the code, ultimately enforces
  the restriction.
- **An instantiated `*stripe.Client`**, never the deprecated global
  `stripe.Key = …` assignment.
- **Prices, never the deprecated `plan` object.** A `plan_prices` row (never
  an inline `PriceData`) is what a Checkout Session's line item references,
  so every invoice line item reads the plan's own Stripe Product name.
- **`IntegrationIdentifier`** is sent on every Checkout Session
  (`sapanjai_billing_fepumlri` — fixed, not regenerated per request, so
  flows stay comparable to each other over time in Stripe's own Dashboard
  reporting).
- **`IgnoreAPIVersionMismatch` is never set** on webhook verification. An
  endpoint configured against a different API-version release train sends
  object shapes this SDK version may deserialize wrongly (several fields
  this code reads have moved between releases — `current_period_end` off
  the Subscription and onto its line item, the Invoice's subscription link
  into `invoice.parent`). Failing loudly on the first mismatched delivery
  beats silently reconciling zero values into `org_subscriptions`.

## 6. What is deliberately not built

- **No usage-based billing vendor** (Metronome or similar). Decision 1 — a
  tool call is a cap, not a charge — means Stripe is never told about usage
  at all, and nothing here should start reporting to it. If a future change
  starts sending `usage_rollups` data to Stripe, that is scope drift against
  this design, not a natural extension of it.
- **No write path from `usage_rollups` to Stripe.** `reported_at` exists as
  a column precisely so a future metered-billing model has somewhere to
  record it, but nothing populates it today.
- **No invoice view.** The Stripe Customer Portal (`POST /billing/portal`)
  is where a customer sees invoice history; nothing here builds a second
  one.
- **No ~80%-of-quota warning email.** A customer only discovers a cap by
  hitting it (`QuotaExceeded()`, a clean MCP `IsError` result, never a panic
  or a raw Go error — the same edge case CLAUDE.md's MCP gateway testing
  section names). A warning job is a natural fourth *kind* of worker
  behavior — the usage-rollup job already computes the numbers a warning
  would need — but it is out of scope here; nothing schedules it.
- **No per-org override of currency, interval, or tax posture** beyond what
  `plan_prices` already models as rows. THB-only and tax-off are global
  postures (decision 3), not per-organization settings.

## 7. Operating it

**"A customer says their usage looks wrong."** `GET /billing/usage`'s
`callCount` is the live number the gateway actually enforces against; ask
which number they're looking at before assuming a bug — the frontend's
per-tool breakdown reads `usage_rollups`, which can lag the live count by up
to `USAGE_ROLLUP_INTERVAL`.

**"Is metering dropping calls?"** Check the worker's structlog for `usage
rollup: usage_events/audit_logs drift detected` (warn-level, emitted only
when the two counters disagree — a clean run logs `usage rollup: no drift
between usage_events and audit_logs` at info instead) and the `usageWriteFailures`
in-process counter surfaced by `internal/module/mcp.Service`. Some drift is
expected by design (§4) — a growing, not merely nonzero, drift is the signal
worth chasing.

**"Checkout/portal/webhook returns 501."** `BILLING_NOT_CONFIGURED` means
`STRIPE_SECRET_KEY` (checkout/portal) or `STRIPE_WEBHOOK_SECRET` (webhook) is
unset — the intended local-dev default, and each half degrades
independently (`config.Config.BillingEnabled`/`WebhookEnabled`).

**Config vars** are listed in CLAUDE.md's `## Environment` section and in
`.env.example` — not in [`02-api-contract.md`](02-api-contract.md), whose
own environment section covers only the vars that are part of the API
contract and defers the rest to CLAUDE.md: `STRIPE_SECRET_KEY`,
`STRIPE_WEBHOOK_SECRET`, `STRIPE_WEBHOOK_IP_ALLOWLIST` (API-side, all
optional), `USAGE_ROLLUP_INTERVAL`, `USAGE_ROLLUP_BATCH_SIZE`,
`USAGE_EVENTS_RETENTION` (worker-side). Note the deliberate divergence from
the `RESEND_API_KEY` precedent (`docs/10-transactional-email.md`): the
Stripe secret key is held by the **API**, not only the worker, because
starting a checkout is request-driven and the webhook endpoint must be
public — there is no way to keep it off the internet-facing service the way
the Resend key is kept off it. The restricted key is what bounds the blast
radius instead.
