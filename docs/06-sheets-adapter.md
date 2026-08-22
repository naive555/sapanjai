# Sapanjai — Google Sheets/Drive Adapter Spec (v0.1 draft)

> Status: draft, written to be used as input for a Claude Code prompt.
> Scope: **read-only MVP** — write tools are in §9 (not built yet).

---

## 1. Scope & assumptions

- Connector `type` = `google_sheets` (the skeleton currently has only `"generic"`)
- One adapter covers both Sheets + Drive — same OAuth scope, and they are naturally coupled (images in Drive, links in a Sheet)
- Target data scale: **~87GB / dozens of spreadsheet files** → pagination + result caps are a requirement, not a nice-to-have
- The end user is an AI agent (Claude Desktop/Code, Cursor) going through MCP — not a human calling directly

---

## 2. Endpoint & transport

```
POST /mcp/:connectorId        Streamable HTTP, stateless JSON
```

- **Streamable HTTP**, not stdio — because this is a remote multi-tenant gateway (which matches best practice: stdio is for local single-user setups)
- Stateless JSON (no session/SSE) — easier to scale, and it fits the existing k8s deployment
- `connectorId` sits in the path so that a single org with several Google accounts can keep separate endpoints

### ⚠️ Gap that has to be decided first: how does the MCP client authenticate?

The current access token lives 15 minutes — an MCP client (Claude Desktop) has no way to refresh it on its own, and there is no browser flow.

Options:

| Option                                                                                                          | Pros                                                              | Cons                                                                            |
| --------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| **A. Personal Access Token (PAT)** — new `api_keys` table, store the hash, `Authorization: Bearer sk_live_...` | Simplest; matches the mental model of an MCP config; revocable     | Requires a new module + migration                                               |
| **B. OAuth 2.1 per the MCP spec**                                                                               | Standard; natively supported by clients                            | Far more work; requires building an authorization server                        |
| **C. Stretch the refresh token and use it directly**                                                            | Nothing new to add                                                 | Violates the security model as designed (refresh tokens should be short-lived + rotate) — **not recommended** |

**Proposal: A for the MVP** — 1 migration + 1 small module. The PAT is bound to an org + a set of permissions (a subset of the creator's role).

---

## 3. Connector config shape (`google_sheets`)

Stored in `encrypted_config` via the existing envelope encryption — **no field ever surfaces in a response DTO or a log**

```json
{
  "oauth": {
    "refresh_token": "1//0g...",
    "client_id": "...apps.googleusercontent.com",
    "client_secret": "..."
  },
  "scope": {
    "spreadsheet_ids": ["1AbC...", "1XyZ..."],
    "drive_folder_ids": ["0B1a..."]
  }
}
```

**`scope` is the single most important security boundary** — the adapter must **reject** any spreadsheet/folder that is not in the allowlist, always, even when the OAuth token could reach it. Otherwise an agent that has been prompt-injected can read the entire Drive account.

---

## 4. Tool catalog

Naming: `{service}_{action}_{resource}` in snake_case, per MCP convention

| Tool                          | Permission    | readOnly | Summary                                          |
| ----------------------------- | ------------- | :------: | ------------------------------------------------ |
| `sheets_list_spreadsheets`    | `sheets:read` |    ✅    | List the spreadsheets in the allowlist           |
| `sheets_describe_spreadsheet` | `sheets:read` |    ✅    | Tabs + header row + row count (schema discovery) |
| `sheets_query_rows`           | `sheets:read` |    ✅    | Filter rows by column — **the primary tool**     |
| `sheets_read_range`           | `sheets:read` |    ✅    | Read an A1 range directly (escape hatch)         |
| `drive_list_folder`           | `drive:read`  |    ✅    | List files in an allowlisted folder              |
| `drive_get_file`              | `drive:read`  |    ✅    | Metadata + short-lived link                      |

### 4.1 `sheets_describe_spreadsheet` — the one you cannot go without

Sheets has **no schema** — an agent has no way to know what a tab is called or what each column means. Without this tool the agent guesses at ranges and fails every time. Always call this one first.

```jsonc
// input
{
  "spreadsheet_id": "string, required",
  "include_sample_rows": "int, 0-5, default 0"  // sample data helps the agent understand the format
}

// output
{
  "spreadsheet_id": "1AbC...",
  "title": "ระบบสัญญา 2568",
  "sheets": [
    {
      "name": "Contracts",
      "row_count": 12450,
      "columns": [
        {"index": 0, "letter": "A", "header": "contract_id"},
        {"index": 1, "letter": "B", "header": "partner_name"}
      ],
      "sample_rows": []
    }
  ]
}
```

### 4.2 `sheets_query_rows` — the workhorse

```jsonc
// input
{
  "spreadsheet_id": "string, required",
  "sheet_name": "string, required",
  "filters": [                                  // all ANDed together
    {"column": "partner_name", "op": "eq", "value": "หจก. ก่อสร้าง"},
    {"column": "status", "op": "in", "value": ["draft", "pending"]}
  ],
  "columns": ["contract_id", "status"],         // projection — cuts down the context sent back
  "limit": "int, 1-200, default 50",
  "offset": "int, default 0",
  "response_format": "markdown | json, default markdown"
}

// output — no "total" field: see "Bounded scan, not a count" below
{
  "count": 50, "offset": 0,
  "has_more": true, "next_offset": 50,
  "scanned_rows": 5000, "scan_complete": false,
  "rows": [{"contract_id": "C-0012", "status": "draft"}]
}
```

**Supported operators:** `eq`, `neq`, `contains`, `gt`, `lt`, `gte`, `lte`, `in`
**Not supported:** free-form expressions, formulas, raw query strings — deliberately (see §6)

#### Bounded scan, not a count

The Sheets API has no server-side filter and no count endpoint, so an exact
count of *every* matching row would mean evaluating the filter over the
whole sheet — either unbounded memory or hundreds of upstream calls per call,
which §6's "never load a whole sheet into memory" rule forbids at this
spec's own target scale. An earlier draft of this section showed an
unconditional `"total": 340` in the output above; that was dropped
(`docs/07-sheets-adapter-plan.md` §1 Decision 4, confirmed by the owner
2026-08-18) in favour of the honest, bounded shape actually implemented in
`internal/module/mcp/tools_sheets.go` (`queryRowsOutput`) and
`internal/adapter/googlesheets/query.go`:

- `scanned_rows` — how many of the sheet's data rows this call actually
  evaluated.
- `scan_complete` — `true` only if the scan reached the sheet's real end
  within this call's budget (a bounded page-fetch loop: pages of up to 5,000
  rows via `Values.Get`, stopping at whichever comes first among enough
  matches, the sheet's real end, a 50,000-row scan budget, or an exhausted
  rate-limit bucket).
