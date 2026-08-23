# MCP Gateway Core (connector-agnostic) — execution plan

> **Status: not started (planned 2026-08-23).** 0 / 2 required steps shipped
> (step 3 is optional and gated on open question Q1).
>
> - [ ] Step 1 — Accept `scopes` on `POST /mcp-keys`
> - [ ] Step 2 — Scope selection in the MCP keys dashboard
> - [ ] Step 3 *(optional)* — `sapanjai_whoami`
>
> **What this is.** A re-cut of
> [`docs/07-sheets-adapter-decisions.md`](../../docs/07-sheets-adapter-decisions.md)
> steps 1–3, written after verifying the repo at `d11c5d1`. It does not
> contradict `docs/07`; it re-slices the same work along a connector-agnostic
> boundary so the gateway core can be finished while the choice of *next*
> connector is open.
>
> **Headline finding: the three items this plan was asked to cover are, with
> one exception, already built and merged on `dev`.** `docs/07` steps 1–12 all
> landed. What follows is (a) the evidence, and (b) the one genuinely
> unfinished piece of connector-agnostic scope, cut into mergeable steps.
>
> **Assumptions & boundary:**
> [`docs/08-gateway-core.md`](../../docs/08-gateway-core.md) — what counts as
> connector-agnostic and why, the `scopes` design decisions, and the open
> questions. Read it before changing any step here.
> **Spec:** [`docs/05-mcp-gateway.md`](../../docs/05-mcp-gateway.md) ·
> **Contract:** [`docs/02-api-contract.md`](../../docs/02-api-contract.md)
>
> **Archive this file into `archives/` once the last step ships.** `docs/08`,
> `docs/05`, and `docs/02` hold this feature's maintained state — those are the
> files to update when it changes again, not this one.

---

## 1. Already done

Verified against `dev` at `d11c5d1` by reading the migrations directory, the
packages involved, and `git log` — not assumed. `make test` and `make lint`
are green as of this writing.

### Item 1 — Personal Access Token auth for MCP clients: **shipped**

| Piece | Where | Commit |
|---|---|---|
| Table | `apps/backend/migrations/00008_mcp_api_keys.sql` — `(id, organization_id, user_id, name, key_hash, scopes text[], last_used_at, expires_at, revoked_at, created_at)`, unique `key_hash`, unique `(organization_id, name)`, FK-cascade to orgs and users, index on `organization_id` | `46e2b00` |
| Hashed storage | `internal/module/mcpkey/service.go` — `sk_live_<base64url(32 CSPRNG bytes)>`, SHA-256 hashed, raw token returned only from `POST /mcp-keys` (`CreateResponse.APIKey`), never persisted, never logged | `b4d98f9` |
| Bearer auth | `internal/middleware/mcpkey.go` — `RequireMCPKey` resolves the presented PAT by hash, rejects missing/malformed/unknown/revoked/expired identically, stamps `last_used_at` best-effort | `46e2b00` |
| Org scoping | The key row carries `organization_id`; every gateway request resolves the connector inside that org, so another org's connector is a 404 | `46e2b00` |
| Permission-subset scoping | `rbac.Principal.Narrow(scopes)` — `NULL` scopes = the creator's live grant, non-`NULL` intersects (never widens), and clears `Role` so the owner bypass cannot leak through | `f9c6e64` |
| Revocation | `DELETE /mcp-keys/:keyId` sets `revoked_at`, idempotent, 404 for another org's id | `b4d98f9` |
| Redaction | `internal/shared/logger/redact.go` — `apikey` is in `sensitiveKeys` (keys normalize with `-`/`_` stripped, so `api_key`/`apiKey` both match) | prior |
| Contract + swagger | `docs/02-api-contract.md` §"MCP keys (`/mcp-keys`)" lines 177–195; swaggo annotations on all three handlers | `b4d98f9` |
| Tests | unit: `internal/module/mcpkey/{service,handler}_test.go`; integration: `internal/server/mcpkey_integration_test.go` (CRUD, duplicate name, validation, per-verb permission enforcement, owner bypass, cross-org isolation, guard basics) | `b4d98f9` |
| Frontend | `apps/frontend/app/(dashboard)/mcp-keys/page.tsx` + `page.test.tsx` | `5673b64`, `df1157f` |

