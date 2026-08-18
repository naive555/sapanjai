# Google Sheets/Drive MCP Adapter — Implementation Plan

> **Status: in progress.** All four decisions **confirmed by the owner 2026-08-18**;
> step 1 started. Later steps still gated only by their stated dependencies.
> **Spec:** [`06-sheets-adapter.md`](06-sheets-adapter.md) (read-only MVP).
> **Architecture it builds on:** [`05-mcp-gateway.md`](05-mcp-gateway.md) Phase 1 + Phase 2.
> **Execution tracker:** [`.claude/plans/2026-08-18-sheets-adapter.md`](../.claude/plans/2026-08-18-sheets-adapter.md)
> — same steps with checkboxes; this file holds the detail.

12 steps. Each is one PR: the repo builds, `make test` and `make lint` pass, and
nothing is half-wired at the end of any step. Steps 1–4 retire the risky unknowns
(MCP handshake, credential model, RBAC filtering, rate limiting) against a trivial
tool before a single line of Google API code exists. Steps 5–9 are the adapter
itself. Steps 10–12 are self-service surface.

---

## 1. Decisions

Three questions the spec left open (§10). All three are resolved below. **Each
needs the owner's confirmation before the step that depends on it starts** — they
are marked with the step they gate.

A **fourth** decision surfaced while tightening steps 3/5/7 for execution: spec §4.2's
`total` field contradicts §6's memory rule and cannot be built as written. It is
resolved inline at **step 7** and needs the same confirmation.

### Decision 1 — MCP client auth: **confirm Personal Access Tokens (spec option A)**

**Gates step 2. Confirm before starting step 2.**

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

**Gates step 5 (and step 12). Confirm before starting step 5.**

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

**Gates step 1. Confirm before starting step 1.**

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

## 2. Steps

### Step 1 — Extract `ActionMatches` + `rbac.Principal`

**Goal.** Make the RBAC semantics callable in bulk from a handler without changing
any existing behaviour, so later steps can filter a tool catalog in one query.

**Files.**
- create `apps/backend/internal/module/rbac/permission.go` — `ActionMatches`, `Principal`, `Principal.Allows`
- create `apps/backend/internal/module/rbac/permission_test.go`
- modify `apps/backend/internal/module/rbac/service.go` — add `Authorize`, reduce `HasPermission` to a wrapper
- modify `apps/backend/internal/module/rbac/service_test.go` — cover `Authorize`

**Depends on.** Nothing. **Gated by Decision 3.**

**Verify.**
```
cd apps/backend && go test ./internal/module/rbac/... ./internal/middleware/... -count=1
make test && make lint
```
The existing `HasPermission` and `RequirePermission` tests must pass **unmodified** —
that is the proof this is a pure refactor. If a middleware test needs editing, the
refactor changed behaviour and is wrong.

**Ground rules.** No `net/http` in the service. `Principal` carries actions only —
it must never grow a credential field.

---

### Step 2 — `mcp_api_keys` migration + PAT module

**Goal.** Ship mint/list/revoke for long-lived, org-scoped, revocable MCP keys. No
MCP protocol code yet — this step is only the credential.

**Files.**
- create `apps/backend/migrations/00008_mcp_api_keys.sql`
- create `apps/backend/internal/infra/database/queries/mcp_api_keys.sql`
- **run `make sqlc`** (regenerates `internal/infra/database/db/`) — an explicit action in this step, not a side effect
- create `apps/backend/internal/module/mcpkey/{service.go,handler.go,dto.go}`
- create `apps/backend/internal/module/mcpkey/{service_test.go,handler_test.go}`
- create `apps/backend/internal/server/mcpkey_integration_test.go`
- modify `apps/backend/internal/shared/apperror/apperror.go` — `MCP_KEY_NOT_FOUND` (404), `MCP_KEY_NAME_TAKEN` (409)
- modify `apps/backend/internal/server/server.go` — wire the module
- modify `apps/backend/internal/shared/logger/redact.go` — nothing new needed if the
  raw-token JSON field is named `apiKey` (already in the sensitive set); **verify
  this rather than assume it**, and add the key there if the field is named otherwise
- modify `docs/02-api-contract.md` — new "MCP keys" section + the two error codes
- **run `make swagger`**

