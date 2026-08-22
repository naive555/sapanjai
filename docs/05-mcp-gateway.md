# MCP Gateway — design notes

> **Status (2026-08-22): Phase 0 and Phase 1 shipped. Phase 2 shipped for one
> connector, read-only. Phase 3 shipped in part.** The gateway itself —
> `POST /mcp/:connectorId` (`internal/module/mcp`), org-scoped Personal
> Access Tokens (`internal/module/mcpkey`, `/mcp-keys`), the RBAC-filtered
> tool catalog, and the per-connector Redis rate limiter — is live production
> code, built and verified end to end in
> [`docs/07-sheets-adapter-plan.md`](07-sheets-adapter-plan.md) steps 1–4. The
> first real connector, `google_sheets` (`internal/adapter/googlesheets`,
> [`docs/06-sheets-adapter.md`](06-sheets-adapter.md)), is also live: allowlisted
> Sheets + Drive reads, a registered `connector.Checker`, and dashboard pages
> to mint/revoke MCP keys (`/mcp-keys`) and configure the connector
> (`/connectors`, `/connectors/:id/google-sheets`). **Not built**, with the
> trigger that would change that: any write tool (`sheets_append_row`, …),
> the LINE adapter, an OAuth consent flow in the dashboard (onboarding is a
> manual credential paste today), OAuth 2.1 / dynamic client registration for
> Claude Desktop's connector UI, per-key tool scoping in the mint-key UI, and
> MCP-specific plan limits (`max_mcp_calls_per_month`) — see
> `docs/07-sheets-adapter-plan.md` §3 "Out of scope" and §9 below for the
> full list. See "Suggested phases" for the phase-by-phase breakdown.
> The product pivot: the Sapanjai (HeartBridge) platform core becomes a **Managed MCP Gateway** — per-org, permission-scoped [Model Context Protocol](https://modelcontextprotocol.io) endpoints that let AI agents (Claude, Cursor, …) reach customer data sources, starting with Thai accounting/ERP systems.
> Everything below was validated by the spike in [`spikes/mcp-gateway/`](../spikes/mcp-gateway/) — its own Go module (`github.com/sapanjai/spikes/mcp-gateway`), not built or linted by `make`/CI. Its [`docs/FINDINGS.md`](../spikes/mcp-gateway/docs/FINDINGS.md) holds the full verification transcript and client-registration walkthrough; this document holds what the backend needs to know.

## The shape of it

An MCP tool is a route. `invoice:read` gates `list_invoices` exactly the way
`RequirePermission("invoice:read")` gates a REST route — same action strings,
same `*` / exact / `resource:*` semantics, same denial wording. **The RBAC
engine needs no changes.**

The genuinely new property is that a denied MCP tool is *invisible*, not merely
un-callable: it never appears in `tools/list`, so the model does not know to
attempt it.

## Constraints that shape the architecture

These are the findings that cost design decisions rather than typing. Each was
hit and verified in the spike.

| # | Constraint | Consequence |
| - | ---------- | ----------- |
| 1 | **No per-request org header.** MCP client headers are static configuration; the model cannot choose one per call, and no client has a "switch org" concept. | `x-organization-id` has no analogue. The org must be bound to the credential. See ["Where the org comes from"](#where-the-org-comes-from). |
| 2 | **Stateful HTTP pins the server instance at `initialize`.** Later requests carrying `Mcp-Session-Id` route to the existing session. | Use `StreamableHTTPOptions{Stateless: true}`. A mid-session permission revocation then applies on the next call rather than the next reconnect. |
| 3 | **stdio cannot be multi-tenant.** One process per client, spawned by the client, no request headers — identity must come from env/argv and is fixed for the process lifetime. | stdio is a dev shim only. The credential would also sit in plaintext in the customer's client config, which is not shippable to accounting firms. **The product is the HTTP endpoint.** |
| 4 | **15-minute access tokens are the wrong credential.** An agent session outlives one easily, and mid-session 401 handling varies by client. | Long-lived, revocable, org-scoped API keys — not the existing JWT pair. |
| 5 | **Clients cache the tool list.** `notifications/tools/list_changed` exists but prompt client action cannot be relied on. | Two enforcement layers (below). The tool list is a UX affordance, never an authorization boundary. |
| 6 | **Errors have two channels.** A JSON-RPC error aborts the turn; `CallToolResult{IsError: true}` is text the model can read and adapt to. | Permission denials and not-founds must be `IsError`, so an agent says "I don't have access to that" instead of crashing. Needs a second mapping alongside the existing `apperror` → HTTP-status handler. |

Stateless mode (2) has three payoffs worth stating plainly: the request
lifecycle becomes **identical to an Echo route**, so the existing guard mental
model transfers unchanged; there are **no sticky sessions**, so the k8s
deployment scales horizontally with no session affinity or shared session
store; and permission changes take effect immediately. The cost is no
server→client requests (sampling, elicitation) and no resumable streams —
neither is needed. `JSONResponse: true` additionally drops SSE framing for
plain `application/json`, which is far easier to put behind a load balancer.

If a connector ever needs progress reporting (a long ERP sync), that requires
stateful + SSE and reintroduces sticky routing. Prefer designing those async:
return a job id, poll with a second tool.

## Tool ↔ permission binding

Each tool declares its required action beside itself in one catalog, so "which
tools does this session see?" is a pure function of `(principal, catalog)`.
There is no second list to keep in sync, and a tool added without an action is
a visible omission rather than a silent hole.

| MCP tool (spike) | RBAC action | |
| ---------------- | ----------- | - |
| `list_invoices` | `invoice:read` | `ReadOnlyHint: true` |
| `get_invoice_by_id` | `invoice:read` | org-scoped lookup |
| `create_invoice` | `invoice:write` | proves the read/write split |

### Two enforcement layers, both required

1. **Construction-time filtering** — unpermitted tools are never registered on
   the `*mcp.Server`, so they never reach `tools/list`. This is the layer that
   matters for model behavior: a tool the model cannot see is one it will not
   attempt, which saves tokens and avoids teaching it to expect failures.
2. **Request-time middleware** — every `tools/call` re-checks the action before
   reaching a handler. Redundant against layer 1 today; load-bearing the moment
   a client holds a stale tool list, tools are registered on a shared server,
   or a permission changes mid-session.

Denials return `"Missing permission: <action>"` — byte-identical to the 403 body
`RequirePermission` produces, so gateway logs and API logs grep alike.

### Where the org comes from

This is the one part of the model that does not port cleanly, and it is the
decision that shapes the schema.

`RequireOrg` reads `x-organization-id` per request and checks membership; the
frontend's org switcher is built on exactly that. MCP has no equivalent
(constraint 1). Three ways out:

1. **Org-scoped credential (recommended).** The API key encodes the org; one key
   per principal × org. `x-organization-id` becomes unnecessary because the
   credential *is* the org selection. A user needing two orgs configures two MCP
   servers — honest, and it mirrors how people already keep several connectors.
2. **Path-scoped endpoint** — `POST /mcp/orgs/:orgID`, membership checked against
   the token. Works and puts the org in access logs, but the URL becomes a
   secret-adjacent thing users copy around.
3. **A `switch_organization` tool — do not.** It puts tenant selection inside the
   model's decision loop, where a prompt injection can reach it. **Tenant scope
   must never be model-reachable.**

The spike implements (1). What falls out of it: every tool handler closes over
the session's authenticated org id and passes it to the data layer, so there is
no code path where a model-supplied argument can widen tenant scope. The spike's
`TestTenantIsolation` confirms a valid invoice id from org A returns "not found"
for org B.

## What this repo added

This section described a plan; here is what actually landed, per
`docs/07-sheets-adapter-plan.md` steps 1–4, and where it differs.

- **`mcp_api_keys`** (migration `00008`) — `(id, organization_id, user_id,
  name, key_hash, scopes, last_used_at, expires_at, revoked_at, created_at)`.
  One column beyond the original plan: `scopes text[]` (nullable — `NULL`
  means "whatever the creator's live RBAC grant allows," re-resolved every
  request; non-`NULL` narrows it, never widens it). Hashed with SHA-256, not
  reused-Redis-blacklist — a PAT is long-lived by design, so instant Redis
  revocation was deliberately dropped in favour of one indexed DB read per
  call (`docs/07` §1 Decision 1); revisit only under measured load.
  `internal/module/mcpkey` owns mint/list/revoke behind `/mcp-keys`, with a
  dashboard page at `/mcp-keys` (step 10) to use it without curl.
- **`internal/module/mcp`** — handler → service → catalog shape, mounting the
  Go MCP SDK's `StreamableHTTPHandler` (stateless JSON) at
  `POST /mcp/:connectorId`. The service owns the tool catalog and the
  permission filter, exactly as planned.
- **`RequireMCPKey` middleware** (`internal/middleware/mcpkey.go`) — resolves
  a bearer PAT to `(userID, orgID, principal)` via `rbac.Service.Authorize`
  intersected with the key's `scopes`. One wrinkle the plan didn't
  anticipate: `internal/middleware` cannot import `internal/module/rbac`
  (every handler already imports `middleware`, which would cycle), so the
  principal resolver is injected from `server.go` as a
  `func(...) (any, error)` closure with a type assertion on the other side —
  contained, documented in the code, worth revisiting if a third caller ever
  needs the same shape.
- **New permission actions** — not `invoice:read`/`invoice:write` (those were
  spike placeholders); the real ones seeded for the shipped connector are
  `sheets:read` and `drive:read` (`internal/module/mcp/tools_sheets.go`,
  `tools_drive.go`), plus `mcpkey:{read,write,delete}` for the PAT routes
  themselves. `sheets:write` is reserved in `docs/06-sheets-adapter.md` §5
  but nothing grants it yet — no write tool exists.
- **Audit actions** — `mcp.session.started`, `mcp.tool.called`,
  `mcp.tool.denied`, `mcp.ratelimit.hit`, and one not in the original plan,
  `mcp.file.downloaded` (written by the signed-file-link download route,
  `GET /mcp/files/:connectorId/:fileId`) — all in
  `internal/module/auditlog/service.go`, written best-effort from the
  gateway's enforcement path exactly as designed.
- **Rate limiting** — `internal/infra/redis/ratelimit.go`, a Lua token-bucket
  script keyed `mcp:ratelimit:<connectorId>` (`MCP_RATE_LIMIT_PER_MIN`,
  default 60/min), charged per upstream page fetched by `sheets_query_rows`'s
  scan loop and at a floor of 1 unit per `tools/call` otherwise. Not in the
  original plan's list, but load-bearing per `docs/06-sheets-adapter.md` §6.
- **Plan limits** — **not built.** `max_mcp_calls_per_month` /
  `max_connectors` were judged not load-bearing for the MVP (the
  per-connector rate limiter covers the quota-exhaustion risk that actually
  threatens it); see `docs/07-sheets-adapter-plan.md` §3 "Out of scope."
- **`docs/02-api-contract.md`** — has its MCP keys and MCP gateway sections
  now (`docs/07` step 2 and step 3 onward keep it current per tool added).

## Client integration notes

Durable bits only; the full walkthrough with verified transcripts is in the
spike's `docs/FINDINGS.md`.

- **Go SDK:** `github.com/modelcontextprotocol/go-sdk`, stable at **v1.7.0**,
  negotiating protocol versions `2024-11-05` → `2026-07-28`. Static binaries
  (~12 MB) drop into the existing distroless image unchanged.
- **Schema generation is free.** The generic `mcp.AddTool` derives input/output
  JSON Schema from Go types and validates inputs; `jsonschema:"…"` struct tags
  become the descriptions the model reads. **Tool and field descriptions are
  prompt surface** — budget real review time for their wording, not just their
  types.
- **stdout is the wire** for stdio servers. A stray `fmt.Println` corrupts the
  JSON-RPC stream and the client drops the connection with an opaque parse
  error. Log to stderr.
- **Roots and sampling are deprecated** as of protocol `2026-07-28` (SEP-2577).
  Do not build on them.
- **No CORS concern** — MCP clients are not browsers, so the "no CORS middleware
  and none should be added" rule survives. But note the MCP endpoint is a **new
  public surface** that the frontend's `/api/*` proxy does not front.
- **Claude Desktop's custom-connector flow expects OAuth 2.0** with dynamic
  client registration; a static `Authorization` header works in Claude Code and
  Cursor but is not the Desktop path.

## Open decisions

Both resolved — see `docs/07-sheets-adapter-plan.md` §1 Decisions 1 and 2 for
the full reasoning the owner signed off on 2026-08-18.

1. ~~**Credential + org binding.**~~ **Resolved: org-scoped API keys** (option
   1 above), shipped as `internal/module/mcpkey`'s `mcp_api_keys` table.
2. ~~**OAuth or static bearer keys.**~~ **Resolved: static keys** (PATs) —
   shipped. OAuth is still second, and still not started (Phase 4 below);
   nothing has changed the trigger condition (Claude Desktop's connector UI
   becoming a real acquisition channel).

## Suggested phases

### Phase 0 — Spike ✅
Standalone MCP server, dummy invoice tools, RBAC-filtered tool surface, both
transports registered and confirmed connected in Claude Code. Done; see
[`spikes/mcp-gateway/`](../spikes/mcp-gateway/).

### Phase 1 — Gateway skeleton ✅
Shipped. `mcp_api_keys` migration (`00008`) + sqlc queries, `RequireMCPKey`,
`internal/module/mcp` mounting the SDK handler at `POST /mcp/:connectorId` in
stateless mode (the path carries `:connectorId`, one refinement past this
doc's original `POST /mcp` — see "The org comes from" below, unchanged: the
org is still bound to the key, not the path). The spike's catalog + two-layer
enforcement was ported calling the real `rbac.Service.Authorize` (not
`HasPermission` directly — see "What this repo added" above for why). Audit
actions and the rate limiter both landed in this phase too. Built and
verified in `docs/07-sheets-adapter-plan.md` steps 1–4; step 3 in particular
retired risk 1 below (the SDK mounts cleanly inside Echo).

### Phase 2 — First real connector ✅ (read-only)
One connector end-to-end: `google_sheets`, not a Thai accounting system —
the owner redirected to Google Sheets/Drive first (`docs/06-sheets-adapter.md`).
Per-tenant OAuth (manual credential paste, `docs/07` §1 Decision 2), all six
spec'd read tools (`sheets_list_spreadsheets`, `sheets_describe_spreadsheet`,
`sheets_query_rows`, `sheets_read_range`, `drive_list_folder`,
`drive_get_file`), a registered `googlesheets.Checker`, and the connector's
own rate-limit/outage handling are all live (`docs/07` steps 5–9). **Not
done**: write tools (`docs/06` §9, `docs/07` §3) and a second connector type
(FlowAccount, PEAK, Xero TH) — this phase is one connector deep, not the
breadth the original phase description implied.

### Phase 3 — Self-service (partial)
Done: dashboard pages to mint/revoke MCP keys (`/mcp-keys`, `docs/07` step
10) and to configure the `google_sheets` connector including its allowlist
(`/connectors`, `/connectors/:id/google-sheets`, step 11 — this step's scope
grew to include the connectors list page itself, since none existed before
it). Viewing the MCP audit trail has no dedicated page, but the existing
`/audit-logs` page already lists every `mcp.*` action org-wide, filterable by
action, so this is arguably covered rather than missing. **Not done**:
picking which tools a key exposes (every key gets the creator's full
intersected grant; there is no scope picker in the mint-key dialog — a known
gap, see step 3's "known wart" in the archived tracker) and plan-limit
enforcement on calls/connectors.

### Phase 4 — OAuth
Dynamic client registration so Claude Desktop's connector UI works. Only once
Desktop is a channel worth the work.

## Reality check

The protocol is the easy half, and it is already proven. The real cost is the
**connector layer** — per-tenant auth to PEAK / FlowAccount / Xero TH, credential
custody, schema mapping, and upstream reliability. Budget Phase 2 accordingly.
