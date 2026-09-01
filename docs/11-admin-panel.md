# 11 — Admin panel: the platform staff console

An internal console at `/admin` for platform staff, sitting **outside** the
tenant boundary. This document records the design — the shape of the
authorization boundary, what staff may do, the impersonation threat model, and
what was deliberately left out. The execution script that builds it is
[`.claude/plans/2026-08-28-admin-panel.md`](../.claude/plans/2026-08-28-admin-panel.md),
a throwaway; this file is the maintained one.

Route shapes, status codes, and error messages live in
[`02-api-contract.md`](02-api-contract.md), which stays the source of truth for
all of that.

> **Status:** design agreed, build in progress. Sections describing behaviour
> are written in the present tense because they describe the target; check the
> contract for what is actually wired today.

---

## 1. What this is

Every existing route is org-scoped by `RequireOrg` / `RequirePermission`. Admin
routes are the deliberate exception: they read and write **across all
organizations**, guarded by a new `RequirePlatformRole` and by a
`users.platform_role` column that no tenant-facing code path can set. There is
no membership involved — platform staff typically hold no membership anywhere,
so `RequireOrg`'s `x-organization-id` contract does not apply to `/admin`.

The product being supported is an MCP gateway. A support ticket here is almost
always *"my MCP client returns 401"* or *"the sheets connector says
unhealthy"*. A console that can only see orgs and users cannot answer either
question, which is why cross-org **read** views of connectors, MCP keys and
gateway audit traffic are in v1 rather than a later phase.

Staff are 2–5 people. Every design call below is made for that scale: an extra
indexed lookup per admin request is free, and a second source of truth for
anything is not worth owning.

## 2. Locked decisions

Owner-approved 2026-08-28. Do not re-litigate without the owner.

| # | Decision | Rationale |
|---|---|---|
| D1 | **`platform_role` is never a JWT claim.** `RequirePlatformRole` reads `users.platform_role` fresh from the database on every admin request. | A claim keeps a demoted account privileged for a full access-token lifetime, and the fix for that is a Redis override key — a second source of truth. This repo already made the opposite call for `is_verified` (*"deliberately not a JWT claim, which would go stale"*). One indexed primary-key lookup per admin request buys instant revocation. |
| D2 | **Two platform roles: `superadmin`, `support`.** | A third value is one `CHECK` constraint change later, not a redesign. |
| D3 | **Bans are durable.** `users.banned_at` / `users.ban_reason` are the source of truth; Redis `banned:<userId>` is a fast-path cache. | A Redis-only ban means a Redis flush silently unbans everyone. |
| D4 | **Password re-auth on destructive ops** — delete org, grant/revoke `platform_role`, ban. Per-operation, not a sudo window. | The friction *is* the feature on delete-org; a 5-minute sudo window removes exactly the friction that matters. |
| D5 | **Impersonation ships**, read-only and non-refreshable. See §5. | Reproducing a tenant's `tools/list` output is otherwise impossible, and the risk is containable because the token is method-restricted at the guard rather than per route. |
| D6 | **IP allowlist + TOTP step-up ship**, in a final phase that can be cut without touching the ones before it. | Both are `/admin`-only concerns and touch no tenant route. |
| D7 | **Route prefix is `/admin`.** | Nothing collides. |

## 3. Seven findings, designed out

The reference implementation this design draws on (the superadmin module of a
sibling project, `../agritech`, outside this repo) shipped, then needed a
follow-up plan that listed seven security findings against its own first
build. Six are designed out here from
the start, and the seventh is the reason §4 exists:

| # | Finding in the reference build | How this design avoids it |
|---|---|---|
| 1 | Stale platform role carried in the JWT | D1 — no claim at all |
| 2 | Redis-only bans, erased by a flush | D3 — the DB column is truth |
| 3 | No self-ban guard | `CANNOT_TARGET_SELF`, checked in the service |
| 4 | Privileged accounts bannable/deletable directly | `TARGET_IS_PLATFORM_STAFF` — demote first |
| 5 | Role change did not verify the target exists | load-then-update, not a blind `UPDATE` |
| 6 | No re-authentication for destructive ops | D4 |
| 7 | No IP / user-agent in admin audit metadata | captured in the handler as an `adminAuditContext` and attached to every admin audit write |