**Routes** (all `perm:` guarded, reusing the existing pattern):

| Method/Path | Guard | Behaviour |
| --- | --- | --- |
| `POST /mcp-keys` | perm:`mcpkey:write` | `{ name, expiresInDays? }` → `{ id, name, apiKey, expiresAt, createdAt }`. **`apiKey` returned here and nowhere else.** |
| `GET /mcp-keys` | perm:`mcpkey:read` | Org's keys, no hash, no raw token. |
| `DELETE /mcp-keys/:keyId` | perm:`mcpkey:delete` | Sets `revoked_at`. `{ success: true }`. 404 for another org's id. |

**Depends on.** Step 1 (not strictly — it compiles without it — but keeping the order
avoids a merge conflict in `rbac`). **Gated by Decision 1.**

**Verify.**
```
make migrate && make sqlc && make test
go test ./internal/server/ -run TestMCPKey -count=1
curl -X POST localhost:3000/mcp-keys -H "Authorization: Bearer $TOK" \
  -H "x-organization-id: $ORG" -d '{"name":"laptop"}'
```
Tests: unit (service, mocked store — token generated once, hash stored, raw token
never persisted) **and** integration (every route × happy path × 401/403/404/409).

**Ground rules.** Additive-forward migration — `00008` is new, nothing earlier is
touched. `make sqlc` is run in this step. The raw token is never logged at any level.

---

### Step 3 — `RequireMCPKey` + `POST /mcp/:connectorId` + one trivial tool ⚠️ **risk retirement**

**Goal.** Prove the whole authorization path end to end — PAT → org → connector →
RBAC-filtered tool list → `tools/call` → audit — against one trivial tool, before any
Google code exists. This is the step that either validates or invalidates the plan.

**Files.**
- create `apps/backend/internal/middleware/mcpkey.go` — `RequireMCPKey`, resolving a bearer PAT to `(userID, orgID, principal)` via `rbac.Authorize` ∩ `scopes`
- create `apps/backend/internal/middleware/mcpkey_test.go`
- create `apps/backend/internal/module/mcp/{handler.go,service.go,catalog.go,errors.go}`
- create `apps/backend/internal/module/mcp/{service_test.go,catalog_test.go}`
- create `apps/backend/internal/server/mcp_integration_test.go`
- modify `apps/backend/internal/module/auditlog/service.go` — `mcp.session.started`, `mcp.tool.called`, `mcp.tool.denied`
- modify `apps/backend/internal/server/server.go` — mount `POST /mcp/:connectorId`
- modify `apps/backend/go.mod` — `github.com/modelcontextprotocol/go-sdk v1.7.0`
- modify `docs/02-api-contract.md` — new "MCP gateway" section (JSON-RPC envelope, its own section shape, per `docs/05`)
- **run `make swagger`**

**Read this before writing any code in this step.**
`spikes/mcp-gateway/` is a **working reference implementation of exactly this
pattern**, verified against SDK v1.7.0. Read these three files and port the *pattern*
— do not import the module, do not copy it into `apps/backend`, do not add it to the
build (it is a separate Go module and must stay one):

| Spike file | What to take from it |
| ---------- | -------------------- |
| `cmd/httpsrv/main.go` | `mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server, opts)` — the per-request server construction that stateless mode requires, plus the `WWW-Authenticate: Bearer realm="sapanjai"` header on 401 that makes MCP clients offer to re-authenticate instead of erroring out. |
| `internal/gateway/gateway.go` | `BuildServer` (construction-time filtering) and `EnforcePermissions` (the `mcp.Middleware` on `tools/call` + `tools/list`), registered via `s.AddReceivingMiddleware`. The `IsError` denial body is already written correctly there. |
| `internal/tools/tools.go` | The `Entry{Name, Permission, Description, Register}` catalog shape — one declaration site binding tool to RBAC action, consumed by both enforcement layers. |
| `internal/principal/principal.go` | Its package comment names the two real calls that replace the spike's fixture table. In our case it is one: `rbac.Service.Authorize` from step 1. |