**The one gap** — see §3: `POST /mcp-keys` has **no `scopes` field**.
`CreateMCPKey` (`internal/infra/database/queries/mcp_api_keys.sql`) inserts
`(organization_id, user_id, name, key_hash, expires_at)` only, so every key
minted through the API has `scopes = NULL`. The narrowing machinery, the
middleware plumbing, the `MCPKeyResponse.Scopes` field, and integration
coverage all exist; the two tests that exercise narrowing
(`TestIntegration_MCP_ScopedKeyNarrowsOwnerBypass`,
`..._ScopedKeyIncludingPermissionStillWorks`) set `scopes` with raw SQL and
say so in a comment: *"exactly as step 3 prescribes for exercising a code path
nothing else writes yet."*

### Item 2 — Permission matcher extraction: **shipped, complete**

`internal/module/rbac/permission.go` exposes `ActionMatches(granted []string,
action string) bool` with the documented `*` > exact > `resource:*` semantics,
plus `Principal{UserID, OrganizationID, Role, Actions}` with `Allows(action)`
(owner bypass, then `ActionMatches`) and `Narrow(scopes)`.

The rewiring the task asked for is done: `rbac.Service.HasPermission` is now a
thin wrapper over `Authorize(...).Allows(action)`
(`internal/module/rbac/service.go:211`), and
`middleware.Guards.RequirePermission` still calls `HasPermission` unchanged.
One implementation, two callers, no duplicated matcher.

One wrinkle worth knowing (documented in `docs/05` and in the code):
`internal/middleware` cannot import `internal/module/rbac` — every handler
already imports `middleware`, which would cycle — so `server.go` injects the
principal resolver as a `func(ctx, userID, orgID, scopes) (any, error)`
closure with a type assertion on the other side. Contained and deliberate.

Unit coverage: `internal/module/rbac/permission_test.go`,
`internal/middleware/auth_test.go`.

### Item 3 — MCP handshake + RBAC end-to-end against one trivial tool: **shipped, complete**

- `POST /mcp/:connectorId` (`internal/module/mcp/handler.go`) mounts the Go MCP
  SDK's `StreamableHTTPHandler`, **stateless JSON, single endpoint**. No `/sse`
  route exists and none should be added.
- The trivial connector-agnostic tool exists — it is
  **`sapanjai_describe_connector`**, not `sapanjai_whoami`. It touches no
  upstream API, returns `{name, type, status}` from the connector row (its
  output struct has no `config` field by construction), and is gated on
  `connector:read`.
- Two enforcement layers, both live: `BuildServer` registers only permitted
  entries, so a denied tool never appears in `tools/list`; `Service.enforce`
  re-checks at call time, so a permission change takes effect on the very next
  call rather than the next reconnect.
- Audit events shipped **with** the tool, not as a later pass:
  `mcp.session.started`, `mcp.tool.called`, `mcp.tool.denied`,
  `mcp.ratelimit.hit`, `mcp.file.downloaded` — all in
  `internal/module/auditlog/service.go`, all written best-effort.
- Integration coverage in `internal/server/mcp_integration_test.go`: no key,
  malformed token, revoked key, expired key, cross-org connector is 404,
  principal without `connector:read` sees zero tools, scoped key narrows the
  owner bypass, scoped key including the permission still works, happy path
  `initialize → tools/list → tools/call` with the audit trail asserted, and
  tool-call audit surviving client disconnect.

### Beyond this plan's scope, also already merged

`docs/07` steps 4–12 landed too: the Redis token-bucket rate limiter
(`mcp:ratelimit:<connectorId>`), the whole `google_sheets` adapter
(`internal/adapter/googlesheets`, six read tools), signed file-download links,
and both frontend surfaces. Nothing in this plan touches any of it.

---

## 2. Scope

**In scope — the connector-agnostic gateway core:**

1. **PAT auth for MCP clients** — table, hashed storage, bearer auth, org +
   permission-subset scoping, revocation. *Done except scope selection at mint
   time — §3 steps 1–2.*
2. **Permission matcher extraction** — a callable matcher plus a resolved
   `Principal`, with the middleware rewired onto the same function. *Done; no
   remaining work.*
3. **Handshake + RBAC proven end-to-end against one trivial tool.** *Done; no
   remaining work. Optional follow-on in step 3.*