Three things the reference got right and this design copies unchanged: a
short-TTL cached `COUNT(*)` behind paged list endpoints, English-only admin UI
(no i18n keys for staff-only screens), and a visually distinct admin layout so
nobody confuses the console with the tenant app.

Two things it did that this design does **not** copy:

- **Seeding a superadmin with a fixed password.** A seed that mints a
  privileged account with a known credential is a production incident waiting
  for someone to run `make seed` against the wrong `DATABASE_URL`. Replaced by
  a promote-only bootstrap (`cmd/grantadmin`, `make admin-grant`) that looks up
  an existing account by email and can never create one.
- **Hard user deletion with a `deleted_users` archive.** Deferred — see §6.

## 4. Role / permission matrix

The split is one rule: **support reads everything and mutates nothing**, except
starting an impersonation, which is itself read-only. It is easy to hold in
your head, and it means a compromised support account cannot destroy anything.

| Route | superadmin | support |
|---|---|---|
| `GET /admin/me` | ✓ | ✓ |
| `GET /admin/organizations` · `/:orgId` | ✓ | ✓ |
| `GET /admin/users` · `/:userId` | ✓ | ✓ |
| `GET /admin/connectors` | ✓ | ✓ |
| `GET /admin/mcp-keys` | ✓ | ✓ |
| `GET /admin/audit-logs` | ✓ | ✓ |
| `GET /admin/system/stats` | ✓ | ✓ |
| `GET /admin/plans` | ✓ | ✓ |
| `POST /admin/organizations/:orgId/plan` | ✓ | — |
| `PUT /admin/organizations/:orgId/limits` | ✓ | — |
| `DELETE /admin/organizations/:orgId` | ✓ | — |
| `PATCH /admin/users/:userId/platform-role` | ✓ | — |
| `PATCH /admin/users/:userId/ban` | ✓ | — |
| `POST /admin/plans` · `PUT` · `DELETE` | ✓ | — |
| `POST /admin/users/:userId/impersonate` | ✓ | ✓ |

### Bans reach further than login

A ban is enforced in three places, because the JWT is not the only credential
this platform issues:

- `Guards.verify()` rejects an already-issued access token with `401 Account
  suspended` — 401 rather than 403 because the credential is no longer usable
  and the frontend's 401 path already clears the session cleanly.
- `POST /auth/login` refuses a banned credential with `403 ACCOUNT_SUSPENDED`,
  and re-primes the Redis cache on the way past, which is what makes a Redis
  flush self-heal.
- `RequireMCPKey` rejects a banned key **owner** with the same indistinguishable
  401 the gateway already returns for revoked, expired and unknown keys. This
  is the important one: an MCP PAT has no expiry of its own, so a banned user's
  key would otherwise work forever — a strictly worse hole than the JWT one.

The 401-from-the-guard / 403-from-login asymmetry is intentional and is written
into the contract. Do not unify them.

Because a ban also revokes every session, the refresh path is dead immediately;
the only window left after a Redis flush is a banned user's already-issued
access token living out its ≤15-minute expiry. That bound is why no reaper job
exists.

## 5. Impersonation threat model

A staff member obtains a short-lived token that authenticates **as** a tenant
user, so they see exactly what that user sees — which orgs, which connectors,
which `tools/list` output.

What makes it containable:

1. **Read-only, enforced at the guard, not per route.** A token carrying
   `imp: true` is rejected on any request whose method is not `GET` / `HEAD` /
   `OPTIONS`, inside `Guards.verify()`. A route added next year is covered
   automatically; a per-route allowlist would not be.

   The exception worth knowing: routes that never run `verify()` at all are
   not covered by it, and `POST /auth/logout` is the only unauthenticated
   route with a write effect. It identifies the session by the refresh token
   in its body (it must work for a caller whose access token has already
   expired), so no guard runs on it. That is safe rather than merely
   tolerated: logout's destructive half needs the *target's refresh token*,
   which impersonation never issues, so the call is a no-op success. It is
   pinned by a test — see `TestIntegration_Admin_Impersonation`'s "logout is
   unguarded but harmless" case. Any future unauthenticated write route needs
   the same reasoning done explicitly; it will not inherit the guard's rule.
