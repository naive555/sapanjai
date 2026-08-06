# Sapanjai MCP gateway — Phase 0 spike findings

**Date:** 2026-08-05 · **Verdict: feasible, no blockers.** The
`modelcontextprotocol/go-sdk` is at a stable **v1.7.0**, the transport that
Sapanjai needs (Streamable HTTP) supports per-request auth cleanly, and
controlplane's RBAC engine ports over without modification.

Two design decisions need making before production code (§4, §5). Everything
else is mechanical.

> This is the spike's own record — verification transcript, client-registration
> walkthrough, raw notes. The distilled version the backend works from is
> [`docs/05-mcp-gateway.md`](../../../docs/05-mcp-gateway.md), and **it is the
> authority** where the two disagree (its phase numbering supersedes §6's).

---

## 1. What was actually verified

Not "should work" — run, on this machine, with output captured.

| # | Claim | How verified |
| - | ----- | ------------ |
| 1 | MCP handshake completes, server identifies itself | `TestHandshake`; raw `initialize` over curl returned `serverInfo` + negotiated `protocolVersion` |
| 2 | Tool surface is a pure function of the RBAC grant | `TestToolVisibilityByPermission`, 4 principals covering owner-bypass / exact / `resource:*` / no-grant |
| 3 | Hidden tools are also un-callable | `TestDeniedToolIsNotCallable`, `TestMiddlewareDeniesEvenWhenRegistered` |
| 4 | Tenant isolation holds against a guessed id | `TestTenantIsolation` — org B asking for org A's `inv_1001` gets "not found" |
| 5 | Per-request auth on one HTTP endpoint | `TestStreamableHTTPPerRequestAuth` — 3 tokens, same URL, 3/2/0 tools |
| 6 | Bad credentials rejected before MCP | `TestStreamableHTTPRejectsBadToken`; curl gets HTTP 401 + `WWW-Authenticate` |
| 7 | **Registers in Claude Code over stdio** | `claude mcp add` → `claude mcp list` → `✔ Connected` |
| 8 | **Registers in Claude Code over HTTP** | `claude mcp add --transport http` with `--header` → `✔ Connected`; server log shows the bookkeeper principal resolved and all 3 tools granted |
| 9 | Thai UTF-8 survives the protocol | `บริษัท เจริญกิจ จำกัด` round-tripped intact through JSON-RPC |

`go build ./...`, `go vet ./...`, `gofmt -l .` and `go test ./...` are all
clean. 8 tests, 0 failures.

**Not verified: Claude Desktop.** It is not installed on this machine
(`%APPDATA%\Claude\` does not exist). The config format in §2 is documented
from the spec, not observed. Claude Code exercises the identical protocol over
both transports, so the risk is config-file shape only — but treat §2's
Desktop block as unconfirmed until someone runs it.

## 2. Registration — exact steps

### Claude Code, stdio (verified)

```bash
go build -o bin/sapanjai-mcp-stdio.exe ./cmd/stdio
claude mcp add sapanjai-spike --scope local \
  --env SAPANJAI_TOKEN=tok_reader_siam \
  -- /abs/path/to/bin/sapanjai-mcp-stdio.exe
claude mcp list        # -> sapanjai-spike: ... - ✔ Connected
```

### Claude Code, Streamable HTTP (verified)

```bash
go run ./cmd/httpsrv -addr 127.0.0.1:8090
claude mcp add sapanjai-http --scope local --transport http \
  http://127.0.0.1:8090/mcp \
  --header "Authorization: Bearer tok_bookkeeper_siam"
```

**Gotcha:** `--header` is variadic (`-H <header...>`), so it swallows any
argument after it. The URL must come *before* `--header`, or you get
`error: missing required argument 'commandOrUrl'`. Cost me a cycle.

### Claude Desktop (documented, unverified)

`%APPDATA%\Claude\claude_desktop_config.json` on Windows,
`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS:

```json
{
  "mcpServers": {
    "sapanjai": {
      "command": "C:\\path\\to\\sapanjai-mcp-stdio.exe",
      "env": { "SAPANJAI_TOKEN": "tok_reader_siam" }
    }
  }
}
```

Restart Desktop fully (tray icon → Quit; closing the window is not enough).
Remote HTTP servers go through **Settings → Connectors → Add custom
connector**, which expects the server to speak OAuth rather than accept a
static header — see §5.

### Other gotchas hit

- **stdout is the wire.** Any stray `fmt.Println` in a stdio server corrupts
  the JSON-RPC stream and the client drops the connection with an opaque parse
  error. All logging in `cmd/stdio` goes to stderr. This is the single easiest
  way to waste an afternoon.
- **Absolute paths only** in client config; the client does not inherit your
  shell's working directory.
- **`Accept` must list both** `application/json` and `text/event-stream` on
  HTTP POSTs, even when the server is configured for JSON responses. curl
  without it gets rejected.

## 3. Protocol constraints that affect the real integration

### 3.1 stdio cannot be multi-tenant — and it is the wrong product shape

A stdio server is one process per client, spawned by the client, with no
request headers. Identity must come from env vars or argv, fixed for the
process lifetime. That means:

- one principal per process — no per-request auth is even expressible;
- **the customer's credential sits in plaintext** in `claude_desktop_config.json`
  or `~/.claude.json`, which is not something to ship to accounting firms;
- you cannot revoke, rotate, meter, or audit centrally.

stdio is right for local dev and for a thin CLI shim. **The product is the
HTTP endpoint.** Keep `cmd/stdio` as a dev affordance and do not put it on the
pricing page.

### 3.2 Stateful HTTP pins the server instance at `initialize` — use stateless

`NewStreamableHTTPHandler` takes `getServer func(*http.Request) *Server`. In
stateful mode that is consulted when the session is created; later requests
carrying `Mcp-Session-Id` route to the existing session. A permission revoked
mid-session therefore would **not** take effect until reconnect.

`StreamableHTTPOptions{Stateless: true}` makes every POST self-contained: auth,
build the authorized surface, serve, discard. Verified — the response carries
no `Mcp-Session-Id`, and `tools/list` works with no prior `initialize`.

Three things fall out of this, all good:

- the request lifecycle becomes **identical to an Echo route**, so the existing
  guard/middleware mental model transfers exactly;
- **no sticky sessions**, so the k8s deployment scales horizontally with no
  session affinity and no shared session store;
- permission changes take effect on the next call, not the next reconnect.

Cost: no server→client requests (sampling, elicitation) and no resumable
streams. Sapanjai needs neither today. `JSONResponse: true` additionally drops
SSE framing for plain `application/json`, which is far easier to put behind a
load balancer.

**If a connector ever needs progress reporting** (a long ERP sync), that
requires stateful + SSE, which reintroduces sticky routing. Prefer designing
those as async: return a job id, poll with a second tool.

### 3.3 There is no per-request org header

Covered in depth in [RBAC-MAPPING.md](RBAC-MAPPING.md) — the short version is
that MCP client headers are static config, so `x-organization-id` has no
analogue and the org must be bound to the credential. This is the biggest
single deviation from the existing API model.

Do **not** solve it with a `switch_organization` tool: that puts tenant
selection inside the model's decision loop, where a prompt injection can reach
it.

### 3.4 15-minute JWTs are the wrong credential

Access tokens expire in 15 minutes. An agent session outlives that easily, and
MCP's mid-session 401 handling varies by client — the failure mode is an agent
that silently loses its tools mid-conversation.

Use **long-lived, revocable, org-scoped API keys** (§"What controlplane needs
to add" in RBAC-MAPPING.md), not the existing JWT pair. The Redis blacklist
infrastructure already in place gives instant revocation.

### 3.5 Errors have two channels, and the distinction matters

- **JSON-RPC error** (the Go handler's `error` return) — a hard failure; the
  model sees the turn abort.
- **`CallToolResult{IsError: true}`** — the model sees the text and can adapt.

Permission denials and not-founds should be `IsError`, so the model says "I
don't have access to that" rather than crashing the turn. That means
controlplane needs a *second* mapping alongside the existing
`apperror` → HTTP-status `HTTPErrorHandler`: `apperror` → tool result. Worth
writing once, centrally, when the `mcp` module lands.

### 3.6 Clients cache the tool list

`notifications/tools/list_changed` exists, but you cannot rely on every client
acting on it promptly. Hence two enforcement layers (§RBAC-MAPPING). Never
treat the tool list as an authorization boundary.

### 3.7 Smaller notes

- **Schema generation is free.** The generic `mcp.AddTool` derives input and
  output JSON Schema from Go types and validates inputs automatically;
  `jsonschema:"..."` struct tags become the descriptions the model reads. Tool
  and field descriptions are prompt surface — budget real review time for
  their wording, not just their types.
- **Protocol version negotiation works.** SDK v1.7.0 supports `2024-11-05`
  through `2026-07-28` and negotiates down; the curl test settled on
  `2025-06-18` because that is what it asked for.
- **Roots and sampling are deprecated** as of protocol `2026-07-28` (SEP-2577).
  Do not build on them.
- **No CORS concern.** MCP clients are not browsers. The existing "no CORS
  middleware and none should be added" rule survives — but note the MCP
  endpoint is a *new* public surface that the frontend proxy does not front.
- **Binary size:** 9 MB stdio / 12 MB HTTP, static. Drops into the existing
  distroless image unchanged.

## 4. Decision needed: credential + org binding

Recommend **org-scoped API keys** (`mcp_api_keys` table), one key per
(principal × org), with the org implied by the key. Rationale in
RBAC-MAPPING.md §"Where the org comes from". This is the decision that shapes
the schema, so it wants settling before code.

## 5. Decision needed: OAuth or not

Claude Desktop's custom-connector flow expects OAuth 2.0 with dynamic client
registration; a static `Authorization` header works in Claude Code but is not
the Desktop path. The SDK ships `auth.RequireBearerToken` and OAuth helpers,
so either is reachable.

Suggested sequencing: **static bearer keys first** (ship, get design partners
onto Claude Code / Cursor), **OAuth second** when Desktop's connector UI
becomes a real acquisition channel. Do not let OAuth block Phase 1.

## 6. Recommended integration approach

Phase 1, into controlplane proper:

1. **`internal/module/mcp`**, following the existing handler → service →
   queries convention. The handler delegates to `StreamableHTTPHandler` rather
   than returning JSON itself; the service holds the catalog and the
   permission filter.
2. **Mount at `POST /mcp`** on the existing Echo server, guarded by a new
   `RequireMCPKey` middleware that resolves an API key to
   `(userID, orgID, role, actions)` — the same tuple `RequireOrg` already
   builds, from a different credential.
3. **Port `internal/gateway` and `internal/tools` nearly verbatim.** The
   `internal/rbac` port goes away; call the real `rbac.Service.HasPermission`
   instead. That is the whole seam.
4. **Migration** `00007_mcp_api_keys.sql`, additive-forward. Seed the new
   `invoice:*` actions.
5. **Audit** `mcp.tool.called` / `mcp.tool.denied` from the enforcement
   middleware, best-effort per the existing rule.
6. **Contract** — add the MCP endpoint to `docs/02-api-contract.md`. It is a
   JSON-RPC envelope rather than REST, so it needs its own section shape.
7. **Keep `cmd/stdio`** as a dev-only shim.

The real work is not the protocol — that part is done and proven. It is the
**connector layer**: authenticating to PEAK / FlowAccount / Xero TH per tenant,
storing those upstream credentials safely, mapping their data models onto
stable tool schemas, and handling their rate limits and outages. Budget Phase 1
accordingly; MCP is the easy half.

## 7. Files

```
cmd/stdio/          stdio server (dev / Claude Desktop)
cmd/httpsrv/        Streamable HTTP server (the product shape)
internal/gateway/   BuildServer + EnforcePermissions — the two enforcement layers
internal/tools/     tool catalog; each tool declares its RBAC action
internal/rbac/      verbatim port of controlplane HasPermission
internal/principal/ token -> principal (the seam that becomes 2 real calls)
internal/mockdata/  fake Thai accounting upstream, 2 orgs
```
