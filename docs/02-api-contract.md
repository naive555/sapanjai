# API Contract — parity target for the Go backend

> The Go backend must match this contract exactly — paths, methods, headers, status codes, bodies. This is the reference clients are written against; change the contract here first, then the code.

## Conventions

### Headers

| Header              | Direction | Description                                            |
| ------------------- | --------- | ------------------------------------------------------ |
| `Authorization`     | request   | `Bearer <accessToken>` — required on guarded routes    |
| `x-organization-id` | request   | Active org context — required on org-scoped routes     |
| `x-request-id`      | both      | Client-supplied or server-generated UUID, always echoed back |

### Guard levels

- **public** — no auth
- **auth** — valid, non-blacklisted access JWT
- **org** — auth + `x-organization-id` header + caller is a member of that org
- **perm:`<action>`** — org + RBAC permission check. First used by the Connectors routes below: the permission check runs *before* membership is resolved, so a non-member gets 403 `Missing permission: <action>`, never `Not a member of this organization`.
- **platform:`<role…>`** — auth + `users.platform_role` is one of the listed roles, read **fresh from the database on every request** and never carried as a JWT claim (`docs/11-admin-panel.md` D1). Used only by the `/admin` routes below. There is no `x-organization-id` and no membership check: these routes read across every organization, and platform staff typically hold no membership anywhere. A caller with no platform role, or with one not in the list, gets 403 `Insufficient permissions` — deliberately the same wording `FORBIDDEN` uses, so the console cannot be probed for which platform roles exist.

### Error responses

Guard failures:

| Condition                          | Status | Message                          |
| ---------------------------------- | ------ | -------------------------------- |
| Missing/invalid/expired token      | 401    | `Unauthorized`                   |
| Blacklisted token                  | 401    | `Token revoked`                  |
| Missing `x-organization-id`        | 400    | `Missing x-organization-id header` |
| Not a member of the org            | 403    | `Not a member of this organization` |
| Missing RBAC permission            | 403    | `Missing permission: <action>`   |
| Banned user, any auth-guarded route | 401   | `Account suspended`              |
| Missing/insufficient platform role | 403    | `Insufficient permissions`       |

Service error map (service throws code → HTTP response):

| Code                    | Status | Message                                          |
| ----------------------- | ------ | ------------------------------------------------ |
| `EMAIL_TAKEN`           | 409    | Email already taken                              |
| `INVALID_CREDENTIALS`   | 401    | Invalid email or password                        |
| `TOO_MANY_ATTEMPTS`     | 429    | Too many login attempts, try again in 15 minutes |
| `INVALID_REFRESH_TOKEN` | 401    | Invalid refresh token                            |
| `REFRESH_TOKEN_REUSE`   | 401    | Refresh token reuse detected                     |
| `REFRESH_TOKEN_EXPIRED` | 401    | Refresh token expired                            |
| `ACCOUNT_SUSPENDED`     | 403    | Account suspended                                |
| `SLUG_TAKEN`            | 409    | Organization slug already taken                  |
| `USER_NOT_FOUND`        | 404    | User not found                                   |
| `ALREADY_MEMBER`        | 409    | User is already a member                         |
| `MEMBER_NOT_FOUND`      | 404    | Member not found                                 |
| `CANNOT_REMOVE_OWNER`   | 403    | Cannot remove organization owner                 |
| `LIMIT_EXCEEDED`        | 403    | Plan limit exceeded                              |
| `ROLE_NOT_FOUND`        | 404    | Role not found                                   |
| `FORBIDDEN`             | 403    | Insufficient permissions                         |
| `NOT_FOUND`             | 404    | Resource not found                               |
| `CONNECTOR_NAME_TAKEN`  | 409    | Connector name already taken                     |
| `INVALID_CONNECTOR_TYPE`| 422    | Unsupported connector type                       |
| `HEALTH_CHECK_UNSUPPORTED` | 501 | Health check not supported for this connector type |
| `MCP_KEY_NOT_FOUND`     | 404    | MCP key not found                                |
| `MCP_KEY_NAME_TAKEN`    | 409    | MCP key name already taken                       |
| `RATE_LIMITED`          | 429    | Rate limit exceeded, try again later             |
| `ALREADY_VERIFIED`      | 409    | Email already verified                           |
| `INVALID_VERIFICATION_TOKEN` | 400 | Invalid or expired verification token          |
| `VERIFICATION_RESEND_TOO_SOON` | 429 | Verification email already sent, try again in a few minutes |
| `INVALID_RESET_TOKEN`   | 400    | Invalid or expired password reset token          |
| (unknown)               | 500    | Internal server error                            |

There is deliberately no `RESET_TOO_SOON` code: `POST /auth/forgot-password`
always returns `200 { success: true }`, whether or not the address belongs to
an account and whether or not its 15-minute resend cooldown is currently
active — a distinguishable response for "cooldown active" would itself be the
enumeration oracle the uniform response exists to close.

A banned account is rejected with **403 `ACCOUNT_SUSPENDED` from `POST /auth/login`** but **401 `Account suspended` from the guard** on an already-issued access token. The asymmetry is intentional: at login the credential is valid and the account is not, while mid-session the credential itself is no longer usable and the frontend's existing 401 path clears the session cleanly. Do not unify them. `RequireMCPKey` is a third case and stays silent about the reason — a banned key owner gets the same indistinguishable 401 as a revoked, expired, or unknown key (`docs/11-admin-panel.md` §4).

`RATE_LIMITED` has exactly one definition (`apperror.Map`, for a future REST
caller) but no REST route emits it today — the MCP gateway's rate limiter
(below) never surfaces it as an HTTP status at all, since a `tools/call`
denial is a `CallToolResult{ IsError: true }`, not an HTTP error response.

Global: unknown route → 404 `Route not found`; body validation failure → 422 `Validation failed`; malformed JSON → 400 `Invalid request body`.

## Endpoints

### Health

| Method/Path | Guard  | Response |
| ----------- | ------ | -------- |
| `GET /health` | public | `{ status: "ok", uptime: <seconds> }` |

### Auth (`/auth`)

