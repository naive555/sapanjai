# Mapping MCP tools onto controlplane's RBAC model

_Task item 3. Validated by the code in this spike, not just sketched._

## The one-line version

An MCP tool is a route. `invoice:read` gates `list_invoices` exactly the way
`RequirePermission("invoice:read")` would gate `GET /invoices` — same action
strings, same match semantics, same denial wording. The only genuinely new
thing is that a denied MCP tool is *invisible*, not just un-callable.

## The existing engine, unchanged

`apps/backend/internal/module/rbac/service.go`'s `HasPermission` is ported
verbatim into `internal/rbac/rbac.go`. Match order:

1. no membership in the org → deny
2. membership role `owner` → allow, without consulting the roles tables
3. otherwise scan the caller's aggregated actions for `*`, an exact
   `resource:verb`, or the `resource:*` wildcard

Nothing about MCP requires changing any of that. The spike's tests exercise
all four branches (`internal/gateway/gateway_test.go`,
`TestToolVisibilityByPermission`).

## The binding

Each tool declares its required action next to itself, in one catalog
(`internal/tools/tools.go`):

| MCP tool            | RBAC action     | Notes                        |
| ------------------- | --------------- | ---------------------------- |
| `list_invoices`     | `invoice:read`  | `ReadOnlyHint: true`         |
| `get_invoice_by_id` | `invoice:read`  | `ReadOnlyHint: true`         |
| `create_invoice`    | `invoice:write` | write path, proves the split |

"Which tools does this session see?" is then a pure function of
`(principal, catalog)` — there is no second list to keep in sync, and adding
a tool without an action is a compile-time-visible omission rather than a
silent hole.

Observed, from the running server (`tools/list` against the same URL, five
different bearer tokens):

```
tok_owner_siam       (role=owner)                -> [create_invoice, get_invoice_by_id, list_invoices]
tok_reader_siam      (invoice:read)              -> [get_invoice_by_id, list_invoices]
tok_bookkeeper_siam  (invoice:*)                 -> [create_invoice, get_invoice_by_id, list_invoices]
tok_nogrants_siam    (report:read only)          -> []
tok_bogus                                        -> HTTP 401
```

## Two enforcement layers, and why both

**Layer 1 — construction-time filtering** (`gateway.BuildServer`). Unpermitted
tools are never registered on the `*mcp.Server`, so they never appear in
`tools/list`. This is the layer that matters for *model behavior*: a tool the
model cannot see is a tool it will not attempt, which saves tokens and avoids
training it on failures. This has no analogue in the REST API — a 403 route
still exists; a hidden tool does not.

**Layer 2 — request-time middleware** (`gateway.EnforcePermissions`, an
`mcp.Middleware` on `tools/call`). Redundant against layer 1 *today*, and
load-bearing the moment any of the following is true, all of which will be:

- the client cached a tool list from before a permission change
- tools get registered on a shared server rather than a per-principal one
- a permission is revoked mid-session

**The tool list is a UX affordance, not an authorization boundary.** Anything
that treats it as one will be wrong the first time a role changes.

Denials come back as `CallToolResult{IsError: true}` carrying
`"Missing permission: <action>"` — byte-identical to the 403 body
`RequirePermission` produces, so gateway logs and API logs grep alike.

## Where the org comes from — the one real deviation

This is the part that does not port cleanly, and it is worth deciding
deliberately.

`RequireOrg` reads `x-organization-id` off each request and checks membership.
The caller picks the org per request; the frontend's org switcher is built on
exactly that. **MCP has no equivalent.** Client-side headers are static
configuration; the model cannot choose one per call, and Claude Code /
Claude Desktop offer no "switch org" concept.

Three ways out:

1. **Org-scoped credential** (recommended). The MCP API key encodes the org.
   One key per (user or service account) × org. `x-organization-id` becomes
   unnecessary because the credential *is* the org selection. Users who need
   two orgs configure two MCP servers — which is honest, and mirrors how
   people already keep several MCP connectors.
2. **Path-scoped endpoint** — `POST /mcp/orgs/:orgID`, membership checked
   against the token. Works, and makes the org visible in access logs, but the
   URL is now a secret-adjacent thing users copy around.
3. **An `switch_organization` tool** — do not. It puts tenant selection inside
   the model's decision loop, which means a prompt injection can move the
   session to another tenant. Tenant scope must never be model-reachable.

The spike implements (1). Note what falls out of it in
`internal/tools/tools.go`: every handler closes over `p.OrgID` and passes it
to the data layer, so there is no code path where a model-supplied argument
widens the tenant scope. `TestTenantIsolation` confirms a valid invoice id
from org A returns "not found" for org B.

## What controlplane needs to add

Small, and additive-forward per the repo's migration rule:

- **`mcp_api_keys`** — `(id, organization_id, user_id, name, key_hash,
  last_used_at, expires_at, revoked_at)`. bcrypt or SHA-256 the key; reuse the
  existing Redis blacklist pattern for instant revocation.
- **New actions in the seed** — `invoice:read`, `invoice:write`, and whatever
  each connector adds. These are ordinary rows in `permissions`; the RBAC
  module needs no code change.
- **A `mcp` module** following the existing handler → service → queries shape.
  The MCP handler is unusual only in that it delegates to
  `StreamableHTTPHandler` rather than returning JSON itself.
- **Audit actions** — `mcp.tool.called`, `mcp.tool.denied`, `mcp.session.opened`.
  `tools/call` is the natural auditable unit and maps onto the existing
  best-effort `auditlog` writes. This is a selling point, not overhead: "every
  action your AI agent took against your accounting system, with the
  permission that allowed it" is the compliance story the product wants.
- **Plan limits** — `plans.max_members` already establishes the pattern; add
  `max_mcp_calls_per_month` / `max_connectors` and check them in the same
  middleware that checks permissions.