**Design points fixed here.**
- `StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: ...}` — per `docs/05` constraint 2.
- **Mount into Echo with `echo.WrapHandler(mcpHandler)`.** The SDK gives an ordinary `http.Handler`; Echo wraps it directly.
- ⚠️ **The principal must be put on the *request* context, not `echo.Context`.** `c.Set(...)` writes to Echo's own map, which `mcp.NewStreamableHTTPHandler`'s `func(r *http.Request)` callback **cannot see** — it only receives `r.Context()`. `RequireMCPKey` must therefore end with:
  ```go
  ctx := context.WithValue(c.Request().Context(), principalKey{}, p)
  c.SetRequest(c.Request().WithContext(ctx))
  ```
  This is the single most likely way to lose an hour in this step. The spike does the equivalent at `cmd/httpsrv/main.go`'s `withAuth`.
- SDK surface to expect (all verified in the spike against v1.7.0, but re-check the godoc if a signature does not compile — do **not** invent an adaptation): `mcp.NewServer`, `mcp.Implementation`, `mcp.ServerOptions{Instructions, Logger}`, `mcp.Middleware`, `mcp.MethodHandler`, `mcp.Request`, `mcp.CallToolParamsRaw`, `mcp.ListToolsResult`, `mcp.CallToolResult`, `mcp.Content`, `mcp.TextContent`, `mcp.AddTool`.
- The connector in the path is resolved **and its `organization_id` checked against the PAT's org**. A mismatch is a not-found, indistinguishable from a nonexistent id.
- **Both enforcement layers** from `docs/05`: unpermitted tools are never registered (invisible in `tools/list`), *and* a receiving middleware re-checks the action on every `tools/call`.
- Denials return `CallToolResult{IsError: true}` with the text `Missing permission: <action>` — byte-identical to the REST 403 body. `errors.go` owns the `apperror` → `IsError` mapping; it is a second mapping alongside the HTTP one, not a replacement.
- The trivial tool is **`sapanjai_describe_connector`** (permission `connector:read`): returns the connector's `name` / `type` / `status` only. Chosen because it exercises connector resolution and tenant isolation while being structurally incapable of leaking `config`.

**Depends on.** Steps 1, 2.

**Verify.**
```
go test ./internal/module/mcp/... ./internal/middleware/... -count=1
go test ./internal/server/ -run TestMCP -count=1
npx @modelcontextprotocol/inspector          # initialize → tools/list → tools/call
```
Plus a real client: add to `~/.claude.json` as an HTTP MCP server with
`Authorization: Bearer sk_live_...` and confirm the tool appears and returns.
Integration tests must cover: no key → 401; revoked key → 401; expired key → 401;
key for org A + connector of org B → not-found; principal without `connector:read` →
tool absent from `tools/list` **and** `IsError` on a direct `tools/call`.

**Ground rules.** Audit writes are best-effort — a failed audit insert never fails an
MCP call. Log individual fields only; never log the request envelope. No CORS
middleware — MCP clients are not browsers.

---

### Step 4 — Redis token-bucket rate limiter

**Goal.** Land the limiter **before** any tool can reach a Google API, so no real
tool ever ships unguarded.

**Files.**
- create `apps/backend/internal/infra/redis/ratelimit.go` — token bucket on `mcp:ratelimit:<connectorId>`, exposing `Take(ctx, connectorID, n int)` so a caller can charge more than one unit
- create `apps/backend/internal/infra/redis/ratelimit_test.go`
- modify `apps/backend/internal/module/mcp/service.go` — check before dispatch
- modify `apps/backend/internal/module/mcp/errors.go` — `RATE_LIMITED` with retry-after seconds
- modify `apps/backend/internal/module/auditlog/service.go` — `mcp.ratelimit.hit`
- modify `apps/backend/internal/config/config.go` + `.env.example` + `.env.docker.example` + `README.md` + `CLAUDE.md` — `MCP_RATE_LIMIT_PER_MIN` (default 60)
- modify `docs/02-api-contract.md` — the new env var and error code