| Method/Path      | Guard  | Body | Response |
| ---------------- | ------ | ---- | -------- |
| `POST /auth/register` | public | `{ email: email, password: min 8, displayName?: min 1 }` | `{ accessToken, refreshToken }` |
| `POST /auth/login`    | public | `{ email: email, password }` | `{ accessToken, refreshToken }` |
| `POST /auth/refresh`  | public | `{ refreshToken }` | `{ accessToken, refreshToken }` (rotated; access token claims: `sub` only) |
| `POST /auth/logout`   | public (reads Authorization if present) | `{ refreshToken }` | `{ success: true }` — blacklists access token 15 min, revokes ALL user sessions |
| `POST /auth/verify-email` | public | `{ token }` | `{ success: true }` — consumes a single-use, Redis-backed verification token (24h TTL) and marks the owning user verified. `POST`, not `GET`: the frontend page at `/verify-email?token=...` is the GET target and POSTs the token here, so a link-scanner prefetching the page never burns it. `400 INVALID_VERIFICATION_TOKEN` for an unknown, expired, or already-consumed token — indistinguishable from a token naming a user that no longer exists. Idempotent for an already-verified user with a still-live token: returns success with no second audit write. |
| `POST /auth/resend-verification` | auth | — | `{ success: true }` — re-sends the verification email for the caller, subject to a 5-minute cooldown (`404 USER_NOT_FOUND`, `409 ALREADY_VERIFIED`, `429 VERIFICATION_RESEND_TOO_SOON`). |
| `GET /auth/me` | auth | — | `{ id, email, displayName, isVerified, createdAt }` — the caller's own profile, read fresh from the database on every call. `isVerified` is deliberately **not** carried in the JWT claims (see below): a claim would go stale for up to `JWT_ACCESS_EXPIRES_IN` after verifying. |
| `POST /auth/forgot-password` | public | `{ email: email }` | **Always** `{ success: true }` — never distinguishes an unknown address, a known one, or one whose 15-minute resend cooldown (keyed by email, not user id — the address isn't known to belong to a user until after the cooldown check) is currently active. On a known, non-cooldown address it generates a single-use, Redis-backed password-reset token (1h TTL) and enqueues the reset email. |
| `POST /auth/reset-password` | public | `{ token, password: min 8 }` | `{ success: true }` — consumes the single-use reset token (`400 INVALID_RESET_TOKEN` for an unknown, expired, or already-consumed token, indistinguishable from a token naming a user that no longer exists) and, in one transaction, updates the password, marks the user verified (reaching the link proves mailbox control — same call the plan makes for `POST /auth/verify-email`), and **revokes every session for that user**. Already-issued access tokens are unaffected and remain valid until their own (short) expiry; only refresh sessions die immediately. |

JWT claims — access: `{ sub: userId, email }`, HS256, exp = `JWT_ACCESS_EXPIRES_IN` (default 15m). Refresh: `{ sub: userId }`, separate secret, no embedded exp in source (session row `expires_at` = now + `JWT_REFRESH_EXPIRES_IN` seconds, default 604800).

Registration enqueues a verification email in the same transaction as the user insert (`internal/infra/database/queries/email_outbox.sql`'s `EnqueueEmail`, drained by the worker's `email-dispatch` job — see the Background worker bullet in CLAUDE.md) — a user row can never exist without its verification email queued, and vice versa. Verification tokens and their 5-minute resend cooldown live only in Redis (`verify:email:<sha256hex(token)>`, `verify:resend:<userId>`), never in Postgres — see CLAUDE.md's Redis key conventions. Password-reset tokens and their 15-minute resend cooldown follow the same pattern in a second key namespace (`reset:password:<sha256hex(token)>`, `reset:request:<email>` — keyed by email rather than user id, since `forgot-password` runs before the address is known to belong to a user).

### Organizations (`/organizations`)

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /organizations` | auth | `{ name: min 1, slug: min 2, ^[a-z0-9-]+$ }` | Creates org + owner membership for caller; returns org row. 409 `SLUG_TAKEN`. |
| `GET /organizations` | auth | — | Caller's memberships with embedded organization objects. |
| `GET /organizations/members` | org | — | Active org's member roster: `[{ userId, email, displayName, role, joinedAt }]`, ordered by membership creation time. **Not in the source app** — added in Phase 6 for the frontend members page (see `docs/03` open question #2). |
| `POST /organizations/invite` | org | `{ email: email, role: "admin"\|"member" }` | Caller's membership role must not be `member`. Enforces `max_members` plan limit. Target user must exist and not already be a member. Returns `{ success: true }`. |
| `DELETE /organizations/members/:userId` | org | — | Caller role must not be `member`; target must exist; cannot remove `owner`. Returns `{ success: true }`. |

### RBAC (`/rbac`)

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `GET /rbac/roles` | org | — | List org's custom roles. |
| `POST /rbac/roles` | org | `{ name: min 1, description?, permissions: string[] }` | Create role + set permissions; returns role row. |
| `PUT /rbac/roles/:roleId/permissions` | org | `{ permissions: string[] }` | Replace role's permission set. Role must exist and belong to org. `{ success: true }`. |
| `POST /rbac/assign` | org | `{ userId, roleId }` | Assign custom role to a member's membership. `{ success: true }`. |

Permission semantics: `*` grants everything; exact `resource:verb` match; `resource:*` wildcard matches any verb on that resource.

### Audit logs (`/audit-logs`)

| Method/Path | Guard | Query | Behavior |
| ----------- | ----- | ----- | -------- |
| `GET /audit-logs` | org | `userId?`, `action?` (repeatable — `?action=a&action=b` matches either; a single `action=x` behaves as before), `since?` (RFC3339 timestamp, inclusive lower bound on `createdAt`), `limit?` (1–100, default 50) | Org's logs, newest first. |

Recorded actions: `user.login`, `user.register`, `org.created`, `org.member.invited`, `org.member.removed`, `role.created`, `role.assigned` (last three defined but only the first four are currently written), plus `connector.created`, `connector.updated`, `connector.deleted` (all three written, from the Connectors module below), plus `mcp.session.started`, `mcp.tool.called`, `mcp.tool.denied`, `mcp.ratelimit.hit`, `mcp.file.downloaded` (all five written, from the MCP gateway below), plus `user.email_verified` (written by `POST /auth/verify-email`, from the Auth section above), plus `user.password_reset_requested` and `user.password_reset` (written by `POST /auth/forgot-password` and `POST /auth/reset-password` respectively, from the Auth section above).

### Subscription (`/subscription`)

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `GET /subscription` | org | — | Org's subscription incl. plan (nullable if none). |
| `POST /subscription/assign` | org | `{ planId }` | Upsert org subscription. ⚠️ Source has no admin check — see quirks in 01-source-analysis.md. |
| `GET /plans` | auth | — | All available plans: `[{ id, name, limits, createdAt }]`. **Not in the source app** — added in Phase 6 so the frontend subscription page can populate a plan picker (plan ids are server-generated UUIDs with no fixed/knowable value otherwise). Global, not org-scoped, so `auth` not `org` guard. |

### Connectors (`/connectors`)

Org-scoped upstream connections (DB creds, API keys, ...) for the Managed MCP
Gateway product (`docs/05-mcp-gateway.md`). `type` accepts `"generic"` (the
skeleton placeholder — no adapter, health-check always 501) and
`"google_sheets"` (the first real adapter, `internal/adapter/googlesheets`,
`docs/07-sheets-adapter-decisions.md` step 5) — more per-type integrations
(FlowAccount, PEAK, ...) land the same way.

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /connectors` | perm:`connector:write` | `{ name: 1-100, type: "generic" \| "google_sheets", config: object }` | Seals `config` at rest with envelope encryption (fresh AES-256 data key per row, wrapped by `CONNECTOR_MASTER_KEY`) and stores it; new rows start `status: "inactive"`. Enforces the `max_connectors` plan limit. 409 `CONNECTOR_NAME_TAKEN` (unique per org); 422 `Validation failed` for an unrecognized `type` (the request validator rejects it before the service's own `INVALID_CONNECTOR_TYPE` check is ever reached). `config` is not shape-validated against the type at create time — an unparsable `google_sheets` config still creates the row; it only surfaces as a failed health-check. |
| `GET /connectors` | perm:`connector:read` | — | Org's connectors, oldest first. |
| `GET /connectors/:connectorId` | perm:`connector:read` | — | One connector. 404 `NOT_FOUND` for another org's id — indistinguishable from a nonexistent one. |
| `PATCH /connectors/:connectorId` | perm:`connector:write` | `{ name?, status?, config? }` | Partial update; unset fields are left unchanged. A supplied `config` is re-sealed under a brand-new data key (the old ciphertext is overwritten, not versioned). `type` is immutable — there is no `type` field to patch. |
| `DELETE /connectors/:connectorId` | perm:`connector:delete` | — | `{ success: true }`. 404 `NOT_FOUND` if already gone or not this org's. |
| `POST /connectors/:connectorId/health-check` | perm:`connector:write` | — | Probes the upstream and records `status`/`lastHealthCheckAt`. For `type: "generic"` (no checker registered) this always returns 501 `HEALTH_CHECK_UNSUPPORTED` and leaves the row untouched. For `type: "google_sheets"`, `googlesheets.Checker` parses `config`, refreshes the OAuth token, and reads metadata for the first allowlisted spreadsheet (or lists the first allowlisted Drive folder if none is allowlisted) — success writes `status: "active"`, any failure (bad config shape, expired/invalid refresh token, upstream error) writes `status: "error"` and still returns 200 with the updated row; the probe error itself is never returned to the caller or logged with credential material. Gated by `connector:write` (not `:read`) because it writes to the row. |

**`google_sheets` config shape** (sealed the same as any other connector's
`config` — never returned by any endpoint):

```jsonc
{
  "oauth": { "refresh_token": "1//0g...", "client_id": "...apps.googleusercontent.com", "client_secret": "..." },
  "scope": {
    "spreadsheet_ids": ["1AbC...", "1XyZ..."],
    "drive_folder_ids": ["0B1a..."],
    "header_rows": { "1AbC...": 3 }
  }
}
```

`scope` is the security boundary: `spreadsheet_ids` / `drive_folder_ids` is
an allowlist the adapter enforces on every call, independent of whatever the
OAuth token itself can reach — an id absent from the allowlist is always
rejected. At least one of `spreadsheet_ids` / `drive_folder_ids` must be
non-empty. `header_rows` is an optional per-spreadsheet override for the
header row (default: row 1) — real customer sheets often carry a title
banner above the real header. Onboarding is manual credential paste for the
MVP (`docs/07-sheets-adapter-decisions.md` §1 Decision 2) — no OAuth consent flow
in the dashboard yet.

Response shape (all endpoints except `DELETE`, which returns `{ success: true }`):

```
{ id, organizationId, name, type, status, lastHealthCheckAt, createdAt, updatedAt }
```

**`config` is never returned by any endpoint, ever** — not on create, not on
update, not on get. The decrypted config exists only transiently inside the
service (to seal it on write, or to hand it to a health-check `Checker`); no
DTO or log line carries it.

### MCP keys (`/mcp-keys`)

Org-scoped, revocable Personal Access Tokens (PATs) that MCP clients present
as a bearer credential (`docs/07-sheets-adapter-decisions.md` Decision 1).
`scopes` is enforced by the MCP gateway below: it intersects a key's
(nullable) `scopes` with the creator's live RBAC grant, re-resolved on every
request — a key can only ever narrow that grant, never widen it.

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /mcp-keys` | perm:`mcpkey:write` | `{ name: 1-100, expiresInDays?: int ≥ 1, scopes?: string[] }` | Mints a token of the form `sk_live_<base64url(32 random bytes)>` and stores only its SHA-256 hash (`key_hash`, unique-indexed) — never bcrypt, and deliberately so: the token is 256 bits of CSPRNG output, not a human-chosen password, so brute force is moot and a lookup-by-hash needs a deterministic digest. `expiresInDays`, if given, sets an absolute `expires_at`; omitted means the key never expires. `scopes` is three-state: omitted or `null` mints an unrestricted key that rides the creator's live RBAC grant (today's default behavior, unchanged); a non-empty list of `*`/`resource:verb`/`resource:*` action strings narrows the key to that list, re-intersected with the creator's *live* grant on every gateway request — never widened, and not validated against the creator's grant at mint time (a scope the creator does not currently hold is simply inert, not rejected — see `docs/08-gateway-core.md` §4); `scopes: []` is rejected with 422, since a key scoped to nothing is a configuration mistake, not a use case. Each scope string must match `*` or `<resource>:<verb>`/`<resource>:*` (`[a-z][a-z0-9_]*` for resource/verb) or the request 422s. 409 `MCP_KEY_NAME_TAKEN` (unique per org). **The raw token is returned in this response's `apiKey` field and nowhere else** — it is never stored, never logged, and cannot be recovered later. |
| `GET /mcp-keys` | perm:`mcpkey:read` | — | Org's keys, oldest first. Never includes `key_hash` or the raw token. |
| `DELETE /mcp-keys/:keyId` | perm:`mcpkey:delete` | — | Sets `revoked_at`; `{ success: true }`. Idempotent — revoking an already-revoked key re-stamps `revoked_at` and still succeeds. 404 `MCP_KEY_NOT_FOUND` for another org's id — indistinguishable from a nonexistent one. |

Response shapes:

```
POST /mcp-keys  -> { id, name, apiKey, expiresAt, createdAt }
GET  /mcp-keys  -> [{ id, organizationId, userId, name, scopes, lastUsedAt, expiresAt, revokedAt, createdAt }]
```

### MCP gateway (`POST /mcp/:connectorId`)

Not a REST route — one Streamable HTTP MCP endpoint per connector, speaking
the [Model Context Protocol](https://modelcontextprotocol.io) JSON-RPC 2.0
envelope over a single `POST`. This section describes the envelope and
gateway-specific behavior rather than a request/response table, per
`docs/05-mcp-gateway.md`. Implemented in `docs/07-sheets-adapter-decisions.md`
step 3, which proves the full authorization path (PAT → org → connector →
RBAC-filtered tool list → `tools/call` → audit) against one trivial tool
before any Google API code exists.

**Transport.** `mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}`:
every `POST` is self-contained (no `Mcp-Session-Id`, no server-side session
table), and the response is plain `application/json` rather than SSE
framing. `Content-Type: application/json` and `Accept:
application/json, text/event-stream` are both required on every request (the
SDK rejects a POST missing either). `GET`/`DELETE` on this route return 405.

**Auth.** `Authorization: Bearer sk_live_...` — an MCP key minted via
`POST /mcp-keys` above, **not** a JWT access token. Rejections are always
401 `{ "message": "Unauthorized" }` with a `WWW-Authenticate` header set (so
MCP clients offer to re-authenticate instead of failing silently):
`Bearer realm="sapanjai"` when no token was presented at all,
`Bearer realm="sapanjai", error="invalid_token"` when one was presented but
rejected (unknown hash, revoked, or expired — these three are
indistinguishable in the response body, by design, so a probing client
learns nothing about which reason applied). The org is bound to the key,
never supplied by the client — MCP clients have no per-request header
analogous to `x-organization-id` (`docs/05-mcp-gateway.md`).

**Connector resolution.** `:connectorId` is resolved scoped to the key's
organization. A connector belonging to another org is 404
`{ "message": "Resource not found" }` — byte-identical to a nonexistent id,
so the id cannot be used to probe for another tenant's connectors.

**RBAC filtering — two layers, both enforced on every request:**

1. **Construction-time.** A fresh `*mcp.Server` is built per request, and
   only tools the caller's principal is permitted to use are registered on
   it — an unpermitted tool never appears in `tools/list` and calling it
   anyway gets the SDK's own "unknown tool" protocol error.
2. **Call-time.** An `mcp.Middleware` re-checks the RBAC action on every
   `tools/call` and scrubs `tools/list`, so a permission change takes effect
   on the very next call rather than the next reconnect — the tool list is
   never itself an authorization boundary.

The principal is the caller's live RBAC grant (`rbac.Service.Authorize`,
re-resolved on every request — never cached on the key) intersected with the
key's own `scopes` when non-`NULL` (Decision 1). An owner-held key with a
non-`NULL` `scopes` list is narrowed the same way a member's is: the owner
bypass does not survive scoping, or a scoped key held by an owner would
silently grant everything.

**Denials.** A missing-permission `tools/call` returns
`CallToolResult{ IsError: true, Content: [{ text: "Missing permission: <action>" }] }`
— a normal tool result, not a JSON-RPC error, so the model sees the refusal
and can adapt rather than the turn aborting. The text is byte-identical to
the REST 403 body above.

**Tool catalog (step 6):** every entry also carries a connector-type gate —
a tool is only ever registered against a connector of its own type, checked
alongside `Permission` before `tools/list` or `tools/call` can reach it, so a
`google_sheets`-only tool never appears against a `generic` connector (or
vice versa) regardless of what the caller is permitted to do.

| Tool | Connector type | Permission | Description | Returns |
| ---- | --------------- | ---------- | ------------ | ------- |
| `sapanjai_describe_connector` | any | `connector:read` | Describes the connector this session is bound to. Takes no arguments — the connector is fixed by the URL, not model-supplied. | `{ name, type, status }` — structurally incapable of returning `config`; the decrypted connector config never leaves `connector.Service`, same invariant as the REST `/connectors` routes. |
| `sapanjai_whoami` | any | `connector:read` | Reports the caller's own organization, the display name of the PAT this session authenticated with, and its resolved permission list — the actions actually granted after intersecting the key's own `scopes` with its creator's live role. Takes no arguments. | `{ organizationId, keyName, permissions: [string] }` — `permissions` is `["*"]` for an unscoped key riding an owner's live bypass, `[]` (never `null`) for a principal with no resolved actions, and the resolved action list otherwise; structurally incapable of returning a credential, config, or a key id/hash. |
| `sheets_list_spreadsheets` | `google_sheets` | `sheets:read` | Lists every spreadsheet the connector's own allowlist (`config.scope.spreadsheet_ids`) grants access to, with each one's title. The OAuth account behind the connector may be able to reach other spreadsheets too; only allowlisted ones are ever returned. Takes no arguments. | `{ spreadsheets: [{ spreadsheet_id, title, accessible }] }` — `accessible` is `false` for an allowlisted id the OAuth token can no longer read (a revoked share, a deleted file); reported per-item rather than failing the whole call. |
| `sheets_describe_spreadsheet` | `google_sheets` | `sheets:read` | Schema discovery (docs/06-sheets-adapter.md §4.1): one spreadsheet's title, every tab's name and row/column count, and each tab's column headers, optionally with a few sample data rows. Sheets has no schema, so this is meant to be called before `sheets_query_rows`/`sheets_read_range`. Input: `{ spreadsheet_id: string (required), include_sample_rows: int 0-5, default 0 }`. | `{ spreadsheet_id, title, sheets: [{ name, row_count, column_count, columns: [{ index, letter, header }], sample_rows: [[string]] }] }` |
| `sheets_query_rows` | `google_sheets` | `sheets:read` | The workhorse (step 7): filters one sheet's data rows by a structured column DSL (`eq`/`neq`/`contains`/`gt`/`lt`/`gte`/`lte`/`in`, AND-ed together), with column projection and offset/limit pagination. No Google Visualization Query Language is ever exposed — a filter value is always compared as literal text or a plain number, never evaluated as a formula, regardless of a leading `=`/`+`/`-`/`@`. Input: `{ spreadsheet_id, sheet_name, filters?: [{ column, op, value }], columns?: [string], limit?: int 1-200 default 50, offset?: int default 0, response_format?: "markdown" \| "json" default "markdown" }`. | See "Bounded scan" below for the response shape. |
| `sheets_read_range` | `google_sheets` | `sheets:read` | The escape hatch (step 8): reads an explicit A1 range directly, for whatever `sheets_query_rows`' filter DSL cannot express — no filtering, no projection. The range is parsed and re-validated in our own code, never handed to Google opaquely; it must carry explicit numeric row bounds on both ends (a bare sheet name, or a column-only range like `A:D`, is rejected as unbounded — a row-only range like `1:100` is not, since its rows are still explicitly bounded). An omitted sheet name resolves to the spreadsheet's first tab. Input: `{ spreadsheet_id: string (required), range: string (required) }`. | `{ spreadsheet_id, range, sheet_name, columns: [string], rows: [[string]], row_count, column_count }` — `range` is always the fully resolved range actually read (sheet name and column bounds filled in); `rows` is padded to a rectangle `column_count` wide, never ragged. |
| `drive_list_folder` | `google_sheets` | `drive:read` | The Drive half of the adapter (step 9): lists the files directly inside one of this connector's allowlisted Drive folders (`config.scope.drive_folder_ids`) — `folder_id` must be one of those, checked before any network call. Only *direct* children are listed; a subfolder's own contents are not traversed. `drive:read` is a distinct permission from `sheets:read` — neither grants the other. Input: `{ folder_id: string (required), page_token?: string }`. | `{ folder_id, files: [{ file_id, name, mime_type, size_bytes, modified_time }], next_page_token, has_more }` — results are capped per call; `has_more`/`next_page_token` page forward. |
| `drive_get_file` | `google_sheets` | `drive:read` | One Drive file's metadata by id, plus — for non-Google-native files — a short-lived, replayable download link (no consumption tracking — anyone holding it can use it until it expires) (see "Signed download link" below). Unlike every other tool here, the allowlist can't be checked until the file's metadata is fetched (a bare `file_id` carries no folder context), so this tool always reaches the network before it can reject an out-of-scope file. Input: `{ file_id: string (required) }`. | `{ file_id, name, mime_type, size_bytes, modified_time, download_url?, download_url_expires_at? }` — `download_url` is omitted for a Google-native file (a Doc/Sheet/Slide — no raw bytes to download) or when link minting is disabled (empty `CONNECTOR_MASTER_KEY`). |

**`sheets_query_rows`'s bounded scan (step 7).** The Sheets API has no
server-side filter and no count endpoint, so an exact `total` over the whole
sheet would mean either unbounded memory or hundreds of upstream calls per
call — incompatible with never loading a whole sheet into memory
(docs/06-sheets-adapter.md §6) at the spec's own target scale. This is a
deliberate deviation from docs/06 §4.2's example output (`Decision 4`,
`.claude/plans/2026-08-18-sheets-adapter.md`): instead of an unconditional
`total`, the response is

```jsonc
{ "count": 50, "offset": 0, "has_more": true, "next_offset": 50,
  "scanned_rows": 5000, "scan_complete": false, "rows": [ { "...": "..." } ] }
```

`total` (exact) is added **only** when `scan_complete` is `true` — the scan
reached the sheet's real end within this call's budget. When `scan_complete`
is `false`, there is no `total` field at all, and both the tool description
and the result text tell the model `count`/`scanned_rows` are a lower bound,
not a final answer, and to narrow the filter (or page forward with
`next_offset`) rather than assume nothing else matches.

The scan itself: pages of up to 5,000 rows are fetched via `Values.Get`,
charging the connector's rate-limit bucket (see below) **one unit per page
fetched**, not one per tool call; filters are evaluated in-process, retaining
at most `offset + limit + 1` matched rows (the `+1` is what sets `has_more`
without a second pass) so peak memory is one fetched page plus that small
retained window, independent of the sheet's actual size. The scan stops at
whichever comes first: enough matches, the sheet's real end, a scan budget of
50,000 rows (configurable), or an exhausted rate-limit bucket — the last of
these ends the scan cleanly with `scan_complete: false`, never as an error.
Response body over ~256KB returns `RESULT_TOO_LARGE` instead of a truncated
result, naming `columns` projection or a narrower filter/limit as the fix.

**`sheets_read_range`'s A1 parser (step 8).** `range` is agent-supplied
input, not a trusted identifier — it is parsed into a sheet name and numeric
column/row bounds in our own code, never passed through to Google opaquely.
Anything that doesn't parse into that shape (a formula like
`=IMPORTRANGE(...)`, a second range smuggled in after a comma, mismatched
reference kinds across the `:`) is rejected before any network call, the
same way a filter value is never interpolated into anything Google
evaluates. A column reference past `ZZZ` — Google Sheets' own ceiling of
18,278 columns per sheet — is rejected on the same grounds, since a narrow
span of absurd column letters would otherwise clear both size caps below
and spend a rate-limit unit on a call Google can only answer with a 400.
Reversed bounds (`D10:A1`) are normalized into ascending order
rather than rejected. Beyond the unbounded-range rejection above, a parsed
range spanning more than 1,000 rows or 20,000 cells is rejected
before `Values.Get` (naming "narrow the range" as the fix) — the same
256KB `RESULT_TOO_LARGE` cap `sheets_query_rows` uses is then re-checked
against the actual rows fetched, as a second line of defence. A row-only
range's column span is resolved against the sheet's real width only once
`SpreadsheetMeta` is available, so the cell-count check runs a second time
at that point too. Exactly one rate-limit unit is charged for the single
`Values.Get` call (never for the `SpreadsheetMeta` lookup, which rides the
dispatch-time floor every `tools/call` already pays); unlike
`sheets_query_rows`' scan, there is no partial answer to fall back to, so
an exhausted bucket here is always `RATE_LIMITED`, never a degraded result.

**Google Sheets tool guardrails.** `spreadsheet_id` is checked against the
connector's stored allowlist on **every** call — freshly decrypted and
re-parsed each time, never a value cached from connector-creation or
session-start time, so a narrowed allowlist takes effect on the very next
call. An id absent from the allowlist returns `IsError: true` with a
`SPREADSHEET_NOT_ALLOWED` result (docs/06-sheets-adapter.md §8), naming
`sheets_list_spreadsheets` as the recovery path — not a JSON-RPC protocol
error, so the model can adapt rather than the turn aborting.
`include_sample_rows` outside `0-5` is rejected before any config decryption
or upstream call, and so are `sheets_query_rows`' own input errors: `limit`
outside `1-200`, a negative `offset`, an unsupported filter `op`, or a value
shaped wrong for its operator (an array for anything but `in`, a non-array
for `in`) — all checked before any network call, as is every
`sheets_read_range` range-shape error described above. A `sheet_name`
absent from the spreadsheet's own tabs (or, for `sheets_read_range`, a
sheet name parsed out of `range`) returns `SHEET_NOT_FOUND`; a filter or
projection column absent from the sheet's header row returns
`COLUMN_NOT_FOUND`, both naming `sheets_describe_spreadsheet` as the
recovery path. The connector's OAuth access token is cached in-process per connector id
(`golang.org/x/oauth2.ReuseTokenSource`, deliberately not Redis — a derived
access token is a live credential) and reused across calls against the same
connector.

**Audit.** Best-effort (a failed write never fails the MCP call), same
`GET /audit-logs` trail as everything else:

| Action | Written when | Metadata |
| ------ | ------------ | -------- |
| `mcp.session.started` | The handshake's first request — `initialize` for pre-2026-07-28 (SEP-2575) clients, `server/discover` for clients that negotiate the newer protocol by default (SDK v1.7.0 does, in Stateless mode) | `connector_id` |
| `mcp.tool.called` | A `tools/call` the caller was permitted to make and whose rate-limit check passed, recorded once the call has actually finished (step 7: `duration_ms`/`row_count` don't exist until then) | `connector_id`, `tool`, `duration_ms`; plus `spreadsheet_id`/`sheet_name`/`file_id`/`folder_id` when the tool's own arguments name them (the last two from `drive_list_folder`/`drive_get_file`, step 9), `filter_columns` (column **names** `sheets_query_rows` filtered on — never the filter values) when present, and `row_count` when the tool's own output names a count — `sheets_query_rows`' `count` field or `sheets_read_range`'s `row_count` field, never the range's own cell contents |
| `mcp.tool.denied` | A `tools/call` the caller was **not** permitted to make | `connector_id`, `tool`, `missing_permission` |
| `mcp.ratelimit.hit` | A permitted `tools/call` refused because its connector's rate-limit bucket is exhausted (step 4) | `connector_id`, `tool` |
| `mcp.file.downloaded` | A `GET /mcp/files/:connectorId/:fileId` download that actually streamed (step 9) — this route has no bearer-token principal, so the actor recorded is the `uid` the link itself was minted for, not a re-resolved live grant | `connector_id`, `file_id`, `mime_type` |

Metadata never carries the bearer token, decrypted connector config, a whole
request/tool-argument struct, or a filter's actual value — only the small,
explicit fields above. `mcp.tool.called` moved from before-dispatch to
after-dispatch in step 7 specifically so `duration_ms`/`row_count` could join
the row the spec calls for, rather than either being dropped or landing in a
second row; it still fires exactly once per permitted, budgeted call,
including when the tool's own handler panics (recorded via `defer`, ahead of
the process's top-level recovery).

**Rate limiting (step 4).** Every connector has its own token bucket
(`internal/infra/redis.RateLimiter`, Redis key `mcp:ratelimit:<connectorId>`),
capacity `MCP_RATE_LIMIT_PER_MIN` tokens (default 60), refilling continuously
over 60 seconds. The bucket counts **upstream API requests**, not MCP tool
calls — Google's own quotas (~60 req/min/user, ~300 req/min/project) are
counted in API requests, so a limiter keyed to tool calls would measure the
wrong thing. Step 3's trivial tool (and every tool before a real adapter
lands) makes zero upstream calls, so every `tools/call` charges a floor of 1
unit today; a real adapter charges N units for N upstream requests instead
(the paged sheet scan in step 7 charges 1 unit per page fetched, mid-scan,
rather than paying the whole scan's cost up front). The check runs after the
permission check and before dispatch, so an unpermitted call never spends
budget.

An exhausted bucket is a normal tool result, not a protocol error or an HTTP
failure:
```
CallToolResult{ IsError: true, Content: [{ text:
  "RATE_LIMITED: this connector's request budget is exhausted. Retry after <n> seconds." }] }
```
`<n>` is always a whole number of seconds (rounded up), so the agent can back
off a concrete amount rather than guessing. Each denial writes a best-effort
`mcp.ratelimit.hit` audit row (`connector_id`, `tool`) — see the audit table
above.

**Signed download link (`GET /mcp/files/:connectorId/:fileId`, step 9).**
The **one** gateway route authenticated by URL signature rather than a
bearer header — there is no PAT, no `Authorization` header, nothing else
standing between an internet-reachable URL and a customer's file. Google
Drive has no signed-URL feature of its own, so this gateway mints and
verifies the link itself: `drive_get_file` above signs it, and this route
verifies the signature before doing anything else. Not mounted behind
`RequireMCPKey` — deliberately, since the whole point is that the link can
be handed to something with no PAT at all (a browser, `curl`, a different
process).

Query params, all required: `org` (the org the link is scoped to), `uid`
(the principal whose agent minted the link — carried purely so the download
can be audited to a real actor, not itself part of authorization), `exp`
(Unix seconds the link stops working at), `sig`
(`base64url(HMAC-SHA256(key, "v1\norg\nconnectorId\nfileId\nuid\nexp"))`,
where `key` is derived from `CONNECTOR_MASTER_KEY` via HKDF-SHA256 — never
the master key itself). TTL is **15 minutes**, matching
`docs/06-sheets-adapter.md` §4.3's hard ceiling; `exp` is never honored
past that regardless of what a (still correctly signed) link claims.

Every failure — a missing or malformed query param, a bad signature, an
expired `exp`, an unresolvable connector, a connector of the wrong type, a
file whose parent folder was removed from the allowlist since the link was
minted, or a Google-native file with no bytes to stream — is the exact same
uniform `404 { "message": "Resource not found" }` a client cannot
distinguish, mirroring the JSON-RPC route's own `POST /mcp/:connectorId`
tenant-isolation behavior. Two cases get a different status because they
are not authorization failures: a file whose size exceeds 25MiB is
`413 { "message": "File too large" }`, and an exhausted per-connector
rate-limit bucket (charged 1 unit for the download itself, separate from
the unit `drive_get_file`'s own metadata fetch already charged) is
`429` with a stated retry-after, same wording as the JSON-RPC route's
`RATE_LIMITED` result.

A successful response streams the file's raw bytes with
`Content-Type` (from Drive), `Content-Disposition: attachment;
filename*=UTF-8''<percent-encoded name>`, `Cache-Control: private,
no-store`, and `X-Content-Type-Options: nosniff`, and writes a best-effort
`mcp.file.downloaded` audit row (`connector_id`, `file_id`, `mime_type`;
actor `uid`, org `org` — both from the verified link, since this route has
no re-resolved RBAC principal of its own).

### Admin console (`/admin`)

The platform-staff surface, and the one deliberate exception to tenant
scoping: every route below reaches **across all organizations**, guarded by
`RequirePlatformRole("superadmin", "support")` (reads, impersonation, the
three `/admin/2fa/*` routes) or a separate `RequirePlatformRole("superadmin")`
instance (mutations) rather than `RequireOrg`/`RequirePermission`. Design,
threat model, and the decisions behind it: [`11-admin-panel.md`](11-admin-panel.md).

The whole group additionally sits behind an IP allowlist and, once a staff
member has enrolled, a TOTP step-up gate — both described below the endpoint
table, after the response shapes.

**Role split.** One rule: support reads everything and mutates nothing,
except starting an impersonation, which is itself read-only (`GET /admin/me`
reports which role the caller holds).

| Route group | superadmin | support |
| ----------- | :--------: | :--------: |
| Every `GET /admin/*` read route | yes | yes |
| `POST /admin/users/:userId/impersonate` | yes | yes |
| `POST/PUT/DELETE/PATCH` mutations (plan/limit changes, delete org, platform-role grant, ban, plan CRUD) | yes | no |
| `POST /admin/2fa/{enroll,confirm,verify}` | yes | yes |

A support caller hitting a mutation route gets the same 403
`Insufficient permissions` `RequirePlatformRole` returns for a non-staff
caller — the two are indistinguishable, so a mutation route cannot be probed
to learn who holds which platform role. The only way to grant the **first**
platform role is still the operator CLI
(`make admin-grant EMAIL=… ROLE=superadmin|support|none`) — a chicken-and-egg
`PATCH /admin/users/:userId/platform-role` cannot solve on its own, since
calling it already requires being a superadmin. Once at least one superadmin
exists, that route is the normal path for every subsequent grant or revoke;
the CLI remains available for the bootstrap case and for revoking access
without going through the console.

**Shared list conventions.** Every list route takes `?limit=` (1–100, default
50 — `GET /admin/audit-logs` allows up to 200) and `?offset=` (≥ 0, default
0), and returns `{ items: [...], total }` rather than a bare array. `?search=`
is a case-insensitive substring match; an absent or empty value binds "no
filter" rather than an empty-string `ILIKE`. Out-of-range `limit`/`offset`,
a non-UUID `organizationId`/`userId`, an unrecognized `role`/`status`, or an
unparseable `from`/`to` are all 422 `Validation failed`.

`total` is served from a 30-second Redis cache (`admin:count:<hash>`) keyed by
the exact filter set, so paging through one search does not re-`COUNT(*)` per
page. It is therefore allowed to lag a write by up to 30s. A Redis failure
falls back to counting directly — an admin list may be slow when Redis is
down, never broken.

| Method/Path | Guard | Query | Behavior |
| ----------- | ----- | ----- | -------- |
| `GET /admin/me` | platform:`superadmin,support` | — | The calling staff account and its `platformRole`. This is what the console's layout guard calls to decide whether to render at all. |
| `GET /admin/organizations` | platform:`superadmin,support` | `search`, `limit`, `offset` | Every org, oldest first. `search` matches name **or** slug. Each row carries `memberCount`/`connectorCount`/`mcpKeyCount` and `planName` (null for an org with no subscription). |
| `GET /admin/organizations/:orgId` | platform:`superadmin,support` | — | One org's detail view: members, connectors (metadata only), MCP keys (metadata only), the 20 most recent audit entries, `planName`, and `effectiveLimits` (the custom-over-plan merge, resolved by `subscription.Service` rather than re-derived here). 404 `Resource not found` for an unknown **or malformed** id — a malformed UUID can never match a row, so it resolves to the same 404 rather than a 422. |
| `GET /admin/users` | platform:`superadmin,support` | `search`, `role`, `banned`, `limit`, `offset` | Every user, oldest first. `search` matches email **or** display name. `role` is `superadmin`\|`support`\|`none` (`none` = no platform role); `banned` is a bool on whether `bannedAt` is set. |
| `GET /admin/users/:userId` | platform:`superadmin,support` | — | One user's detail view: memberships with org name/slug/role, `bannedAt`/`banReason`, and `activeSessions`. **Never `password_hash`** — the response is mapped field-by-field from the row, never a struct embed. 404 `User not found` for an unknown or malformed id. |
| `GET /admin/connectors` | platform:`superadmin,support` | `organizationId`, `type`, `status`, `search`, `limit`, `offset` | Every connector, oldest first, with its org's name joined in. `status` is `active`\|`inactive`\|`error`; `search` matches connector name. **Metadata only** — no `encrypted_config`, no decrypted config, and not even a "config present" boolean beyond what `status` already implies. |
| `GET /admin/mcp-keys` | platform:`superadmin,support` | `organizationId`, `userId`, `search`, `limit`, `offset` | Every MCP key, oldest first, with org name and owner email joined in. `search` matches key name **or** owner email. **Never `key_hash`, never a raw token** — a raw token does not exist anywhere after its mint response. |
| `GET /admin/audit-logs` | platform:`superadmin,support` | `organizationId`, `userId`, `action` (repeatable), `from`, `to`, `limit` (1–200), `offset` | Cross-org audit query, newest first, with org name and actor email joined in. `action` is repeatable (`?action=a&action=b`, matching any); a trailing `*` makes one entry a prefix match (`?action=mcp.*`), and everything else is matched literally with `%`/`_`/`\` escaped, so an action string can never be misread as a pattern. `from`/`to` are RFC3339 and are normalized to UTC before binding — `audit_logs.created_at` is a `timestamp` with no time zone, so an unnormalized offset would silently compare against the wrong instant. |
| `GET /admin/system/stats` | platform:`superadmin,support` | — | Platform-wide counts for the console landing page: orgs, users, connectors, MCP keys (total and active), active sessions, audit rows, the `email_outbox` breakdown, 7-day signup deltas, org-count per plan, and Redis's `used_memory_human`. Same 30s count cache as the lists. A rising `emailOutbox.failed` is the earliest signal that Resend or the `EMAIL_FROM` domain is misconfigured. |
| `GET /admin/plans` | platform:`superadmin,support` | — | Every plan with its raw `limits` JSON. `total` is simply `len(items)` — plans is a small seeded table with no filters, so it bypasses the count cache. |

**Mutations.** Superadmin only (`RequirePlatformRole("superadmin")`, a
separate guard instance from the read routes above — support never even
reaches these handlers). `POST/PUT/PATCH` bodies are bound and validated the
normal way; `DELETE /admin/organizations/:orgId` is the one route in this
module that needs `httpx.BindBodyAndValidate`, since Echo's default binder
does not read a `DELETE` body.

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /admin/organizations/:orgId/plan` | platform:`superadmin` | `{ planId }` | Upserts the org's subscription via `subscription.Service.AssignPlan` — this module never reimplements the upsert. No password re-auth: a plan change alone is not destructive. `200 { success: true }`. A well-formed but nonexistent `planId` surfaces as a 500 (FK violation), the same tolerance `subscription.Service.AssignPlan` already accepts elsewhere. |
| `PUT /admin/organizations/:orgId/limits` | platform:`superadmin` | `{ customLimits: {…} \| null }` | Overwrites `org_subscriptions.custom_limits`; `null` clears back to plan-only limits. Every present value must be a whole number (422 `Validation failed`) — no key is required, since this is a partial overlay `subscription.Service.EffectiveLimits` merges custom-over-plan. `404 Organization has no subscription to set limits on` if no subscription row exists yet to attach an override to. No re-auth. |
| `DELETE /admin/organizations/:orgId` | platform:`superadmin` | `{ confirm, password }` | Re-authenticates the caller's password (see below) first, then requires `confirm` to equal the org's own **slug** exactly — typing it out is the deliberate friction on an irreversible delete. Memberships/connectors/mcp_api_keys/org_subscriptions all cascade; `audit_logs.organization_id` carries no FK, so the audit trail survives the org it describes — which is why the audit write happens **before** the `DELETE`, not after. Errors: `403 REAUTH_FAILED`, `400 ORG_CONFIRM_MISMATCH`, `404 Resource not found` (unknown or malformed id), `429 TOO_MANY_ATTEMPTS`. |
| `PATCH /admin/users/:userId/platform-role` | platform:`superadmin` | `{ role: "superadmin"\|"support"\|null, password }` | Re-authenticates, then grants (`role` set) or revokes (`role: null`) `users.platform_role`. Every session for the target is revoked in the same transaction as the write — a demotion also ends the target's tenant sessions immediately. No Redis override key is needed: `platform_role` is re-read from the database on every `/admin` request (see below), so there is no stale JWT claim to compensate for. Errors: `403 REAUTH_FAILED`, `403 CANNOT_TARGET_SELF`, `409 SUPERADMIN_LIMIT` (capped at 10 concurrent superadmins — a scripting-mistake guard, not a real ceiling), `404 USER_NOT_FOUND`, `422 Validation failed`, `429 TOO_MANY_ATTEMPTS`. |
| `PATCH /admin/users/:userId/ban` | platform:`superadmin` | `{ banned, reason?, password }` | Re-authenticates, then sets/clears `users.banned_at`/`ban_reason` and revokes every session for the target, in one transaction. `reason` is optional either direction (conventionally set only on ban). On ban, the Redis `banned:<userId>` fast-path cache is primed best-effort (self-heals on the target's next login attempt if that write fails); on unban the Redis `Unban` call is **not** best-effort — it has no TTL, so a failed clear here has no other self-healing path and the error is surfaced. Deliberately leaves `mcp_api_keys` untouched: the gateway already refuses a banned owner's key at the MCP-key join, and revoking keys outright is irreversible where a ban is not. Errors: `403 REAUTH_FAILED`, `403 CANNOT_TARGET_SELF`, `409 TARGET_IS_PLATFORM_STAFF` (checked both directions — a still-privileged account must be demoted first), `404 USER_NOT_FOUND`, `422 Validation failed`, `429 TOO_MANY_ATTEMPTS`. |
| `POST /admin/plans` | platform:`superadmin` | `{ name, limits }` | Creates a plan. `limits` must define `max_members`/`max_roles`/`max_connectors` (the keys `subscription.Service.EnforceLimit` and `cmd/seed` actually depend on; `-1` means unlimited) and every value, required or not, must be a whole number — `422 Validation failed` otherwise. No re-auth: plan CRUD shapes pricing tiers, not tenant or staff access. |
| `PUT /admin/plans/:planId` | platform:`superadmin` | `{ name, limits }` | Full replace of name+limits together, not a partial patch. Same `limits` validation as create. `404 Resource not found` for an unknown or malformed id. |
| `DELETE /admin/plans/:planId` | platform:`superadmin` | — | `409 PLAN_IN_USE` if any `org_subscriptions` row still references the plan (checked explicitly so this is a real 409 rather than a 500 from the underlying `ON DELETE NO ACTION` constraint). `404 Resource not found` for an unknown or malformed id. |

**Impersonation.** On the *read* guard, not write — it grants no more than
support already has (read access), and the token it mints is itself
read-only.

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /admin/users/:userId/impersonate` | platform:`superadmin,support` | `{ reason }` (required, 10-500 chars) | Mints a 10-minute, non-refreshable, read-only access token authenticating **as** the target user — no `refreshToken` in the response; re-issuing writes a fresh audit entry every time. No password re-auth: the operation is read-only, and prompting for one here would train staff to enter it reflexively, cheapening the prompt on routes that actually need it. Refusals, in order: `404 User not found`; `403 CANNOT_IMPERSONATE_STAFF` if the target holds any `platform_role` (checked before the ban state so a banned staff account reports the staff refusal, not its ban status); `403 ACCOUNT_SUSPENDED` if the target is banned. `422 Validation failed` for a `reason` under 10 characters. Every start is audited with the reason, actor, target, ip and userAgent — the containment story here is detection, not prevention. The minted token carries an `imp: true` claim: `Guards.verify()` rejects any non-`GET`/`HEAD`/`OPTIONS` request on it with `403 IMPERSONATION_READ_ONLY`, and `RequirePlatformRole` separately refuses **any** token carrying `imp` before it even reads the caller's user row — an impersonation token can never reach `/admin` itself, regardless of what the target's `platform_role` becomes mid-flight. |

**2FA step-up.** All three routes sit on `RequirePlatformRoleNo2FA` — same
role check as every other `/admin` route, but exempt from the
`ADMIN_REQUIRE_2FA` gate described below (the chicken-and-egg: a staff member
cannot complete step-up through a route step-up itself gates).

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /admin/2fa/enroll` | platform:`superadmin,support`, 2FA-gate exempt | — | Generates a fresh TOTP secret, seals it under `CONNECTOR_MASTER_KEY` (the same envelope machinery and `KeyProvider` connectors use), and returns its `otpauth://` URI **once** — never persisted in cleartext, including logs. Re-callable: enrolling again wipes any prior confirmation and recovery codes along with the superseded secret. |
| `POST /admin/2fa/confirm` | platform:`superadmin,support`, 2FA-gate exempt | `{ code }` (6-digit) | Verifies `code` against the most recently enrolled secret; on success stamps `confirmed_at` and returns **ten** recovery codes once (only their SHA-256 hashes are persisted — losing this response means losing the codes). `400 TOTP_NOT_ENROLLED` if no secret was ever enrolled. `401 INVALID_TOTP_CODE` on a wrong code. |
| `POST /admin/2fa/verify` | platform:`superadmin,support`, 2FA-gate exempt | `{ code }` (TOTP code or an unused recovery code) | The step-up check itself: on success sets the `admin:2fa:<userId>` Redis key (12h TTL) every other `/admin` route depends on when `ADMIN_REQUIRE_2FA=true`. Tries `code` as a live TOTP code first, then as an unused recovery code — a matched recovery code is deleted from the stored set immediately, so it verifies exactly once. Rate-limited independently of every other admin limiter (`admin:2fa:attempts:<userId>`, 5 attempts / 15 min) — `429 TOO_MANY_ATTEMPTS`. `400 TOTP_NOT_ENROLLED` if confirm never landed (a secret exists but is unconfirmed is treated identically, so as not to leak the in-progress state). `401 INVALID_TOTP_CODE` if `code` matches neither a live TOTP window nor any unused recovery code — deliberately one code for both failure modes, same reasoning as `INVALID_CREDENTIALS`. |

Response shapes:

```
GET /admin/me                     -> { id, email, displayName, platformRole }

GET /admin/organizations          -> { items: [{ id, name, slug, createdAt, memberCount,
                                                 connectorCount, mcpKeyCount, planName }], total }

GET /admin/organizations/:orgId   -> { id, name, slug, createdAt, updatedAt, planName,
                                       effectiveLimits: { <limit>: number },
                                       members:  [{ userId, email, displayName, role, joinedAt }],
                                       connectors: [ <ConnectorItem> ],
                                       mcpKeys:    [ <MCPKeyItem> ],
                                       recentAuditLogs: [{ id, userId, action, metadata, createdAt }] }

GET /admin/users                  -> { items: [{ id, email, displayName, isVerified, platformRole,
                                                 bannedAt, createdAt, orgCount }], total }

GET /admin/users/:userId          -> { id, email, displayName, isVerified, platformRole, bannedAt,
                                       banReason, createdAt, activeSessions,
                                       memberships: [{ organizationId, organizationName,
                                                       organizationSlug, role, joinedAt }] }

GET /admin/connectors             -> { items: [ ConnectorItem ], total }
    ConnectorItem                 =  { id, organizationId, organizationName, name, type, status,
                                       lastHealthCheckAt, createdAt }

GET /admin/mcp-keys               -> { items: [ MCPKeyItem ], total }
    MCPKeyItem                    =  { id, organizationId, organizationName, userId, userEmail,
                                       name, scopes, lastUsedAt, expiresAt, revokedAt, createdAt }

GET /admin/audit-logs             -> { items: [{ id, organizationId, organizationName, userId,
                                                 userEmail, action, metadata, createdAt }], total }

GET /admin/system/stats           -> { organizations, users, connectors, mcpKeysTotal, mcpKeysActive,
                                       sessionsActive, auditLogs,
                                       emailOutbox: { pending, sent, failed },
                                       usersLast7d, organizationsLast7d,
                                       planBreakdown: [{ planName, orgCount }],
                                       redisUsedMemoryHuman }

GET /admin/plans                  -> { items: [{ id, name, limits, createdAt }], total }

POST /admin/organizations/:orgId/plan   -> { success: true }
PUT  /admin/organizations/:orgId/limits -> { success: true }
DELETE /admin/organizations/:orgId      -> { success: true }
PATCH /admin/users/:userId/platform-role -> { success: true }
PATCH /admin/users/:userId/ban          -> { success: true }
POST /admin/plans                       -> { id, name, limits, createdAt }
PUT  /admin/plans/:planId               -> { id, name, limits, createdAt }
DELETE /admin/plans/:planId             -> { success: true }

POST /admin/users/:userId/impersonate   -> { accessToken, expiresIn,
                                              user: { id, email, displayName } }

POST /admin/2fa/enroll                  -> { otpauthUri }
POST /admin/2fa/confirm                 -> { recoveryCodes: [ <10 strings> ] }
POST /admin/2fa/verify                  -> { success: true }
```

**What these routes must never return**, restated here because it is the rule
most likely to be relaxed by a well-meaning "the console needs to debug this"
change (`docs/11-admin-panel.md` §7): a connector's `config`, sealed or
opened; an MCP key's `key_hash` or raw token; a user's `password_hash`. A
staff console that can read a tenant's upstream credentials is a credential
store with a login page, and the gateway's whole security argument depends on
it not being one. Connector health is diagnosable from `status` and
`lastHealthCheckAt` plus the tenant's own `mcp.*` audit trail, which is why
those are joined into these views instead.

This rule is enforced by test, not by review habit:
`internal/server/admin_integration_test.go` seeds a connector whose sealed
config contains a known secret, then walks every admin response — including
nested objects — asserting both that no `encrypted_config`/`password_hash`/
`key_hash` key appears under any spelling and that the secret's raw bytes
appear nowhere in the body. A leak under a renamed key still fails.

Admin **reads** are **not** themselves audit-logged today — that remains a
known gap, not a decision (the audit trail records what tenants do, not what
staff looked at). Every admin **mutation**, and every impersonation start, is
audit-logged (`admin.*`/`admin.impersonation.started` actions), so "who
looked at this" is answered for impersonation even while it stays unanswered
for an ordinary `GET /admin/users/:userId`.

**Password re-auth.** `DELETE /admin/organizations/:orgId`,
`PATCH /admin/users/:userId/platform-role`, and
`PATCH /admin/users/:userId/ban` re-verify the caller's own password
(`{ password }` in the body) before anything destructive happens — a session
left open on an unlocked laptop, or a stolen access token, is not by itself
sufficient to delete an organization, grant/revoke platform access, or ban a
user. Rate-limited independently of login (`admin:reauth:attempts:<userId>`,
5 attempts / 15 min, same shape as the login limiter) so the password field
cannot become an online password oracle against the highest-value accounts in
the system: `403 REAUTH_FAILED` on a wrong password, `429 TOO_MANY_ATTEMPTS`
after 5. `POST /admin/organizations/:orgId/plan`,
`PUT /admin/organizations/:orgId/limits`, and plan CRUD do **not** re-auth —
none of them is irreversible or changes who can access what the way
delete-org/role-change/ban do.

**IP allowlist.** `ADMIN_IP_ALLOWLIST` (comma-separated CIDRs) gates the
entire `/admin` route group **before** `RequireAuth` runs at all — an
off-network caller never reaches the login/2FA surface, let alone a read or
mutation. Rejection is a plain `404 Route not found`, never `403`: a `403`
would confirm to an off-network scanner that something exists here to be
denied from. Empty/unset (the required default for local dev, and for any
deployment that hasn't set the variable) disables the check entirely and lets
every request through unmodified. Parsed once at boot, not per request — a
malformed CIDR fails startup rather than silently letting every request
through (or none).

**2FA step-up.** When `ADMIN_REQUIRE_2FA=true` (the default), every `/admin`
route except the three `POST /admin/2fa/*` routes above requires a live
`admin:2fa:<userId>` Redis key (12h TTL) from a completed
`POST /admin/2fa/verify` — `403 TWO_FACTOR_REQUIRED` otherwise. The role
check runs, and can reject, before this one: a demoted staff member's
`platform_role` is gone immediately, so a stale-but-unexpired `admin:2fa:`
key from before the demotion is never even reached. `platform_role` itself
stays a fresh per-request database read rather than a JWT claim or a cached
value, for the same reason `isVerified` is (see `GET /auth/me` above) — no
Redis override key compensates for it.

### API docs

Source serves Swagger UI at `/swagger` with bearerAuth security scheme. Go port should serve equivalent OpenAPI docs (echo-swagger / swag, or generated OpenAPI 3 spec).

## Environment variables (contract)

| Variable | Default | Notes |
| -------- | ------- | ----- |
| `PORT` | 3000 | |
| `APP_NAME` | sapanjai-api | logger service name |
| `DATABASE_URL` | — | `postgres://user:pass@host:5432/sapanjai` |
| `REDIS_URL` | — | required at boot |
| `REDIS_KEY_PREFIX` | `sapanjai:` | prepended to every Redis key (blacklist, login attempts, verification/reset tokens, MCP rate-limit buckets, worker job locks) so a shared Redis instance cannot collide with another app's keys. Must be identical on api and worker. Explicit empty string opts out. |
| `JWT_ACCESS_SECRET` / `JWT_REFRESH_SECRET` | — | min 32 chars |
| `CONNECTOR_MASTER_KEY` | — | base64, exactly 32 bytes (`openssl rand -base64 32`); master key wrapping every connector's envelope-encryption data key |
| `JWT_ACCESS_EXPIRES_IN` | 15m | duration string |
| `JWT_REFRESH_EXPIRES_IN` | 604800 | **seconds** (integer) |
| `MCP_RATE_LIMIT_PER_MIN` | 60 | per-connector upstream-API token-bucket capacity, tokens/minute (`mcp:ratelimit:<connectorId>`) — see the MCP gateway section above |
| `APP_PUBLIC_URL` | http://localhost:4000 | browser-facing **frontend** origin that verification/reset links are built against; trailing slash trimmed |
| `LOG_LEVEL` | info | fatal/error/warn/info/debug/trace |
| `NODE_ENV` → rename `APP_ENV` | development | dev enables pretty logging |
| `ADMIN_IP_ALLOWLIST` | — (unset) | comma-separated CIDRs gating the `/admin` group before `RequireAuth`; empty/unset disables the check. Parsed once at boot — a malformed entry fails startup. A wrong value locks every platform staff account out of `/admin` with no in-app recovery path. |
| `ADMIN_REQUIRE_2FA` | true | gates every `/admin` route except `POST /admin/2fa/{enroll,confirm,verify}` behind a completed TOTP step-up (`admin:2fa:<userId>`, 12h TTL). Set false only for local development. |

This table covers the API process. Worker-only variables — `WORKER_PORT`,
`WORKER_JOB_TIMEOUT`, `SESSION_CLEANUP_*`, and the transactional-mail set
(`RESEND_API_KEY`, `EMAIL_FROM`, `EMAIL_DISPATCH_INTERVAL`,
`EMAIL_DISPATCH_BATCH_SIZE`, `EMAIL_MAX_ATTEMPTS`, `EMAIL_OUTBOX_RETENTION`) —
are not part of the API contract and live in CLAUDE.md § Environment and
[`10-transactional-email.md`](10-transactional-email.md). `APP_PUBLIC_URL` is
listed above because the **API** reads it, to build the links it renders into
queued mail.

Redis key conventions: `blacklist:<accessToken>` (EX = 900), `login:attempts:<email>` (EX = 900, INCR on failure), `verify:email:<sha256hex(token)>` (EX = 86400, value = userId, `GETDEL`-consumed), `verify:resend:<userId>` (`SET EX 300 NX`), `reset:password:<sha256hex(token)>` (EX = 3600, value = userId, `GETDEL`-consumed), `reset:request:<email>` (`SET EX 900 NX`). Tokens are hashed before they become a key, so a `KEYS`/`MONITOR`/RDB dump yields nothing redeemable.
