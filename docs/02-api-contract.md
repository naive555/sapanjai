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

### Error responses

Guard failures:

| Condition                          | Status | Message                          |
| ---------------------------------- | ------ | -------------------------------- |
| Missing/invalid/expired token      | 401    | `Unauthorized`                   |
| Blacklisted token                  | 401    | `Token revoked`                  |
| Missing `x-organization-id`        | 400    | `Missing x-organization-id header` |
| Not a member of the org            | 403    | `Not a member of this organization` |
| Missing RBAC permission            | 403    | `Missing permission: <action>`   |

Service error map (service throws code → HTTP response):

| Code                    | Status | Message                                          |
| ----------------------- | ------ | ------------------------------------------------ |
| `EMAIL_TAKEN`           | 409    | Email already taken                              |
| `INVALID_CREDENTIALS`   | 401    | Invalid email or password                        |
| `TOO_MANY_ATTEMPTS`     | 429    | Too many login attempts, try again in 15 minutes |
| `INVALID_REFRESH_TOKEN` | 401    | Invalid refresh token                            |
| `REFRESH_TOKEN_REUSE`   | 401    | Refresh token reuse detected                     |
| `REFRESH_TOKEN_EXPIRED` | 401    | Refresh token expired                            |
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
| (unknown)               | 500    | Internal server error                            |

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

JWT claims — access: `{ sub: userId, email }`, HS256, exp = `JWT_ACCESS_EXPIRES_IN` (default 15m). Refresh: `{ sub: userId }`, separate secret, no embedded exp in source (session row `expires_at` = now + `JWT_REFRESH_EXPIRES_IN` seconds, default 604800).

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
| `GET /audit-logs` | org | `userId?`, `action?`, `limit?` (1–100, default 50) | Org's logs, newest first. |

Recorded actions: `user.login`, `user.register`, `org.created`, `org.member.invited`, `org.member.removed`, `role.created`, `role.assigned` (last three defined but only the first four are currently written), plus `connector.created`, `connector.updated`, `connector.deleted` (all three written, from the Connectors module below).

### Subscription (`/subscription`)

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `GET /subscription` | org | — | Org's subscription incl. plan (nullable if none). |
| `POST /subscription/assign` | org | `{ planId }` | Upsert org subscription. ⚠️ Source has no admin check — see quirks in 01-source-analysis.md. |
| `GET /plans` | auth | — | All available plans: `[{ id, name, limits, createdAt }]`. **Not in the source app** — added in Phase 6 so the frontend subscription page can populate a plan picker (plan ids are server-generated UUIDs with no fixed/knowable value otherwise). Global, not org-scoped, so `auth` not `org` guard. |

### Connectors (`/connectors`)

