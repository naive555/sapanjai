# 10 — Transactional email: verification and password reset

How Sapanjai sends the two emails it sends, and why the delivery path looks the
way it does. This is the file to update when the design changes — the plan that
produced it ([`.claude/plans/2026-08-27-email-verification-password-reset.md`](../.claude/plans/2026-08-27-email-verification-password-reset.md))
is a record of a decision, not a living document.

Route shapes, status codes, and error messages live in
[`02-api-contract.md`](02-api-contract.md), which stays the source of truth for
all of that. This document covers the mechanism behind those routes.

## The shape

```
POST /auth/register  ─┐
                      ├─ ONE transaction: INSERT users + INSERT email_outbox(pending)
                      ┘
                            │
   worker, every 15s ───────┴──▶ claim a batch (lease) ──▶ Resend
                                                            ├─ ok    → status=sent, bodies nulled
                                                            └─ error → next_attempt_at += backoff
                                                                       attempts ≥ 5 → status=failed
```

Three packages, each with one job:

| Package | Owns |
| ------- | ---- |
| `internal/shared/email` | Rendering a `Message` from embedded templates, and delivering it (`Sender`). Knows nothing about *why*. |
| `internal/module/auth` | Token lifecycle, the routes, and enqueueing. Never sends. |
| `internal/job/emaildispatch` | Draining `email_outbox`. The only thing that calls a `Sender`. |

## Why an outbox, not a goroutine

Sapanjai has no job queue. It does have `internal/worker` — a scheduler with a
Redis lock, per-run timeouts, panic recovery, and a health endpoint — plus one
job already proving the shape. An outbox table turns that scheduler into the
queue for the price of one migration (`00010_email_outbox.sql`).

That buys three properties a `go func() { send() }` does not:

- **Atomic with the write that caused it.** A user row exists ⟺ its verification
  email is queued, because both inserts are in the same transaction. There is no
  "registered, but the email vanished when the pod restarted."
- **Retries, with an audit trail.** Attempt count and last error sit in Postgres,
  so "did the mail actually go out?" is a query, not a guess.
- **The Resend key lives only on the worker.** `cmd/worker` is the only binary
  that constructs a `Sender`; the API renders and enqueues and never opens a
  connection to Resend. One fewer secret on the internet-facing service.

The cost is up to one dispatch interval (~15s) of delivery latency. For mail
landing in an inbox, that is invisible.

### Claiming is lease-based, not a `sending` status

A `sending` state needs a reaper for rows whose worker died mid-flight. Instead
the claim query pushes `next_attempt_at` forward by a lease window in the same
`UPDATE`, so a crashed run's rows simply become claimable again once the lease
lapses. Self-healing, and no second job to own. `FOR UPDATE SKIP LOCKED` keeps
it correct even though the worker's Redis lock already serializes runs.

Retries back off exponentially: the nth failure waits `30s << (n-1)`, clamped
at 30 minutes, until `EMAIL_MAX_ATTEMPTS` (default 5) marks the row `failed`.

The lease is **derived, not configured**: `WORKER_JOB_TIMEOUT +
EMAIL_DISPATCH_INTERVAL`, floored at 60s. The worker cancels any run at
`WORKER_JOB_TIMEOUT`, so no run outlives it; one interval of margin on top is
the shortest lease that is always safe. Deriving it removes a way to
misconfigure the system into duplicate sends.

## Tokens

Both flows use the same pattern, in two Redis namespaces. Nothing is stored in
Postgres — no token table, and therefore no third cleanup job.

| Key | Value | TTL | Written by |
| --- | ----- | --- | ---------- |
| `verify:email:<sha256hex(token)>` | userId | 24h | register, resend-verification |
| `verify:resend:<userId>` | `1` | 5m | resend-verification (`SET EX NX`) |
| `reset:password:<sha256hex(token)>` | userId | 1h | forgot-password |
| `reset:request:<email>` | `1` | 15m | forgot-password (`SET EX NX`) |

- **The raw token is never a key.** It is `sha256hex(32 bytes of crypto/rand)`,
  matching the `mcp_api_keys` precedent ([`07-sheets-adapter-decisions.md`](07-sheets-adapter-decisions.md) §1).
  Not because Redis is untrusted, but because a `KEYS`/`MONITOR`/RDB dump then
  yields nothing redeemable, and it costs one line.
- **Redemption is `GETDEL`.** Single-use with no read-then-delete race, so a
  replayed token is indistinguishable from an unknown one.
- **Cooldowns are `SET EX NX`.** One round trip, and free of the race between a
  separate `EXISTS` and `SET`.
- **The reset cooldown is keyed by email, not userId** — `forgot-password` runs
  before the address is known to belong to a user, and an id-keyed cooldown
  could not cover the unknown-address path identically. Same convention as
  `login:attempts:<email>`.

### Losing Redis loses pending tokens

Accepted, deliberately. A flush means outstanding links stop working and users
press "resend"; the reset flow degrades the same way. If this ever matters, the
migration path is a token table, not a change to any caller.

