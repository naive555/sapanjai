# MCP Gateway Core — the connector-agnostic boundary

Companion to [`05-mcp-gateway.md`](05-mcp-gateway.md), which describes the
gateway as built. This document draws a line *through* it: which parts belong
to every connector, and which belong to the adapter that happens to be first.
It exists because the `google_sheets` adapter shipped first and the next
connector is undecided — so the boundary needs stating before Google-shaped
assumptions harden into the core.

It records assumptions and decisions, not tasks. The current work item is
[`.claude/plans/2026-08-23-gateway-core.md`](../.claude/plans/2026-08-23-gateway-core.md).

---

## 1. The boundary test

Every candidate piece of gateway work gets one question:

> **If the first connector had been FlowAccount instead of Google Sheets,
> would this be needed unchanged?**

Yes → it is core, and it should be built and tested against a trivial tool that
touches no upstream API. No → it belongs to an adapter, and building it early
means guessing at a shape only the second connector can confirm.

The test is deliberately about *unchanged*, not *needed*. FlowAccount would
also need credential storage and a health check — but not OAuth consent, not a
spreadsheet allowlist, not a per-minute quota tuned to Sheets' read limits.
Anything that survives only in modified form is adapter work wearing a core
costume.

## 2. What the core is

Three capabilities, all of them live today (see `05-mcp-gateway.md` §"What this
repo added" for the as-built detail):

1. **A long-lived client credential.** The 15-minute JWT access token cannot
   serve an MCP client that stays connected for weeks. `mcp_api_keys` +
   `internal/module/mcpkey` mint org-scoped Personal Access Tokens; the client
   presents one as `Authorization: Bearer sk_live_…`. Nothing about this is
   connector-shaped — the key authenticates a *caller*, and the connector is
   named in the URL.

2. **A permission matcher callable outside middleware.** Every MCP tool call
   lands on the same route, so `RequirePermission` — which gates a *route* —
   cannot gate an individual tool. `rbac.ActionMatches` and `rbac.Principal`
   expose the same `*` > exact > `resource:*` semantics as a plain function, so
   the gateway can filter a whole tool catalog against one resolved grant.
   `Service.HasPermission` was rewired onto that function rather than keeping a
   second copy: one implementation, two callers.

3. **The handshake and its two enforcement layers.** `initialize` →
   `tools/list` → `tools/call` over Streamable HTTP, with construction-time
   filtering (a denied tool never appears in `tools/list`) *and* a call-time
   re-check (a permission change takes effect on the very next call, not the
   next reconnect). Proven against `sapanjai_describe_connector`, which reads
   the connector row and nothing else.

The third is the one that mattered most to retire early: every adapter is built
on top of it, and a flaw there is a flaw in all of them at once.

## 3. What the core is deliberately not

Excluded by §1's test. Several of these are already built for `google_sheets`;
"excluded" means *not generalized into the core*, not *not built*.

| Excluded | Why |
|---|---|
| Google OAuth consent flow, token refresh, credential paste UI | Google-specific. FlowAccount authenticates with an API key; nothing carries over. Lives in `internal/adapter/googlesheets/oauth.go` — do not lift it into the core speculatively. |
| `sheets_*` / `drive_*` tools, the spreadsheet/folder allowlist, sample-row and scan budgets | Per-adapter. The allowlist is a `google_sheets` config field, not a connector-core concept; the next connector's equivalent may not be a list of ids at all. |
| Google API quota handling | Upstream-specific. |
| Write and action tools, and any human-in-the-loop approval mechanism | No write tool exists for any connector (`07-sheets-adapter-plan.md` §3). Approval design depends on what the writes *are* — an approval UX for "append a spreadsheet row" and one for "issue an invoice" are not the same product. |
| Per-connector rate limiting | Belongs with the adapter that needs it, and is already built (`mcp:ratelimit:<connectorId>`) driven by `06-sheets-adapter.md` §6's quota math. A FlowAccount adapter would want different units and a different charge point. |
| HTTP+SSE transport / a `/sse` route | See §5. |
| Plan limits (`max_mcp_calls_per_month`, `max_connectors`) | Judged not load-bearing for the MVP in `07-sheets-adapter-plan.md` §3; the per-connector limiter covers the quota-exhaustion risk that actually threatens it. Unchanged here. |
| PAT expiry sweep job, PAT rotation endpoint, Redis-cached key lookup | Housekeeping and optimization, not capability. Expired and revoked keys are already rejected at auth time; the row lingering costs nothing. Revisit under measured load (`07-sheets-adapter-plan.md` §1 Decision 1). |

## 4. Key scoping: the assumptions behind `scopes`

`mcp_api_keys.scopes` is nullable `text[]`, and `rbac.Principal.Narrow`
intersects it with the caller's live grant. Three decisions sit underneath
that, none of them obvious:

**A key narrows, never widens — and the grant is re-resolved per request.**
Scopes are not a permission set; they are a *ceiling* applied on top of one.
The alternative — snapshotting the creator's permissions onto the key at mint
time — would mean revoking someone's role leaves their keys still working. The
cost is one indexed read plus one grant resolution per gateway request, paid
deliberately.

**`Narrow` clears `Role`, and that line is load-bearing.** A narrowed principal
that kept `Role: "owner"` would short-circuit `Allows` to true for every action
and silently discard the scoping it exists to enforce. An owner minting a
scoped key must end up genuinely scoped.

**Scopes are not validated against the creator's grant at mint time.** A scope
the creator does not hold is inert — `Narrow` drops it on every request — so
rejecting it at creation would buy nothing and cost something: it bakes a
point-in-time grant into a long-lived credential and produces confusing 422s
after an unrelated role change. The UX gap this leaves (typing `sheets:write`
and getting a silently useless key) is a frontend warning problem, not a
backend rejection problem. See Q2.

**Three states, not two.** Omitted/`null` = no independent restriction. A
non-empty list = narrowed to it. `[]` = rejected, on the reading that an empty
list is a bug rather than an intent — see Q4.

## 5. Transport

**Streamable HTTP, stateless JSON, one endpoint per connector.** No session
state, no `/sse` route, and none should be added: HTTP+SSE was deprecated in
the 2025-03-26 MCP spec and superseded again in 2025-11-25, so implementing it
now would be building on a twice-deprecated transport for compatibility with
clients nobody has named. Statelessness is what makes the call-time permission
re-check meaningful — there is no cached session to go stale.

---

## 6. Open questions

**Q1 — Is a `sapanjai_whoami` tool worth building?**
`sapanjai_describe_connector` already proved the handshake, so a tool returning
the caller's org and *resolved* permissions would be a diagnostic convenience —
"what does this key actually allow?" answered in one `tools/call` instead of a
support ticket — rather than risk retirement.
*Resolved by:* whether scoped keys turn out to be common in practice. If keys
stay mostly unscoped, skip it.

**Q2 — Should the dashboard warn about scopes the creator does not hold?**
§4 settles that the backend will not reject them. The open part is whether the
frontend should fetch the creator's actions and grey out unheld ones.
*Resolved by:* the owner. It is a frontend-only change either way, and does not
affect the API contract.

**Q3 — Does any target MCP client still require HTTP+SSE?**
§5 assumes not.
*Resolved by:* naming a specific client that cannot do Streamable HTTP. Absent
one, this stays closed.

**Q4 — Should `scopes: []` be a 422, or mean "unrestricted"?**
§4 picks 422. The forgiving alternative — treat `[]` as `null` — hides a
frontend bug that would otherwise mint a dead key.
*Resolved by:* the owner; a one-line change in `internal/module/mcpkey/dto.go`.

**Q5 — Do expired and revoked PAT rows need a cleanup job?**
Excluded in §3 as housekeeping, though `sessions` got exactly such a job
(`internal/job/sessioncleanup`) and the pattern is cheap to copy.
*Resolved by:* whether `mcp_api_keys` row counts ever grow enough to matter.
Unlike sessions, keys are minted by hand, so probably never.