**Out of scope, and why.** The boundary test — *"if the first connector were
FlowAccount instead of Google Sheets, would this be needed unchanged?"* — and
the full exclusion table live in
[`docs/08-gateway-core.md`](../../docs/08-gateway-core.md) §1 and §3. In
short: no Google OAuth, no `sheets_*`/`drive_*` tools or allowlist, no write
tools or approval flow, no per-connector rate limiting, no `/sse` route, no
plan limits, no PAT cleanup job.

---

## 3. Steps

Two required steps and one optional. Each is one PR: the repo compiles and
`make test` + `make lint` pass at the end of every one.

No migration is needed anywhere in this plan — `scopes text[]` already exists
on `mcp_api_keys` from migration `00008`. **Do not edit `00008`**
(`CLAUDE.md`: schema changes are additive-forward only). Step 1 changes a sqlc
*query*, which is regenerated code, not an applied migration.

---

### Step 1 — Accept `scopes` on `POST /mcp-keys`

**Goal.** Let a key be minted already narrowed to a subset of the creator's
permissions, closing the one gap in item 1: the `scopes` column, `Narrow`, and
the middleware plumbing all exist, but nothing in the API can write the column.

**Dependencies.** None — everything it builds on is merged.

**Files modified**

- `apps/backend/internal/infra/database/queries/mcp_api_keys.sql` — add
  `scopes` to `CreateMCPKey`'s column list and `VALUES` (now `$6`). Keep the
  existing comment style.
- **Run `make sqlc`** → regenerates
  `apps/backend/internal/infra/database/db/mcp_api_keys.sql.go`
  (`CreateMCPKeyParams` gains `Scopes []string`). Commit the regenerated file.
- `apps/backend/internal/module/mcpkey/dto.go` — `CreateRequest` gains
  `Scopes []string \`json:"scopes" validate:"omitempty,min=1,max=64,dive,permaction"\``.
  Document the three-state contract in the doc comment: **omitted or `null`** =
  no independent restriction (the key rides the creator's live grant, exactly
  as today); **a non-empty list** = narrowed to that list, re-intersected on
  every request; **`[]`** = rejected 422, since a key that permits nothing is a
  configuration mistake, not a use case.
- `apps/backend/internal/server/validator.go` — register a `permaction`
  validator alongside `orgslug`/`connectortype`, matching `*` or
  `<resource>:<verb>` / `<resource>:*` where resource and verb are
  `[a-z][a-z0-9_]*`. Keep the pattern in this file next to `orgSlugPattern`.
- `apps/backend/internal/module/mcpkey/service.go` — `Create` takes
  `scopes []string` and passes it through to `CreateMCPKeyParams`. **No
  validation of scopes against the creator's grant here**: `Principal.Narrow`
  intersects with the *live* grant on every request, so a scope the creator
  does not hold is simply inert, and pre-validating it would bake a
  point-in-time grant into a long-lived credential (rationale: `docs/08` §4).
  Say so in the doc comment.
- `apps/backend/internal/module/mcpkey/handler.go` — pass `req.Scopes` through;
  extend the swaggo block (the existing `422 Validation failed` line already
  covers a bad scope string, so no new `@Failure` is needed).
- **Run `make swagger`** → regenerates `apps/backend/docs/`.
- `docs/02-api-contract.md` — update the `POST /mcp-keys` row (line ~187) to
  document the new optional `scopes` field and its three-state semantics, and
  note that scopes narrow but never widen, re-resolved per request.

**Tests**

- *Unit* (`internal/module/mcpkey/service_test.go`): the mock store receives
  the scopes verbatim; `nil` scopes stay `nil` (not `[]string{}`, which would
  be a non-`NULL` empty array in Postgres and silently mint a key that permits
  nothing — assert this explicitly).
- *Unit* (`internal/server/validator_test.go`): `permaction` accepts `*`,
  `connector:read`, `sheets:*`; rejects `""`, `Connector:Read`, `a:b:c`,
  `:read`, `connector:`.
- *Integration* (`internal/server/mcpkey_integration_test.go`): minting with
  `scopes` persists them and `GET /mcp-keys` echoes them back; minting without
  `scopes` yields `null`; `scopes: []` → 422; a malformed scope string → 422.
- *Integration* (`internal/server/mcp_integration_test.go`): replace the raw
  `UPDATE mcp_api_keys SET scopes = ...` in the two narrowing tests with a
  mint-time `scopes` argument, so the path is exercised end-to-end through the
  API rather than around it. Delete the "a code path nothing else writes yet"
  comment.