The mirror-image gap: `SetVerifyToken` writes to Redis *outside* Postgres'
transaction, so a rolled-back registration leaves an orphan key. It points at a
user id that does not exist, `VerifyEmail` maps that to
`INVALID_VERIFICATION_TOKEN`, and it self-expires. Two-store atomicity is not
achievable and is not attempted.

## Bodies contain live credentials

A rendered verification body carries a working, single-use bearer token in a
URL. Three consequences, all load-bearing:

1. **`body_html`/`body_text` are nullable, and `MarkEmailSent` nulls them** in
   the same statement that flips the status. The row survives as an audit trail
   — recipient, subject, when, how many attempts — with the secret stripped.
   `PruneEmailOutbox` then deletes it entirely past `EMAIL_OUTBOX_RETENTION`.
2. **Never log an `email.Message`.** The centralized redaction in
   `internal/shared/logger/redact.go` cannot help: it matches attribute *keys*,
   and the token is inside a value. A `Sender` error must not carry the body
   either.
3. **The window where a token sits in Postgres** is one dispatch interval on the
   happy path, and `EMAIL_OUTBOX_RETENTION` in the worst case — only for rows
   that never send.

### `LogSender` and the local-env allowlist

With no `RESEND_API_KEY`, the worker falls back to `email.LogSender`, so
`make worker` works with no third-party account. In a local `APP_ENV` it writes
the whole rendered body to the log — that is how a developer gets the
verification link without a mailbox.

Anywhere else it does the opposite: recipient and subject only, and it **returns
an error**. Succeeding silently would mark the row `sent` when nothing was sent,
hiding a real outage behind a green row. The error retries, exhausts the attempt
budget, and leaves `no RESEND_API_KEY configured` in `email_outbox.last_error`,
where an operator will actually find it.

The branch is an **allowlist** of local `APP_ENV` values, not "anything that is
not production". The negated form fails open: a staging or preview deploy with
no key would log live tokens.

## Two decisions worth not re-deriving

### `POST /auth/verify-email`, not `GET`

A `GET` that consumes a single-use token gets fired by link scanners — Outlook
Safe Links, corporate mail proxies, chat unfurlers — which silently burns the
token before the human ever clicks. So the **frontend page** is the GET target
(`/verify-email?token=…`, harmless to prefetch) and that page POSTs the token to
the API.

This is why the frontend's verify-email page guards its POST with a `useRef`
latch: React StrictMode double-invokes effects in development, and the second
call would consume-then-400 an already-spent token, making a working flow look
broken.

### `forgot-password` is uniform, and that asymmetry is intentional

`POST /auth/forgot-password` **always** returns `200 { success: true }` — for an
unknown address, a known one, and one whose 15-minute cooldown is currently
active. There is deliberately no `RESET_TOO_SOON` code: a 429 for a known
address against a 200 for an unknown one leaks exactly what the uniform response
exists to hide.

`POST /auth/resend-verification` *does* return `429
VERIFICATION_RESEND_TOO_SOON`, because it is authenticated — the caller already
knows their own address exists. This is not an inconsistency to clean up later.

A successful reset also sets `is_verified = true`: reaching the link proves
control of the mailbox, which is the only thing verification asserts. It revokes
every session for the user, but already-issued **access** tokens survive to
their own ≤15-minute expiry — only refresh sessions die immediately.

## Enforcement: a banner, not a gate

Nothing in the backend reads `is_verified` except the verification flow itself.
No `RequireVerified` guard, no login gate. The frontend surfaces it through
`GET /auth/me` and nags with `components/verification-banner.tsx`, dismissible
per-tab via `sessionStorage`.

`isVerified` is deliberately **not** a JWT claim: it would go stale for a full
`JWT_ACCESS_EXPIRES_IN` after verifying, and the banner would linger past the
moment it stopped being true. One cheap authenticated read is simpler and always
correct.

Tightening this later is purely additive — a new guard on chosen routes — so
nothing here forecloses it.

## Operating it

**"Emails aren't arriving."** Look at `email_outbox` first:

```sql
SELECT to_address, subject, status, attempts, last_error, next_attempt_at
FROM email_outbox ORDER BY created_at DESC LIMIT 20;
```

- `status='failed'` with a 403 in `last_error` → `EMAIL_FROM` is on a domain
  that is not verified in the Resend dashboard. This is the most likely cause.
- `last_error` mentioning `RESEND_API_KEY` → the key is unset on the **worker**
  service (setting it on the API does nothing; the API never sends).
- Rows stuck `pending` with `next_attempt_at` in the past → the worker isn't
  running. Check its `GET /health` on `WORKER_PORT`.

**Config vars** are listed in [`02-api-contract.md`](02-api-contract.md) §Environment
variables and in `.env.example`. `APP_PUBLIC_URL` is the one most easily got
wrong: it is the browser-facing **frontend** origin the links are built against,
not the API's.
