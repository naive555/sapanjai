# Google Sheets/Drive Adapter — decisions & scope

> **Status: the adapter shipped** (steps 1–12, 2026-08-18 → 2026-08-22). All
> four decisions below were **confirmed by the owner 2026-08-18** and none were
> reopened during execution.
>
> **This file was the implementation plan; the plan part has moved.** The 12
> steps now live in
> [`.claude/plans/archives/2026-08-18-sheets-adapter.md`](../.claude/plans/archives/2026-08-18-sheets-adapter.md)
> §"Step detail", alongside the checklist that records how each one actually
> went. What remains here is the durable half: *why* the adapter is shaped the
> way it is, what was deliberately left unbuilt and what would change that, and
> which risks are still live.
>
> **Spec:** [`06-sheets-adapter.md`](06-sheets-adapter.md) (read-only MVP) ·
> **Architecture it builds on:** [`05-mcp-gateway.md`](05-mcp-gateway.md) ·
> **Connector-agnostic boundary:** [`08-gateway-core.md`](08-gateway-core.md)

---

## 1. Decisions

Three questions [`06-sheets-adapter.md`](06-sheets-adapter.md) §10 left open.
All three are resolved below, **all confirmed by the owner 2026-08-18**, and
none was reopened while the adapter was built. Each notes the step it gated, so
the reasoning stays traceable to the code it produced.

A **fourth** decision surfaced while tightening steps 3/5/7 for execution: spec
§4.2's `total` field contradicts §6's memory rule and could not be built as
written. It was resolved the same day and is recorded where it was made — step 7
of [`.claude/plans/archives/2026-08-18-sheets-adapter.md`](../.claude/plans/archives/2026-08-18-sheets-adapter.md)
— and its outcome is visible in `sheets_query_rows`' `scanned_rows` /
`scan_complete` fields instead of an exact `total`.

### Decision 1 — MCP client auth: **Personal Access Tokens (spec option A)**

*Gated step 2. Confirmed 2026-08-18.*

Adopt option A. Option C (reusing refresh tokens) breaks the rotation/reuse-detection
model that `auth` already enforces — a refresh token that a customer pastes into a
static MCP config and never rotates is exactly the token-family reuse the platform
is built to detect. Option B (OAuth 2.1 authorization server) is the correct long-term
answer for Claude Desktop's connector UI, but it is a multi-week build that would
block every other step, and `docs/05` already recommends static keys first and OAuth
second. A PAT is one migration plus one small module.

Resolved details the spec left implicit:

- **Table name `mcp_api_keys`**, not the spec's `api_keys` — matches `docs/05`'s
  naming and leaves room for a future non-MCP key type. Columns per `docs/05`:
  `(id, organization_id, user_id, name, key_hash, scopes, last_used_at, expires_at, revoked_at, created_at)`.
- **Hash with SHA-256, not bcrypt.** This is a deliberate departure from the
  bcrypt-cost-12 rule in `CLAUDE.md`, and it is correct: that rule is about
  *passwords* — low-entropy secrets a human chose. A PAT is 256 bits of CSPRNG
  output, so brute force is irrelevant, and bcrypt is both too slow to run on
  every MCP call and impossible to index for lookup-by-token. `key_hash` gets a
  unique index and the lookup is a single indexed read.
- **Token format `sk_live_<base64url(32 bytes)>`.** Returned in full exactly once,
  from the create response. Never stored, never logged, never re-readable.
- **`scopes text[]` (nullable) ships in the migration** and is enforced as an
  *intersection* with the creator's live RBAC grant from step 3 onward — the spec's
  "subset ของ role ผู้สร้าง". `NULL` means "whatever the user can do in this org",
  re-resolved per request. A PAT can therefore only ever narrow, never widen, and
  revoking the user's role revokes the key's reach immediately.
- **No Redis caching of PAT lookups in the MVP.** `docs/05` suggests reusing the
  blacklist pattern; deferring it is the safer default, because a cache introduces
  revocation lag on the one credential that is long-lived by design. One indexed
  DB read per MCP call is affordable. Revisit only under measured load.