**Ground rules.** Module convention handler → service → sqlc, service returns
`apperror` codes and imports no `net/http`. New/changed contract behaviour goes
into `docs/02-api-contract.md` with swaggo annotations and `make swagger` in
this same step. The PAT itself is untouched here — still hashed, still returned
once, still `[REDACTED]`-covered by the `apikey` key in
`internal/shared/logger/redact.go` (no call-site redaction).

**Verification**

```
make sqlc && make swagger
make test && make lint
git diff --stat apps/backend/migrations/   # must be empty
```

---

### Step 2 — Scope selection in the MCP keys dashboard

**Goal.** Make step 1 usable without curl: pick scopes when minting a key, and
see them on the list.

**Dependencies.** Step 1 (the API field must exist).

**Files modified**

- `apps/frontend/app/(dashboard)/mcp-keys/page.tsx` — add an optional scope
  selector to the create dialog and render `scopes` in the table (an
  "unrestricted" affordance when `null`). Offer the actions the gateway
  actually gates — today `connector:read`, `sheets:read`, `drive:read` — as
  checkboxes, plus a free-text entry for anything else, since the catalog will
  grow with each adapter. Leaving everything unchecked sends no `scopes` field
  at all (unrestricted); it must never send `[]`.
- `apps/frontend/app/(dashboard)/mcp-keys/page.test.tsx` — cover: no selection
  omits the field from the request body; a selection sends exactly those
  scopes; the table renders both a scoped and an unrestricted key.
- Whichever `apps/frontend/lib/api/` type describes the MCP key payloads —
  add `scopes?: string[]` to the create request type (the response type
  already carries `scopes`).

**Ground rules.** The browser calls same-origin `/api/*` through
`app/api/[...path]/route.ts`; no CORS work, no direct backend call.

**Verification**

```
cd apps/frontend && pnpm lint && pnpm exec tsc --noEmit && pnpm test
```

---

### Step 3 (optional) — `sapanjai_whoami`

**Goal.** A second connector-agnostic tool returning the caller's
organization, key name, and *resolved* permission list, so an operator can see
what a scoped key actually grants from inside the MCP client — the natural
diagnostic for step 1, and the thing that makes a mis-scoped key obvious in one
`tools/call` instead of a support ticket.

**Optional, and deliberately so.** The risk this plan's item 3 existed to
retire is already retired by `sapanjai_describe_connector`. Build this only if
the answer to Q1 (`docs/08` §6) is yes.

**Dependencies.** Step 1 (without settable scopes there is little to diagnose).

**Files**

- Created: `apps/backend/internal/module/mcp/tools_whoami.go`,
  `tools_whoami_test.go`.
- Modified: `apps/backend/internal/module/mcp/catalog.go` — one `Entry` with
  `ConnectorType: ""`. Gate it on `connector:read`, the same action
  `sapanjai_describe_connector` uses: it must not be reachable by a principal
  who can see no tools at all, and inventing a `whoami:read` action nobody
  seeds would make it invisible to every existing key.
- Modified: `docs/02-api-contract.md` — add the tool to the gateway's tool
  table.

**Output shape.** `{organizationId, keyName, permissions []string}` and nothing
else. It must never carry the key id, the hash, a token, or any connector
config — same invariant as `describeConnectorOutput`, and worth restating in a
comment on the output struct.

**Tests.** *Unit*: the output lists exactly the narrowed principal's actions;
an owner with `NULL` scopes reports the owner bypass rather than an empty list
(decide and assert the representation — `["*"]` is the honest rendering).
*Integration* (`internal/server/mcp_integration_test.go`): a scoped key's
`sapanjai_whoami` reports exactly its scopes; a `mcp.tool.called` audit row is
written.

**Ground rules.** Audit events ship with the tool. Best-effort audit writes
never fail the call.

**Verification**

```
make swagger && make test && make lint
```

---

## 4. Open questions

Recorded in [`docs/08-gateway-core.md`](../../docs/08-gateway-core.md) §6, with
what would resolve each. The two that gate work here:

- **Q1** — is `sapanjai_whoami` worth building? Gates step 3 entirely.
- **Q4** — should `scopes: []` be a 422 or mean "unrestricted"? Step 1 assumes
  422; the other answer is a one-line change in `dto.go`.