⚠️ **The bucket counts _upstream Google API calls_, not MCP tool calls.** This is the
non-obvious part and it must be built this way from the start. A single
`sheets_query_rows` (step 7) issues a paged scan — potentially many Google requests —
so a limiter that charges one unit per tool call would let one agent burn the org's
Google quota while reporting itself well under budget. Step 7's scan loop charges the
bucket per page fetched and aborts mid-scan when the bucket is empty. The public
Google quotas this defends (~60 req/min/user, ~300 req/min/project) are counted in
*API requests*, so anything else measures the wrong thing.

**Depends on.** Step 3.

**Verify.**
```
go test ./internal/infra/redis/... -count=1
go test ./internal/server/ -run TestMCPRateLimit -count=1
```
Integration test: 61 calls in a minute → the 61st returns `IsError` with
`RATE_LIMITED` and a retry-after, and one `mcp.ratelimit.hit` audit row exists.

**Ground rules.** Reuses the existing Redis key convention — no new infra. The audit
write stays best-effort.

---

### Step 5 — `google_sheets` connector type: config schema, allowlist, OAuth exchange, health checker

**Goal.** Teach the platform the connector type and its credential shape, and land
the allowlist — the spec's single most important security boundary (§3) — in the same
step, before any tool can read a cell.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/{config.go,oauth.go,api.go,client.go,checker.go}`
- create `apps/backend/internal/adapter/googlesheets/{config_test.go,oauth_test.go,client_test.go,checker_test.go}`
- modify `apps/backend/go.mod` — `google.golang.org/api`, `golang.org/x/oauth2`
- modify `apps/backend/internal/module/connector/types.go` — `TypeGoogleSheets Type = "google_sheets"` in `validTypes`
- modify `apps/backend/internal/server/server.go` — `connector.NewRegistry(googlesheets.NewChecker(...))`
- modify `apps/backend/internal/server/connector_integration_test.go` — type is now accepted
- modify `docs/02-api-contract.md` — `type` accepts `google_sheets`; health-check no longer universally 501
- modify `CLAUDE.md` — the layout gains `internal/adapter/`
- **run `make swagger`**

**Design points fixed here.**
- New top-level `internal/adapter/` package. It imports `connector` for the `Checker`
  interface; `connector` does not import it (the registry is assembled in `server.go`),
  so there is no cycle, and step 6+ can import the client without touching `connector`.
- `config.go` parses and validates the §3 shape and exposes
  `IsSpreadsheetAllowed(id)` / `IsFolderAllowed(id)`. **Every** tool calls these on
  every request against the stored config — never against a value cached from
  connector-creation time.
- **Use the official Google clients, not hand-rolled HTTP:**
  `google.golang.org/api/sheets/v4`, `google.golang.org/api/drive/v3`, and
  `golang.org/x/oauth2/google` for the token. Construct with
  `option.WithTokenSource(ts)` plus `option.WithHTTPClient(...)` for an explicit
  timeout. They are pure Go, so the distroless image is unaffected.
- Google access tokens are cached **in-process**, deliberately *not* in Redis — a
  refresh-derived access token is a live customer credential and Redis is not the
  right custody boundary for it. **`oauth2.ReuseTokenSource` already is this cache**:
  it holds the token in memory and refreshes on expiry. Keep one `TokenSource` per
  connector id in a mutex-guarded map; do not write a bespoke TTL cache.
- **Test seam — `api.go`.** Declare a narrow interface for exactly the four upstream
  operations the adapter needs, in the house style of `connectorStore` / `sealer`:
  ```go
  type sheetsAPI interface {
      SpreadsheetMeta(ctx context.Context, spreadsheetID string) (*Meta, error)
      Values(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error)
      ListFiles(ctx context.Context, folderID string, pageToken string) (*FilePage, error)
      File(ctx context.Context, fileID string) (*File, error)
  }
  ```
  `client.go` is the thin implementation over the Google SDKs. Unit tests in steps 6–9
  mock `sheetsAPI` and never touch the network. **One** contract test in
  `client_test.go` exercises the real wrapper against an `httptest` server via
  `option.WithEndpoint`, proving the mapping is right — that is the only place a
  Google response shape is asserted.
- `checker.Check` performs a token refresh plus one cheap metadata read, turning the
  501 stub into a real health check for this type.
- **Header-row handling** (spec open question 3, resolved here since `describe` needs
  it): row 1 is the header by default, with an optional per-spreadsheet override
  `scope.header_rows: {"<spreadsheetId>": 3}` in config. Cheap, additive, and real
  customer sheets do carry title rows above the header.

**Depends on.** Step 4 (ordering only). **Gated by Decision 2.**

**Verify.**
```
go test ./internal/adapter/googlesheets/... -count=1
go test ./internal/server/ -run TestConnector -count=1
curl -X POST localhost:3000/connectors/$ID/health-check -H "Authorization: Bearer $TOK" -H "x-organization-id: $ORG"
```
Allowlist tests must include the negative case explicitly: a spreadsheet id the OAuth
token *can* reach but the allowlist omits is rejected.

**Ground rules.** Decrypted config never leaves the owning service — the `Checker`
contract already states this, and the adapter must not log, retain, or return it.
No config field on any DTO.

---

### Step 6 — `sheets_list_spreadsheets` + `sheets_describe_spreadsheet`

**Goal.** The first real data tools, and the schema-discovery tool the spec calls
non-negotiable (§4.1) — without it an agent guesses ranges and fails every time.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/{list.go,describe.go}` + tests
- create `apps/backend/internal/module/mcp/tools_sheets.go` — catalog entries, `sheets:read`, `ReadOnlyHint: true`
- modify `apps/backend/internal/module/mcp/catalog.go`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md` — tool catalog table
- modify `docs/07-sheets-adapter-plan.md` — tick the step in the tracker

**Depends on.** Step 5.

**Verify.**
```
go test ./internal/adapter/googlesheets/... ./internal/module/mcp/... -count=1
npx @modelcontextprotocol/inspector    # describe a real allowlisted spreadsheet
```
Tests: `SPREADSHEET_NOT_ALLOWED` for a non-allowlisted id; tool invisible without
`sheets:read`; `include_sample_rows` bounded 0–5.

**Ground rules.** Tool and field descriptions are prompt surface (`docs/05`) — budget
review time for their wording. Audit `mcp.tool.called` records `spreadsheet_id` and
`sheet_name`, never cell values.

---

### Step 7 — `sheets_query_rows` (the workhorse)

**Goal.** The structured filter DSL of §4.2, with projection, pagination, result caps,
and formula-injection handling — the tool the product actually sells.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/query.go` + `query_test.go`
- create `apps/backend/internal/adapter/googlesheets/filter.go` + `filter_test.go` — `eq/neq/contains/gt/lt/gte/lte/in`
- modify `apps/backend/internal/module/mcp/tools_sheets.go`
- modify `apps/backend/internal/module/mcp/errors.go` — `COLUMN_NOT_FOUND`, `SHEET_NOT_FOUND`, `RESULT_TOO_LARGE`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md`

> ### ⚠️ Decision 4 — `total` is dropped in favour of a bounded scan
> **Confirm with the owner before starting this step.**
>
> Spec §4.2's example output has `"total": 340` — the count of *all* matching rows.
> That is not compatible with §6's "never load the whole sheet into memory", and the
> two cannot both be honoured: the Sheets API has no server-side filter and no count
> endpoint, so an exact total requires evaluating the filter over **every** row. At the
> spec's own target scale that is either unbounded memory or hundreds of upstream calls
> per tool invocation — which step 4's limiter would (correctly) cut off mid-answer.
>
> **Resolution: bounded scan with an honest signal.** Replace `total` with:
> ```jsonc
> { "count": 50, "offset": 0, "has_more": true, "next_offset": 50,
>   "scanned_rows": 5000, "scan_complete": false }
> ```
> `total` is emitted **only** when `scan_complete` is true (i.e. the scan reached the
> end of the sheet within budget), where it is exact and free. When `scan_complete` is
> false the tool description and the result text tell the agent the answer is partial
> and to narrow the filter — an agent that knows its count is a floor is strictly
> better off than one handed a confidently wrong total.
>
> This is a deliberate deviation from the spec's §4.2 example. It needs the owner's
> sign-off, and step 12 must write it back into `docs/06`.

**Design points fixed here.**
- **No Google Visualization Query Language is ever exposed** (§6). Filters are a
  structured DSL evaluated in our code; any agent-supplied value beginning `=`, `+`,
  `-`, or `@` is treated as a literal string and never interpolated into anything
  Google will evaluate.
- **Never load a whole sheet into memory** (§6) — 87 GB makes this load-bearing. The
  scan loop is the core of this step:
  1. Fetch a bounded page of rows (**5,000 rows per `Values.Get`**, tune later) via the
     `sheetsAPI` seam from step 5.
  2. Charge step 4's rate-limit bucket **one unit per page fetched**, not one per tool
     call. An empty bucket ends the scan early with `scan_complete: false`, not an error.
  3. Evaluate filters in-process, retaining at most `offset + limit + 1` matched rows —
     the `+1` is what sets `has_more` without a second pass.
  4. Stop at whichever comes first: enough matches, end of sheet, the scan budget
     (**50,000 rows**, configurable), or an empty bucket.
  Peak memory is one page plus the retained window, independent of sheet size.
- `limit` max 200, default 50. Response body cap ~256 KB → `RESULT_TOO_LARGE` telling
  the agent to use `columns` projection or narrow the filter.
- `response_format: markdown | json`, default markdown.

**Depends on.** Step 6.

**Verify.**
```
go test ./internal/adapter/googlesheets/ -run 'TestFilter|TestQuery' -count=1 -race
go test ./internal/server/ -run TestMCPQueryRows -count=1
```
Tests must include: a filter value of `=IMPORTRANGE(...)` round-trips as a literal and
reaches no evaluator; `limit=201` rejected; `has_more`/`next_offset` correct across a
page boundary; unknown column → `COLUMN_NOT_FOUND`; a mocked sheet larger than the scan
budget returns `scan_complete: false` with **no** `total` field and peak retained rows
≤ `offset + limit + 1`; an exhausted rate-limit bucket mid-scan ends the scan cleanly
rather than erroring.

**Ground rules.** Audit metadata carries `filter_columns[]` but **never filter
`value`s** (§7) — those are real business data (partner names, contract numbers).

---

### Step 8 — `sheets_read_range` (escape hatch)

**Goal.** Direct A1-range reads for the cases the DSL cannot express. The smallest
step in the plan; kept separate so step 7 stays reviewable.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/readrange.go` + test
- modify `apps/backend/internal/module/mcp/tools_sheets.go`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md`

**Depends on.** Step 7 (shares the cap/truncation helpers).

**Verify.** `go test ./internal/adapter/googlesheets/ -run TestReadRange -count=1`.
The A1 range must be parsed and re-validated against the allowlist — a range string is
agent-supplied input, not a trusted identifier.

---

### Step 9 — `drive_list_folder` + `drive_get_file`

**Goal.** The Drive half of the adapter, with short-lived links.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/drive.go` + `drive_test.go`
- create `apps/backend/internal/module/mcp/tools_drive.go` — `drive:read`
- modify `apps/backend/internal/module/mcp/catalog.go`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md`

**Design point.** `drive_get_file` returns a **signed URL with TTL ≤ 15 minutes**
(§4.3), never a permanent link — agent conversation logs are a leak channel.

**Depends on.** Step 5 (shares OAuth/allowlist); independent of steps 6–8.

**Verify.**
```
go test ./internal/adapter/googlesheets/ -run TestDrive -count=1
go test ./internal/server/ -run TestMCPDrive -count=1
```
Tests: folder outside the allowlist rejected; issued URL expires; tools invisible
without `drive:read` (and specifically **not** granted by `sheets:read`).

---

### Step 10 — Frontend: MCP key management

**Goal.** Let an owner mint and revoke keys without curl. The raw key is shown once,
with a copy affordance and an explicit "you will not see this again".

**Files.**
- create `apps/frontend/app/(dashboard)/mcp-keys/page.tsx`
- create `apps/frontend/lib/api/mcp-keys.ts`
- create `apps/frontend/components/mcp-keys/{key-list.tsx,create-key-dialog.tsx}`
- modify `apps/frontend/components/nav/*` — nav entry
- create `apps/frontend/app/(dashboard)/mcp-keys/page.test.tsx`

**Depends on.** Step 2.

**Verify.**
```
cd apps/frontend && pnpm lint && pnpm exec tsc --noEmit && pnpm test
```
Manual: mint → copy → use in Claude Code → revoke → next call 401.

**Ground rules.** Same-origin `/api/*` proxy only. The raw key is held in component
state and never written to `localStorage`.

---

### Step 11 — Frontend: `google_sheets` connector setup

**Goal.** A form for the paste-path credentials plus the allowlist, so onboarding is
a page rather than a curl transcript.

**Files.**
- create `apps/frontend/app/(dashboard)/connectors/[id]/google-sheets/page.tsx`
- create `apps/frontend/components/connectors/google-sheets-form.tsx`
- modify `apps/frontend/lib/api/connectors.ts`

**Depends on.** Steps 5, 10.

**Verify.** `pnpm lint && pnpm exec tsc --noEmit && pnpm test`; manual round-trip
create → health-check green → tool call succeeds.

**Ground rules.** `config` is write-only in the UI, because it is write-only in the
API — the form never renders a stored secret back, since no endpoint returns one.

---

### Step 12 — Docs consolidation + `docs/05` status update

**Goal.** Leave the design docs true. Small, but it is the step that stops the next
person re-deriving all of this.

**Files.**
- modify `docs/05-mcp-gateway.md` — Phase 1 and Phase 2 status
- modify `docs/06-sheets-adapter.md` — mark the four §10 questions resolved, with pointers, **and correct §4.2's example output to the bounded-scan shape** (Decision 4). Leaving the contradicted `total` in the spec is how the next reader re-introduces the bug.
- modify `CLAUDE.md` — MCP module, `internal/adapter/`, new env vars, new Redis keys
- modify `README.md` — MCP client setup quickstart
- move this plan to `.claude/plans/archives/` per repo convention

**Depends on.** Steps 1–11.

**Verify.** `make test && make lint`; `cd apps/frontend && pnpm lint`. Docs-only
otherwise — the check is that a reader can set up an MCP client from `README.md` alone.

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

Each with the earliest signal that would expose it.

| # | Risk | Earliest signal |
| - | ---- | --------------- |
| 1 | **The Go MCP SDK does not mount cleanly inside Echo.** The spike ran it standalone; nothing has run it behind Echo's middleware chain and `HTTPErrorHandler`. | Step 3, first Inspector `initialize`. This is why step 3 comes before any Google code — if the mount is wrong, only steps 1–2 are sunk. |
| 2 | **`87 GB` of spreadsheet defeats the Sheets API before it defeats us.** The API returns whole ranges; there is no server-side filter or index. Step 7's bounded scan keeps memory and quota safe, but it cannot make an unindexed scan *fast* — a selective filter over a huge sheet may simply exhaust its budget and return `scan_complete: false` every time, which is honest but not useful. | Step 7, first query against the customer's real largest sheet — not a fixture. Measure hit rate and scanned rows before declaring the tool done. If most real queries come back incomplete, the answer is a materialization/sync job into Postgres, which is a different plan and a bigger one. |
| 3 | **Google OAuth scope verification.** `drive.readonly` is a sensitive scope; a testing-mode project caps at 100 users and unverified apps can hit refresh-token expiry after 7 days. | Step 5, the first health check that survives a week. Test refresh-token longevity in a *published* testing project early — a 7-day expiry would make the paste path unusable and force Decision 2 open again. |
| 4 | **Header-row assumptions break on real sheets.** Merged cells, multi-row headers, and title banners are normal in Thai business spreadsheets. The override map handles a title row; it does not handle a two-row header. | Step 6, `describe` against real customer files. If two-row headers are common, `describe` needs a richer header model before step 7 builds on it. |
| 5 | **PAT scope intersection surprises users.** A key minted by an owner then demoted to member silently loses reach mid-session. Correct, and possibly confusing. | Step 3 integration tests make the semantics explicit; the real signal is the first support question. Documented behaviour, not a bug — but it needs to be *in* the docs (step 12). |
| 6 | **Rate limit of 60/min per connector is a guess.** Google's ~60 req/min/user and ~300 req/min/project are per-project, and one org may run several connectors against one Google project. | Step 4 onward. Watch for `UPSTREAM_AUTH_FAILED`/429 from Google despite our limiter passing — that means the limiter is scoped wrong (per connector, should be per Google project). |