### Decision 2 — OAuth onboarding: **manual credential paste for the MVP**

*Gated steps 5 and 12. Confirmed 2026-08-18.*

The MVP does **not** build a dashboard consent flow. Customers supply
`client_id` / `client_secret` / `refresh_token` for a Google Cloud project and paste
them into the existing `POST /connectors` `config` field, which is already
envelope-encrypted and already never echoed back.

Reasoning: a full three-legged flow needs a redirect endpoint, state/PKCE handling,
token-exchange storage, a frontend consent page, **and** a Google OAuth consent
screen review — `spreadsheets.readonly` and `drive.readonly` are sensitive scopes,
and verification for a public app takes weeks of calendar time that is entirely
outside our control. Design partners can run in a testing-mode project (under 100
users, no verification) today. Paste-path costs one connector type and zero new
routes; it also means the credential-custody code path is identical to the one the
consent flow would eventually feed, so the later flow is an addition, not a rewrite.

The cost is honest and should be stated to the owner: onboarding is a support
conversation, not self-service, until the consent flow lands. That is acceptable for
design partners and not acceptable at scale — the flow is listed in "out of scope"
with its trigger condition.

### Decision 3 — Permission matcher extraction: **extract the matcher and add a resolved `Principal`; middleware keeps calling the same service**

*Gated step 1. Confirmed 2026-08-18.*

One correction to the spec's premise first: §5 says the matcher must be "pulled out
of the middleware", but it is **not** in the middleware. `RequirePermission` already
depends on a narrow `permissionChecker` interface and delegates to
`rbac.Service.HasPermission` (`internal/module/rbac/service.go`), which is already a
plain service function a handler can call. The spec's stated goal is already met.

What is actually missing is a *bulk* path. Filtering the MCP tool catalog means
answering "does this principal have action X?" once per tool per request. Calling
`HasPermission` per tool would issue N membership lookups plus N permission queries
for a single `tools/list`. So the refactor is:

- Extract the `*` / exact / `resource:*` comparison — currently inline in
  `HasPermission` — into an exported pure function `rbac.ActionMatches(granted []string, action string) bool`.
- Add `rbac.Service.Authorize(ctx, userID, orgID) (*rbac.Principal, error)`, which
  resolves membership and the granted action set **once** and returns a value with an
  `Allows(action string) bool` method backed by `ActionMatches`. Owner bypass lives in
  the principal (`role == "owner"` ⇒ `Allows` always true).
- Reduce `Service.HasPermission` to `Authorize(...).Allows(action)`.

The result: one implementation of the semantics, three callers (the existing
middleware — **unchanged**, the MCP catalog filter, the MCP call-time check). The
middleware is rewired only in the sense that the function it already calls is now a
thin wrapper. Behaviour is identical, which is what makes step 1 safe to merge alone.

---

## 2. Steps — shipped, archived

The 12-step execution plan that lived here is complete (steps 1–12, 2026-08-18
→ 2026-08-22). Its per-step goals, file lists, dependencies, and verification
commands moved verbatim to the execution tracker that already carried the
checkboxes and the completion notes:
[`.claude/plans/archives/2026-08-18-sheets-adapter.md`](../.claude/plans/archives/2026-08-18-sheets-adapter.md)
§"Step detail". Code comments elsewhere in the repo that cite "step *N*" mean
that file.

Nothing was dropped in the move. What stayed here is what outlives the
execution: the decisions below, what was deliberately left unbuilt, and the
risks that are still live.

---

## 3. Out of scope

Deliberately not built, with the trigger that would change that.

- **All write tools** — `sheets_append_row`, `sheets_update_cells`, `drive_upload_file`
  (spec §9). They need a confirmation/dry-run/rollback design that does not exist yet.
  Read-only must be stable in production first.
- **LINE adapter** (§9). An action-type tool with a far higher blast radius than any
  read. After read is stable.
- **OAuth 2.1 authorization server / dynamic client registration** (`docs/05` Phase 4).
  Trigger: Claude Desktop's connector UI becoming a real acquisition channel.