Org-scoped upstream connections (DB creds, API keys, ...) for the Managed MCP
Gateway product (`docs/05-mcp-gateway.md`). This is the **generic skeleton
only** — no per-type adapter logic yet; every connector uses `type: "generic"`
until a real integration (FlowAccount, PEAK, ...) lands.

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /connectors` | perm:`connector:write` | `{ name: 1-100, type: "generic", config: object }` | Seals `config` at rest with envelope encryption (fresh AES-256 data key per row, wrapped by `CONNECTOR_MASTER_KEY`) and stores it; new rows start `status: "inactive"`. Enforces the `max_connectors` plan limit. 409 `CONNECTOR_NAME_TAKEN` (unique per org); 422 `Validation failed` for an unrecognized `type` (the request validator rejects it before the service's own `INVALID_CONNECTOR_TYPE` check is ever reached). |
| `GET /connectors` | perm:`connector:read` | — | Org's connectors, oldest first. |
| `GET /connectors/:connectorId` | perm:`connector:read` | — | One connector. 404 `NOT_FOUND` for another org's id — indistinguishable from a nonexistent one. |
| `PATCH /connectors/:connectorId` | perm:`connector:write` | `{ name?, status?, config? }` | Partial update; unset fields are left unchanged. A supplied `config` is re-sealed under a brand-new data key (the old ciphertext is overwritten, not versioned). `type` is immutable — there is no `type` field to patch. |
| `DELETE /connectors/:connectorId` | perm:`connector:delete` | — | `{ success: true }`. 404 `NOT_FOUND` if already gone or not this org's. |
| `POST /connectors/:connectorId/health-check` | perm:`connector:write` | — | Probes the upstream and records `status`/`lastHealthCheckAt`. **No per-type checker is registered yet**, so this always returns 501 `HEALTH_CHECK_UNSUPPORTED` and leaves the row untouched. Gated by `connector:write` (not `:read`) because it writes to the row. |

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
as a bearer credential (`docs/07-sheets-adapter-plan.md` Decision 1).
`scopes` is enforced by the MCP gateway below: it intersects a key's
(nullable) `scopes` with the creator's live RBAC grant, re-resolved on every
request — a key can only ever narrow that grant, never widen it.

| Method/Path | Guard | Body | Behavior |
| ----------- | ----- | ---- | -------- |
| `POST /mcp-keys` | perm:`mcpkey:write` | `{ name: 1-100, expiresInDays?: int ≥ 1 }` | Mints a token of the form `sk_live_<base64url(32 random bytes)>` and stores only its SHA-256 hash (`key_hash`, unique-indexed) — never bcrypt, and deliberately so: the token is 256 bits of CSPRNG output, not a human-chosen password, so brute force is moot and a lookup-by-hash needs a deterministic digest. `expiresInDays`, if given, sets an absolute `expires_at`; omitted means the key never expires. 409 `MCP_KEY_NAME_TAKEN` (unique per org). **The raw token is returned in this response's `apiKey` field and nowhere else** — it is never stored, never logged, and cannot be recovered later. |
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
`docs/05-mcp-gateway.md`. Implemented in `docs/07-sheets-adapter-plan.md`
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

**Tool catalog (step 3):**

| Tool | Permission | Description | Returns |
| ---- | ---------- | ------------ | ------- |
| `sapanjai_describe_connector` | `connector:read` | Describes the connector this session is bound to. Takes no arguments — the connector is fixed by the URL, not model-supplied. | `{ name, type, status }` — structurally incapable of returning `config`; the decrypted connector config never leaves `connector.Service`, same invariant as the REST `/connectors` routes. |

**Audit.** Best-effort (a failed write never fails the MCP call), same
`GET /audit-logs` trail as everything else:

| Action | Written when | Metadata |
| ------ | ------------ | -------- |
| `mcp.session.started` | The handshake's first request — `initialize` for pre-2026-07-28 (SEP-2575) clients, `server/discover` for clients that negotiate the newer protocol by default (SDK v1.7.0 does, in Stateless mode) | `connector_id` |
| `mcp.tool.called` | A `tools/call` the caller was permitted to make, recorded before dispatch | `connector_id`, `tool` |
| `mcp.tool.denied` | A `tools/call` the caller was **not** permitted to make | `connector_id`, `tool`, `missing_permission` |

Metadata never carries the bearer token, decrypted connector config, or a
whole request/tool-argument struct — only the small, explicit fields above.

### API docs

Source serves Swagger UI at `/swagger` with bearerAuth security scheme. Go port should serve equivalent OpenAPI docs (echo-swagger / swag, or generated OpenAPI 3 spec).

## Environment variables (contract)

| Variable | Default | Notes |
| -------- | ------- | ----- |
| `PORT` | 3000 | |
| `APP_NAME` | sapanjai-api | logger service name |
| `DATABASE_URL` | — | `postgres://user:pass@host:5432/sapanjai` |
| `REDIS_URL` | — | required at boot |
| `JWT_ACCESS_SECRET` / `JWT_REFRESH_SECRET` | — | min 32 chars |
| `CONNECTOR_MASTER_KEY` | — | base64, exactly 32 bytes (`openssl rand -base64 32`); master key wrapping every connector's envelope-encryption data key |
| `JWT_ACCESS_EXPIRES_IN` | 15m | duration string |
| `JWT_REFRESH_EXPIRES_IN` | 604800 | **seconds** (integer) |
| `LOG_LEVEL` | info | fatal/error/warn/info/debug/trace |
| `NODE_ENV` → rename `APP_ENV` | development | dev enables pretty logging |

Redis key conventions: `blacklist:<accessToken>` (EX = 900), `login:attempts:<email>` (EX = 900, INCR on failure).
