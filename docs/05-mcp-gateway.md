# MCP Gateway — design notes

> **Status: Phase 0 spike complete (2026-08-05). Verdict: feasible, no blockers.**
> **The generic connector skeleton has since landed** (`internal/module/connector`,
> `internal/shared/envelope`): the `connectors` table, RBAC-gated CRUD behind
> `connector:{read,write,delete}`, envelope-encrypted `config`, and an empty
> `Checker` registry (health-check stubbed at 501). See "Phase 2" below — the
> gateway/MCP-protocol pieces (Phase 1) are still no production code.
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

## What this repo needs to add

Additive-forward, per the schema ground rule.

- **`mcp_api_keys`** — `(id, organization_id, user_id, name, key_hash,
  last_used_at, expires_at, revoked_at)`. Hash the key; reuse the existing Redis
  blacklist pattern for instant revocation.
- **`internal/module/mcp`** — the usual handler → service → queries shape. The
  handler is unusual only in delegating to the SDK's `StreamableHTTPHandler`
  rather than returning JSON itself; the service owns the tool catalog and the
  permission filter.
- **`RequireMCPKey` middleware** — resolves an API key to
  `(userID, orgID, role, actions)`: the same tuple `RequireOrg` already builds,
  from a different credential.
- **New permission actions in the seed** — `invoice:read`, `invoice:write`, and
  whatever each connector adds. Ordinary `permissions` rows; the `rbac` module
  needs no code change.
- **Audit actions** — `mcp.session.opened`, `mcp.tool.called`, `mcp.tool.denied`,
  written best-effort from the enforcement middleware. `tools/call` is the
  natural auditable unit. This is a **product feature, not overhead**: "every
  action your AI agent took against your accounting system, with the permission
  that allowed it" is the compliance story the product is selling.
- **Plan limits** — `plans.max_members` already establishes the pattern; add
  `max_mcp_calls_per_month` / `max_connectors`, checked in the same middleware
  that checks permissions.
- **`docs/02-api-contract.md`** — gets an MCP section when the module lands. It
  is a JSON-RPC envelope rather than REST, so it needs its own section shape.
  *Deliberately not added yet* — the contract is the source of truth for routes
  that exist.

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

Both want settling before Phase 1 code, since they shape the schema.

1. **Credential + org binding.** Recommend org-scoped API keys (option 1 above).
2. **OAuth or static bearer keys.** Recommend **static keys first** — ship, get
   design partners onto Claude Code / Cursor — and **OAuth second**, when
   Desktop's connector UI becomes a real acquisition channel. Do not let OAuth
   block Phase 1.

## Suggested phases

### Phase 0 — Spike ✅
Standalone MCP server, dummy invoice tools, RBAC-filtered tool surface, both
transports registered and confirmed connected in Claude Code. Done; see
[`spikes/mcp-gateway/`](../spikes/mcp-gateway/).

### Phase 1 — Gateway skeleton
`mcp_api_keys` migration + sqlc queries. `RequireMCPKey`. `internal/module/mcp`
mounting the SDK handler at `POST /mcp` in stateless mode. Port the spike's
catalog + two-layer enforcement, calling the **real** `rbac.Service.HasPermission`
rather than the spike's port. Audit actions. Seed the new permission actions.
Still serving mock data — the goal is the authorization path in production shape.

### Phase 2 — First real connector
One Thai accounting system end-to-end. The generic skeleton (schema, envelope
encryption, RBAC-gated CRUD, the `Checker` interface) is done — see the status
note above — so what's left is the actual work: per-tenant upstream
authentication, mapping the accounting system's data model onto stable tool
schemas, implementing a `connector.Checker` for it, and absorbing its rate
limits and outages.

### Phase 3 — Self-service
Frontend pages to mint/revoke MCP keys, pick which tools a key exposes, and view
the MCP audit trail. Plan-limit enforcement on calls and connectors.

### Phase 4 — OAuth
Dynamic client registration so Claude Desktop's connector UI works. Only once
Desktop is a channel worth the work.

## Reality check

The protocol is the easy half, and it is already proven. The real cost is the
**connector layer** — per-tenant auth to PEAK / FlowAccount / Xero TH, credential
custody, schema mapping, and upstream reliability. Budget Phase 2 accordingly.