2. **Short-lived and non-refreshable.** 10-minute TTL, no refresh token, no
   `sessions` row. It cannot be extended, only re-issued — and a re-issue
   writes a fresh audit entry.
3. **Staff cannot be impersonated.** A target holding any `platform_role` is
   refused (`CANNOT_IMPERSONATE_STAFF`), so impersonation cannot become a
   privilege-escalation ladder. That check runs at issuance, and `platform_role`
   is a mutable row — promoting the impersonated user during the token's ten
   minutes would defeat it on its own. So `RequirePlatformRole` *separately*
   refuses any token carrying `imp`, before it reads the user row at all: the
   console is unreachable under impersonation regardless of what the target's
   role becomes mid-flight. A banned target is refused too, checked after the
   staff test so a banned staff account reports the staff refusal rather than
   leaking its ban state.
4. **A reason is mandatory** (minimum 10 characters) and lands in the audit
   metadata alongside the actor, target, IP and user agent.
5. **The MCP gateway is untouched.** PATs are authenticated by
   `RequireMCPKey`, which never sees a JWT; there is no way to impersonate into
   the gateway.

`imp` and `act` (RFC 8693's actor claim) *are* JWT claims, which looks like it
contradicts D1. The distinction: they describe **this token** — immutable for
its lifetime, gone in 10 minutes. There is no stale state for them to hold.
`platform_role` describes a mutable database row, which is exactly why it is
not a claim.

Residual risk, stated plainly: a staff member can read any tenant's data,
including connector *metadata* and audit history. They cannot read connector
credentials (§7 holds — nothing decrypts config for a response) and cannot
change anything. The control here is detection, not prevention: every start is
audited with a reason.

## 6. Explicitly deferred

- **Hard user deletion + a `deleted_users` archive.** The sole-owner case —
  deleting the only owner of an org orphans that org, its connectors and its
  MCP keys — is a design question, not a task. Ban covers the operational need
  (a banned user cannot log in, cannot refresh, and cannot use their PATs) and
  is reversible. Deletion gets its own plan when a deletion request actually
  arrives.
- **Per-tenant usage metering / MCP call-volume charts.** Needs a time-series
  store or a rollup table; the audit log is neither. The stats page shows
  counts, not trends beyond 7d.
- **Admin-initiated password reset for a tenant user.** `POST
  /auth/forgot-password` already exists and is enumeration-safe; a
  staff-triggered variant is a second token-minting path to secure for marginal
  gain.
- **Admin-side connector config editing or credential rotation.** Would require
  decrypting config into an admin response, which §7 forbids outright.
- **Notifications / alerting** on `failed` outbox rows or `mcp.ratelimit.hit`
  spikes. The console surfaces both; paging on them is an observability plan,
  not an admin-panel one.
- **i18n for admin screens.** Staff-only UI stays English; the tenant app has
  no i18n either.

## 7. Hard non-goals

These are not "later" — they are lines the design does not cross.

- **No admin endpoint ever returns decrypted connector `config`.** CLAUDE.md:
  *"Decrypted config must never leave the owning service — no response DTO
  field, no log line, no audit metadata."* Admin connector views are metadata
  only: `id`, `organization_id`, `name`, `type`, `status`,
  `last_health_check_at`, `created_at`. If a support engineer needs to know
  whether a credential is valid, the answer is the health-check result, not the
  credential.
- **No admin endpoint returns `mcp_api_keys.key_hash`**, or anything from which
  a PAT could be reconstructed.
- **No CORS middleware.** Admin pages are same-origin `/api/admin/*` through
  the existing runtime proxy, exactly like every other page.
- **No new secret** for impersonation or file links. Existing key material
  only.