- `total` — present **only when `scan_complete` is `true`**, in which case it
  is exact and free. When `scan_complete` is `false`, there is no `total`
  field at all, and both the tool description and the result text tell the
  agent that `count`/`scanned_rows` are a lower bound, not a final answer —
  narrow the filter or page forward with `next_offset` rather than assume
  nothing else matches.

See `docs/02-api-contract.md`'s `sheets_query_rows` entry for the full
scan-loop description and `internal/adapter/googlesheets/query_test.go` for
the tests proving the no-`total`-without-`scan_complete` invariant.

### 4.3 `drive_get_file`

Returns a **signed, short-lived URL (TTL ≤ 15 minutes)**, not a permanent link — this prevents links leaking out through the agent's conversation log.

---

## 5. RBAC mapping

Uses the existing semantics (`*` > `resource:verb` > `resource:*`); the matcher needs no changes.

| Permission                    | Grants                                        |
| ----------------------------- | --------------------------------------------- |
| `sheets:read`                 | Every `sheets_*` read tool                    |
| `drive:read`                  | Every `drive_*` read tool                     |
| `sheets:write`                | (phase 2)                                     |
| `connector:read/write/delete` | CRUD on the connector itself — existing, unchanged |

### ⚠️ Where the existing middleware does not apply directly

`RequirePermission(action)` is Echo middleware bound to a route — but **every MCP tool call arrives on the same route** (`POST /mcp/:connectorId`), and the required permission differs per tool named in the body.

The permission matcher has to be pulled out into a function callable from inside the handler:

```go
// internal/module/rbac — extract the logic the middleware currently uses, for reuse
func (s *Service) HasPermission(ctx, userID, orgID uuid.UUID, action string) (bool, error)
```

The MCP handler then calls it itself, per tool — and the existing middleware still calls that same function, so the logic is not duplicated.

**And `tools/list` must be filtered by the caller's permissions** — an agent should not see a tool it cannot call (it wastes context, and it leaks what capabilities exist).

---

## 6. Guardrails

### Query sanitization

Sheets has no SQL injection, but there are 2 real surfaces:

1. **Formula injection** — any value the agent sends in a filter that begins with `=`, `+`, `-`, or `@` must always be treated as a literal string, and must never be passed into Google Query Language
2. **Range traversal** — `spreadsheet_id` / `sheet_name` must be validated against the config's allowlist **every single time**, not just when the connector is created

**Decision: do not expose Google Visualization Query Language for the agent to write itself** — filters are a structured DSL (§4.2) only. This trades flexibility for an attack surface we can actually control.

### Rate limiting

Google Sheets API quota: ~300 req/min per project, ~60 req/min per user → an agent that calls in a loop will burn the whole org's quota.

- Token bucket in Redis: `mcp:ratelimit:<connectorId>` — proposed 60 req/min per connector, configurable
- This reuses the existing key convention directly; no new infra needed

### Result caps

- `limit` max 200, default 50
- Response body cap (proposed ~256KB) — if exceeded, truncate and tell the agent to use a `columns` projection or narrow the filter
- **Never load an entire sheet into memory** — at 87GB this is life-or-death, not a guideline