- **Google OAuth consent flow in the dashboard.** Trigger: onboarding volume where a
  support conversation per customer stops scaling, or a scope change forcing Google
  app verification anyway.
- **Plan limits on MCP usage** (`max_mcp_calls_per_month`) — `docs/05` Phase 3. Not
  named as load-bearing by the spec; the per-connector rate limiter (step 4) covers
  the quota-exhaustion risk that actually threatens the MVP.
- **Multi-tab join / workflow tools** (spec §10 Q4). The spec's own recommendation is
  coverage before workflow — the agent calls `describe` then two `query_rows` and joins
  the results itself. Revisit if transcripts show agents failing at it.
- **Redis caching of PAT lookups** — see Decision 1.
- **`connector.Service` rotate-on-read wiring** (`CLAUDE.md` follow-up). Real, unrelated
  to this feature, and it needs its own narrow sqlc query. Do not fold it in.

---

## 4. Open risks

Each with the earliest signal that would expose it. Written before execution
and left in that tense, because most of them are *still open*: they were
identified as things production data would settle, and there is no production
data yet.

**Retired.** Risk 1 — the Go MCP SDK mounted inside Echo without incident, and
`POST /mcp/:connectorId` has been serving since step 3. Risk 5 stopped being a
surprise-in-waiting once the intersection semantics were written down
(`docs/02-api-contract.md` §"MCP gateway", `docs/05-mcp-gateway.md`); it is
documented behaviour, and the first support question is still the real signal.

**Still open: 2, 3, 4, and 6.** None of them can be closed from a fixture — each
needs a real customer's spreadsheets, a real Google project that has survived a
week, or real traffic through the limiter.

| # | Risk | Earliest signal |
| - | ---- | --------------- |
| 1 | **The Go MCP SDK does not mount cleanly inside Echo.** The spike ran it standalone; nothing has run it behind Echo's middleware chain and `HTTPErrorHandler`. | Step 3, first Inspector `initialize`. This is why step 3 comes before any Google code — if the mount is wrong, only steps 1–2 are sunk. |
| 2 | **`87 GB` of spreadsheet defeats the Sheets API before it defeats us.** The API returns whole ranges; there is no server-side filter or index. Step 7's bounded scan keeps memory and quota safe, but it cannot make an unindexed scan *fast* — a selective filter over a huge sheet may simply exhaust its budget and return `scan_complete: false` every time, which is honest but not useful. | Step 7, first query against the customer's real largest sheet — not a fixture. Measure hit rate and scanned rows before declaring the tool done. If most real queries come back incomplete, the answer is a materialization/sync job into Postgres, which is a different plan and a bigger one. |
| 3 | **Google OAuth scope verification.** `drive.readonly` is a sensitive scope; a testing-mode project caps at 100 users and unverified apps can hit refresh-token expiry after 7 days. | Step 5, the first health check that survives a week. Test refresh-token longevity in a *published* testing project early — a 7-day expiry would make the paste path unusable and force Decision 2 open again. |
| 4 | **Header-row assumptions break on real sheets.** Merged cells, multi-row headers, and title banners are normal in Thai business spreadsheets. The override map handles a title row; it does not handle a two-row header. | Step 6, `describe` against real customer files. If two-row headers are common, `describe` needs a richer header model before step 7 builds on it. |
| 5 | **PAT scope intersection surprises users.** A key minted by an owner then demoted to member silently loses reach mid-session. Correct, and possibly confusing. | Step 3 integration tests make the semantics explicit; the real signal is the first support question. Documented behaviour, not a bug — but it needs to be *in* the docs (step 12). |
| 6 | **Rate limit of 60/min per connector is a guess.** Google's ~60 req/min/user and ~300 req/min/project are per-project, and one org may run several connectors against one Google project. | Step 4 onward. Watch for `UPSTREAM_AUTH_FAILED`/429 from Google despite our limiter passing — that means the limiter is scoped wrong (per connector, should be per Google project). |