---

## 7. Audit events

Extends the existing audit log (best-effort; never fails the request, per the ground rule)

| Action                | Metadata                                                                                               |
| --------------------- | ------------------------------------------------------------------------------------------------------ |
| `mcp.session.started` | `connector_id`, `client_name`, `client_version`                                                        |
| `mcp.tool.called`     | `connector_id`, `tool`, `spreadsheet_id`, `sheet_name`, `filter_columns[]`, `row_count`, `duration_ms` |
| `mcp.tool.denied`     | `connector_id`, `tool`, `missing_permission`                                                           |
| `mcp.ratelimit.hit`   | `connector_id`, `tool`                                                                                 |

**Important: log the names of the columns being filtered, but never log the filter `value`s** — those are real business data (partner names, contract numbers). This matches the ground rule "log individual fields, never a whole struct".

---

## 8. Error codes

Returned inside the result object (`isError: true`), not as a protocol-level error, per the MCP spec — and each one must **tell the agent how to fix it**.

| Code                      | Suggested message                                                                       |
| ------------------------- | --------------------------------------------------------------------------------------- |
| `SPREADSHEET_NOT_ALLOWED` | Not in the allowlist — call `sheets_list_spreadsheets` to see what is reachable          |
| `SHEET_NOT_FOUND`         | That tab does not exist — call `sheets_describe_spreadsheet` first                       |
| `COLUMN_NOT_FOUND`        | No such column — check the headers via `sheets_describe_spreadsheet`                     |
| `RESULT_TOO_LARGE`        | Suggest adding a `columns` projection or lowering `limit`                                |
| `RATE_LIMITED`            | State the retry-after in seconds                                                         |
| `UPSTREAM_AUTH_FAILED`    | The OAuth refresh token has expired — the owner must re-authorize from the dashboard     |

---

## 9. Phase 2 (not built yet — noted so it isn't forgotten)

Write tools need more safety thinking first (confirmation flow, dry-run, rollback):

- `sheets_append_row` — `destructiveHint: false`, `idempotentHint: false`
- `sheets_update_cells` — `destructiveHint: true` ⚠️
- `drive_upload_file`
- LINE adapter (sending messages to partners) — **an action-type tool, much higher risk than a read**; should come after reads are stable

---

## 10. Questions still to be decided

All four resolved by the owner 2026-08-18 (`docs/07-sheets-adapter-plan.md`
§1 Decisions 1–4); this section is kept as a record of what was asked and
where each answer actually lives in code, not as an open question anymore.

1. **MCP client auth — resolved: Personal Access Tokens (option A).**
   `docs/07-sheets-adapter-plan.md` §1 Decision 1. One migration
   (`apps/backend/migrations/00008_mcp_api_keys.sql`) and one module,
   `internal/module/mcpkey` (`mcp_api_keys` table, `sk_live_...` tokens
   hashed with SHA-256, `scopes text[]` intersected with the creator's live
   RBAC grant on every request). `POST /mcp-keys` returns the raw token
   exactly once; `GET`/`DELETE /mcp-keys` list/revoke. A dashboard page at
   `/mcp-keys` mints and revokes keys without curl
   (`apps/frontend/app/(dashboard)/mcp-keys/page.tsx`).
2. **OAuth onboarding flow — resolved: manual credential paste for the MVP.**
   `docs/07-sheets-adapter-plan.md` §1 Decision 2. No OAuth consent page
   exists; a customer's `client_id`/`client_secret`/`refresh_token` are
   pasted into `POST /connectors`' `config` field (already
   envelope-encrypted, already never echoed back), via the
   `/connectors/:id/google-sheets` form
   (`apps/frontend/components/connectors/google-sheets-form.tsx`). Onboarding
   is a support conversation, not self-service, until a consent flow lands —
   see `docs/07` §3 "Out of scope" for its trigger condition.
3. **Header row detection — resolved: configurable per spreadsheet, row 1
   default.** Resolved in `docs/07-sheets-adapter-plan.md` step 5. The
   connector config's `scope.header_rows` is an optional
   `{"<spreadsheet_id>": <row>}` map (§3 above); `Config.HeaderRow` in
   `internal/adapter/googlesheets/config.go` returns the override when one
   exists, otherwise row 1. Verified against the code: `HeaderRow` reads
   `Scope.HeaderRows[spreadsheetID]`, falling back to `1` when absent or
   `<= 0`.
4. **Multi-tab join — resolved: the agent joins results itself in the MVP.**
   No workflow/join tool was built — the catalog is exactly the six read
   tools in §4 above, calling `sheets_describe_spreadsheet` then one or more
   `sheets_query_rows` and combining results is left to the agent. Recorded
   as deliberately out of scope in `docs/07-sheets-adapter-plan.md` §3, with
   its trigger to revisit: "if transcripts show agents failing at it."
