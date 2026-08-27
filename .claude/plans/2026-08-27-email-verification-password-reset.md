# Email verification + password reset — transactional email via Resend

> **Status: planned 2026-08-27. Not started.**
>
> **Read first:** `CLAUDE.md`, `docs/02-api-contract.md` (§Auth, §Error responses),
> `apps/backend/internal/module/auth/`, `apps/backend/internal/worker/job.go`,
> `apps/backend/internal/job/sessioncleanup/service.go` (the job shape this copies),
> `apps/backend/internal/infra/redis/auth.go` (the Redis-helper shape this copies).
>
> **Reference implementation:** `../agritech` shipped the same feature on a
> TypeScript/Elysia/BullMQ stack (`.claude/plans/archives/20260529-16-email-verify.md`).
> This plan keeps its *product* shape — link-based verification, Redis tokens,
> enumeration-safe reset, unverified users never blocked — and deliberately departs
> from its *delivery* mechanism, because Sapanjai has no job queue. See §2.
>
> **Execution rule:** phased, and **each phase is an approval gate**. Stop after
> each phase, report what landed, wait. Approving one phase never approves the next.

---

## 1. Decisions already taken by the owner (do not re-litigate)

1. **Delivery: transactional outbox + worker job.** Not fire-and-forget goroutines,
   not synchronous sends inside the request.
2. **Enforcement: banner only.** An unverified user is never blocked from anything.
   No `RequireVerified` guard, no login gate. (Tightening later is additive — a new
   guard on chosen routes — so nothing here forecloses it.)
3. **Token store: Redis, SHA-256 hashed.** No token table, no second cleanup job.
4. **Scope: email verification *and* password reset.** They share the sender, the
   outbox, the template layout, and the token pattern; building them together is
   cheaper than building them twice.

Two smaller calls this plan makes on its own — flagged so they can be vetoed cheaply:

- **`POST /auth/verify-email`, not `GET`.** Agritech used `GET /auth/verify-email?token=`.
  A GET that consumes a single-use token is fired by link scanners — Outlook Safe Links,
  corporate mail proxies, chat unfurlers — which silently burns the token before the
  human ever clicks. Here the *frontend page* is the GET target (`/verify-email?token=…`,
  harmless to prefetch) and the page POSTs the token to the API. If you'd rather match
  agritech exactly, this is one route-method change.
- **A successful password reset also sets `is_verified = true`.** Reaching the reset
  link proves control of the mailbox, which is the only thing verification asserts.
  Delete one line in `ResetPassword` if you disagree.

**Owner prerequisite (blocks Phase 5 smoke test only):** a sending domain verified in
the Resend dashboard, and a `RESEND_API_KEY` for it. Until that exists, everything
below runs against the log-only sender (§3) with no functional gap.

---

## 2. Why an outbox instead of a goroutine

Agritech enqueued `email.send` onto BullMQ and let a Redis-backed worker retry it.
Sapanjai has no queue — but it *does* have `internal/worker`, a `worker.Job`
scheduler with a Redis lock, per-run timeouts, panic recovery, and a health endpoint,
plus one job already proving the shape. An outbox table turns that scheduler into the
queue at the cost of one migration:

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

Three properties this buys that a goroutine does not:

- **Atomic with the write that caused it.** A user row exists ⟺ its verification email
  is queued. No "registered but the email vanished because the pod restarted."
- **Retries for free**, with the attempt count and last error visible in Postgres when
  someone asks "did the mail actually go out?"
- **The Resend API key lives only on the worker service.** The API process renders and
  enqueues; it never talks to Resend. Smaller blast radius, and one fewer secret on the
  internet-facing service.

The cost is up to ~15s of delivery latency. For a verification email arriving in an
inbox, that is invisible.

**Claiming is lease-based, not a `sending` status.** A `sending` state needs a reaper
for rows whose worker died mid-flight. Instead the claim query bumps `next_attempt_at`
forward by a lease window in the same `UPDATE`, so a crashed run's rows simply become
claimable again when the lease expires. Self-healing, no extra job, and `FOR UPDATE
SKIP LOCKED` keeps it correct even though the worker's Redis lock already serialises runs.

---

## 3. Phase 1 — Email infrastructure (no routes yet)

Ships the sender, the templates, the outbox table, and the dispatch job. Nothing in
`/auth` changes; the feature is invisible. Verifiable on its own by inserting an
outbox row by hand and watching the worker drain it.

### 1.1 Dependency

```
go get github.com/resend/resend-go/v2@v2.28.0
```

(Verified available; latest stable as of 2026-08-27.)

### 1.2 `internal/shared/email` — sender + templates

New package. Mirrors the `internal/shared/envelope` shape: an interface, a real
implementation, and a swap point in `main`.

```go
// Message is one outbound email. Text is the plain-text alternative; both
// parts are always populated so the mail never lands as HTML-only.
type Message struct{ To, Subject, HTML, Text string }

type Sender interface{ Send(ctx context.Context, m Message) error }
```

- `resend.go` — `ResendSender{client *resend.Client, from string}`. Wraps
  `resend-go/v2`. Maps a non-2xx into a plain error; **never** logs the message body
  (it contains a live token).
- `logsender.go` — `LogSender`, used when `RESEND_API_KEY` is unset so `make worker`
  works with no third-party account. Logs recipient + subject at info. It logs the
  **full body** (and therefore the live link, which is the point in dev) **only when
  `APP_ENV != "production"`**; in production with no key it logs recipient + subject
  and a `no RESEND_API_KEY configured` warning, and returns nil. Guard on `APP_ENV`
  explicitly — `logger.SanitizeURI`/`redact.go` key-matching will not save you here,
  because the token is inside a value, not behind a sensitive attr key.
- `templates/` — `//go:embed`ed `html/template` + `text/template` pairs:
  `layout.html`, `verify_email.html` / `.txt`, `password_reset.html` / `.txt`.
  Renderer API: `Render(name string, data any) (html, text string, err error)`.
  Copy tone/palette from the existing dashboard (`apps/frontend/app/globals.css`
  header comment is the design brief); keep it plain, one CTA button, a raw-URL
  fallback line under it, and an explicit expiry sentence ("this link expires in
  24 hours" / "in 1 hour").
- Subjects: `Verify your email address` and `Reset your Sapanjai password`. English —
  the rest of the product is English; agritech's Thai copy does not carry over.

Unit tests: rendering produces both parts, the CTA href equals the passed URL, the
plain-text part contains the URL, and `LogSender` in `production` mode does not emit
the URL.

### 1.3 Config

`internal/config/config.go` — new fields, all optional with defaults so no existing
deployment breaks:

| Var | Default | Notes |
| --- | --- | --- |
| `RESEND_API_KEY` | *(empty)* | Empty ⇒ `LogSender`. Only the **worker** needs it. |
| `EMAIL_FROM` | `Sapanjai <noreply@localhost>` | Must be a Resend-verified domain in prod. |
| `APP_PUBLIC_URL` | `http://localhost:4000` | Browser-facing frontend base; the link origin. **Not** `BACKEND_URL`. |
| `EMAIL_DISPATCH_INTERVAL` | `15s` | Job interval ⇒ also its Redis lock TTL. |
| `EMAIL_DISPATCH_BATCH_SIZE` | `20` | Rows claimed per run; bound it like `SESSION_CLEANUP_BATCH_SIZE` (1–500). |
| `EMAIL_MAX_ATTEMPTS` | `5` | Then `status='failed'`. |
| `EMAIL_OUTBOX_RETENTION` | `168h` | Prune `sent`/`failed` rows older than this. |

Validate the same way the existing block does: aggregate problems, reject
non-positive durations, bound the batch size. Add a `Config.EmailEnabled()` helper
(`RESEND_API_KEY != ""`).

Token TTLs (24h verify, 1h reset) and the resend cooldowns (5 min / 15 min) stay
**constants** in the auth package, not env vars — they are product decisions, not
deployment knobs, and every extra env var is another thing to get wrong on Railway.

Also update `.env.example`, `compose.yaml` (pass the three new vars to the `worker`
service; the `api` service needs only `APP_PUBLIC_URL`), and
`docs/09-railway-deploy.md`'s env table.

### 1.4 Migration `00010_email_outbox.sql`

Additive-forward, new file, never edit an applied one.

```sql
-- +goose Up
CREATE TABLE "email_outbox" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"to_address" text NOT NULL,
	"subject" text NOT NULL,
	"body_html" text,
	"body_text" text,
	"status" text DEFAULT 'pending' NOT NULL,
	"attempts" integer DEFAULT 0 NOT NULL,
	"last_error" text,
	"next_attempt_at" timestamp DEFAULT now() NOT NULL,
	"sent_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "email_outbox_status_check" CHECK ("status" IN ('pending','sent','failed'))
);
CREATE INDEX IF NOT EXISTS "idx_email_outbox_claim" ON "email_outbox" ("next_attempt_at") WHERE "status" = 'pending';
CREATE INDEX IF NOT EXISTS "idx_email_outbox_prune" ON "email_outbox" ("status", "updated_at");

-- +goose Down
DROP TABLE "email_outbox";
```

`body_html`/`body_text` are **nullable on purpose**: a rendered verification body
contains a live bearer token, so `MarkEmailSent` sets both to `NULL` in the same
statement that flips the status. The row survives as an audit trail (who, what
subject, when, how many attempts) with the secret stripped. Retention then deletes it
entirely after a week.

### 1.5 sqlc queries — `internal/infra/database/queries/email_outbox.sql`

- `EnqueueEmail :one` — INSERT … RETURNING.
- `ClaimPendingEmails :many` — the lease claim:
  ```sql
  UPDATE email_outbox SET
      attempts = attempts + 1,
      next_attempt_at = now() + (sqlc.arg(lease_seconds)::int * interval '1 second'),
      updated_at = now()
  WHERE id IN (
      SELECT id FROM email_outbox
      WHERE status = 'pending' AND next_attempt_at <= now()
      ORDER BY next_attempt_at
      LIMIT sqlc.arg(batch_size)
      FOR UPDATE SKIP LOCKED
  )
  RETURNING *;
  ```
- `MarkEmailSent :exec` — `status='sent', sent_at=now(), body_html=NULL, body_text=NULL, last_error=NULL`.
- `RescheduleEmail :exec` — `last_error=$2, next_attempt_at = now() + ($3 * interval '1 second')`.
- `MarkEmailFailed :exec` — `status='failed', last_error=$2, body_html=NULL, body_text=NULL`.
- `PruneEmailOutbox :execrows` — delete `status IN ('sent','failed') AND updated_at < now() - retention`, `LIMIT`-batched like `DeleteExpiredSessions`.

Then `make sqlc`.

### 1.6 Worker job `internal/job/emaildispatch/`

`Job` implementing `worker.Job`. `Name() = "email-dispatch"`, `Interval() =
cfg.EmailDispatchInterval`.

`Run`:
1. `ClaimPendingEmails(batchSize, leaseSeconds)` where `leaseSeconds = 2 × interval`,
   floored at 60s — long enough that a run that dies mid-send doesn't get its rows
   re-claimed by the next tick while the first send is still in flight.
2. For each row: `sender.Send(ctx, Message{...})`.
   - success → `MarkEmailSent`
   - failure and `attempts >= EMAIL_MAX_ATTEMPTS` → `MarkEmailFailed` + `log.Error`
   - failure otherwise → `RescheduleEmail` with exponential backoff
     (`min(30s × 2^(attempts-1), 30m)`), truncated error string (cap `last_error` at
     ~500 chars so a giant upstream body doesn't bloat the row).
   - A single row's failure must **not** abort the batch.
3. Prune, at most hourly, tracked by an in-struct `lastPrunedAt`. Per-replica state,
   but the DELETE is idempotent and the Redis lock already serialises runs, so a
   redundant prune is harmless. (Alternative if you'd rather keep jobs single-purpose:
   register a second `email-outbox-prune` job at a 24h interval — one extra line in
   `cmd/worker/main.go`. Folded in here to keep one job per feature.)
4. Return `worker.Result{Affected: sent, Attrs: []any{"claimed", n, "failed", f}}`.

Never log `Message.HTML`/`.Text` or the recipient's token. Recipient address is fine
(`to` is not in the redaction set and is genuinely needed for debugging).

Register in `cmd/worker/main.go` — one line, next to `sessioncleanup.New(...)`, plus
the `Sender` construction (`ResendSender` when `cfg.EmailEnabled()`, else `LogSender`).

**Phase 1 done when:** `make migrate && make sqlc && make test && make lint` are clean;
`INSERT INTO email_outbox (to_address, subject, body_html, body_text) VALUES (…)` by
hand is picked up within one interval and flips to `sent` with the bodies nulled; the
worker's `GET /health` on `:3001` shows the new job's run stats.

---

## 4. Phase 2 — Backend: verification

### 2.1 Redis helpers — `internal/infra/redis/email.go`

New `Email` type wrapping the client, same shape as `Auth`. Keys (add them to
CLAUDE.md's Redis-key list):

| Key | Value | TTL |
| --- | --- | --- |
| `verify:email:<sha256hex(token)>` | userId | 24h |
| `verify:resend:<userId>` | `1` | 5m |

Methods:

- `SetVerifyToken(ctx, tokenHash string, userID uuid.UUID) error` — `SET … EX 86400`.
- `ConsumeVerifyToken(ctx, tokenHash) (uuid.UUID, bool, error)` — **`GETDEL`**, so
  consumption is atomic and single-use with no read-then-delete race. Redis 7 is
  already the pinned version, so `GETDEL` is available.
- `MarkVerifyResent(ctx, userID) (bool, error)` — `SET … EX 300 NX`; returns false
  when the cooldown is already active. One round trip instead of the `EXISTS`-then-`SET`
  pair agritech used, and free of the race between them.

**Tokens are hashed before they become a key.** `sha256hex(32 bytes of crypto/rand)`,
matching the `mcp_api_keys` precedent (`docs/07-sheets-adapter-decisions.md` §1) — not
because Redis is untrusted, but because a `KEYS`/`MONITOR`/RDB dump then yields nothing
usable, and it costs one line.

### 2.2 sqlc — `users.sql`

```sql
-- name: MarkUserVerified :exec
UPDATE users SET is_verified = true, updated_at = now() WHERE id = $1;
```

(`users.is_verified` **already exists** — migration `00001`, default `false`. No
schema change is needed for the column, exactly as in agritech.)

### 2.3 apperror codes

Add to `internal/shared/apperror` **and** to the table in `docs/02-api-contract.md`
(they must stay in sync — the contract is the source of truth):

| Code | Status | Message |
| --- | --- | --- |
| `ALREADY_VERIFIED` | 409 | Email already verified |
| `INVALID_VERIFICATION_TOKEN` | 400 | Invalid or expired verification token |
| `VERIFICATION_RESEND_TOO_SOON` | 429 | Verification email already sent, try again in a few minutes |

`USER_NOT_FOUND` (404) already exists and is reused.

### 2.4 `auth.Service` — new file `verification.go`

Widen `authStore` with `MarkUserVerified`, `GetUserByID`, `EnqueueEmail`; add an
`emailQueue`-style narrow interface for the Redis `Email` helper and a `renderer`
interface for the template package, so the service stays unit-testable with hand mocks
(the established pattern — see `authStore`/`loginLimiter`).

New `Service` fields: `mail` (redis email helper), `render` (templates), `appURL` string.

- `SendVerificationEmail(ctx, q db.Querier, userID uuid.UUID, email string, displayName *string) error`
  — generates the token, `SetVerifyToken`, builds
  `<APP_PUBLIC_URL>/verify-email?token=<raw>`, renders, and `EnqueueEmail` **through
  the passed `q`** so it can run inside a caller's transaction.
- `VerifyEmail(ctx, token string) error` — `ConsumeVerifyToken` → miss ⇒
  `INVALID_VERIFICATION_TOKEN`; load user → missing ⇒ same code (never leak whether the
  token was wrong or the user was deleted); already verified ⇒ return nil (idempotent —
  the token is gone either way); else `MarkUserVerified` + audit `user.email_verified`.
- `ResendVerificationEmail(ctx, userID uuid.UUID) error` — load user → `USER_NOT_FOUND`;
  `is_verified` ⇒ `ALREADY_VERIFIED`; `MarkVerifyResent` false ⇒
  `VERIFICATION_RESEND_TOO_SOON`; else send.

**`Register` becomes transactional.** Today `CreateUser` runs bare. Wrap
`CreateUser` + `SendVerificationEmail`'s enqueue in `store.WithTx` so the outbox row
and the user row commit together — the whole point of §2. The `GetUserByEmail`
pre-check and the audit write stay outside the tx (audit writes are best-effort and
must not roll a registration back).

Note the ordering constraint: the Redis `SetVerifyToken` happens *outside* Postgres'
transaction, so a rolled-back registration leaves an orphan Redis key. Harmless — it
points at a user id that does not exist, `VerifyEmail` maps that to
`INVALID_VERIFICATION_TOKEN`, and it self-expires in 24h. Write this down in the code
comment rather than trying to make two stores atomic.

### 2.5 Routes

`docs/02-api-contract.md` §Auth gains:

| Method/Path | Guard | Body | Response |
| --- | --- | --- | --- |
| `POST /auth/verify-email` | public | `{ token }` | `{ success: true }` |
| `POST /auth/resend-verification` | auth | — | `{ success: true }` |
| `GET /auth/me` | auth | — | `{ id, email, displayName, isVerified, createdAt }` |

`GET /auth/me` is new and is what surfaces `isVerified` to the frontend. **Do not add
`isVerified` to the JWT claims** — the contract pins access-token claims at
`{ sub, email }` (and `sub` only after `/auth/refresh`), a claim goes stale for up to
15 minutes after verifying, and the banner would linger past the moment it stops being
true. One cheap authenticated read is simpler and always correct.

Handlers go in `handler.go` alongside the existing four, with swaggo annotations
matching the house style. `resend-verification` and `me` mount on a sub-group carrying
`guards.RequireAuth`.

### 2.6 Audit action

`auditlog.ActionUserEmailVerified = "user.email_verified"`, plus the entry in
`docs/02-api-contract.md`'s recorded-actions line and in the frontend's
`KNOWN_ACTIONS` list (`app/(dashboard)/activity/`) so it is filterable.

### 2.7 Tests

Unit (`verification_test.go`, hand mocks per house style):
- register enqueues exactly one outbox row, in the same tx as the user insert
- verify: valid → `is_verified` true + audit written; unknown token → `INVALID_VERIFICATION_TOKEN`;
  **replayed token → `INVALID_VERIFICATION_TOKEN`** (single-use via `GETDEL`);
  already-verified user with a live token → nil, no second audit row
- resend: already verified → `ALREADY_VERIFIED`; cooldown active → `VERIFICATION_RESEND_TOO_SOON`;
  happy path → enqueues

Integration (real pg+redis, fake `Sender`): each route × happy path × every error code,
per CLAUDE.md's testing expectations.

**Phase 2 done when:** `make test && make lint && make swagger` clean; a live
`register → read the link out of the worker log → POST /auth/verify-email → GET /auth/me`
round trip shows `isVerified: true`, and replaying the same token returns 400.

---

## 5. Phase 3 — Backend: password reset

Same infrastructure, second token namespace.

### 3.1 Redis keys

| Key | Value | TTL |
| --- | --- | --- |
| `reset:password:<sha256hex(token)>` | userId | 1h |
| `reset:request:<email>` | `1` | 15m |

Cooldown is keyed by **email, not user id** — the request endpoint runs before the user
is known to exist, and keying by email is also what makes the unknown-address path
indistinguishable from the known one. Same convention as the existing
`login:attempts:<email>`.

### 3.2 apperror

| Code | Status | Message |
| --- | --- | --- |
| `INVALID_RESET_TOKEN` | 400 | Invalid or expired password reset token |

No `RESET_TOO_SOON` code: see below.

### 3.3 Service methods

- `RequestPasswordReset(ctx, email string) error` — **always returns nil.** In order:
  `MarkResetRequested` (false ⇒ return nil, log at info, send nothing — a cooldown must
  not be observable, or it becomes the enumeration oracle the uniform response was
  meant to close); load user (`pgx.ErrNoRows` ⇒ return nil, send nothing); generate +
  store token; enqueue the reset email; audit `user.password_reset_requested`.
  This is where this plan departs from agritech, which returned `RESET_TOO_SOON` (429)
  from the route — a 429 for a known address vs. a 200 for an unknown one leaks exactly
  what the endpoint is trying not to leak.
- `ResetPassword(ctx, token, newPasswordHash string) error` — `ConsumeResetToken` →
  miss ⇒ `INVALID_RESET_TOKEN`; load user → missing ⇒ same; then in **one transaction**:
  `UpdateUserPassword` + `MarkUserVerified` (§1, the "reaching this link proves mailbox
  control" call) + `RevokeAllUserSessions`. Audit `user.password_reset` after commit.

  Hashing stays in the **handler**, matching `register`'s existing split
  (`bcrypt.GenerateFromPassword(truncatePassword(...), bcryptCost)` — the 72-byte
  truncation must not be skipped here or long passwords silently behave differently
  from registration).

  Already-issued access tokens survive until their ≤15-minute expiry; only refresh
  sessions die immediately. Same caveat agritech documented — note it in the code and
  in the contract row.

### 3.4 sqlc — `users.sql`

```sql
-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;
```

### 3.5 Routes

| Method/Path | Guard | Body | Response |
| --- | --- | --- | --- |
| `POST /auth/forgot-password` | public | `{ email: email }` | **always** `{ success: true }` |
| `POST /auth/reset-password` | public | `{ token, password: min 8 }` | `{ success: true }` — revokes ALL sessions |

Both `noRetry` on the frontend client (a 4xx here is terminal, not a stale-token signal).

### 3.6 Audit actions

`user.password_reset_requested`, `user.password_reset` — plus contract line and
frontend `KNOWN_ACTIONS`.

### 3.7 Tests

- unknown email → 200, **zero** outbox rows
- known email → 200, exactly one outbox row
- second request inside the cooldown → 200, still one outbox row
- reset with a bad/replayed token → `INVALID_RESET_TOKEN`
- reset happy path → password changed, every session revoked (old refresh token now
  401s), `is_verified` true, token single-use
- `password` shorter than 8 → 422 `Validation failed` (validator, before the service)

**Phase 3 done when:** `make test && make lint && make swagger` clean, and the full
forgot → reset → old-refresh-token-rejected loop passes end to end.

---

## 6. Phase 4 — Frontend

`apps/frontend`. Checks after each file: `pnpm lint`, `pnpm exec tsc --noEmit`, `pnpm test`.

### 4.1 `lib/api/endpoints.ts`

```ts
export interface MeResponse { id: string; email: string; displayName: string | null; isVerified: boolean; createdAt: string }
export function me()
export function verifyEmail(input: { token: string })          // noRetry
export function resendVerification()
export function forgotPassword(input: { email: string })        // noRetry
export function resetPassword(input: { token: string; password: string })  // noRetry
```

### 4.2 Pages under `app/(auth)/`

- **`verify-email/page.tsx`** — reads `?token=`, POSTs it **once**. Guard the call with
  a `useRef` latch (or a `useMutation` fired from a one-shot effect): React StrictMode
  double-invokes effects in dev, and the second call would consume-then-400 on an
  already-spent token, making a working flow look broken. States: verifying / success
  (CTA → `/organizations`, and invalidate the `["me"]` query so the banner clears
  immediately) / error (message from `ApiError`, plus "sign in to request a new link").
  Missing `token` → render the error state without calling the API.
- **`forgot-password/page.tsx`** — email field, zod + react-hook-form, matching
  `register/page.tsx` exactly. On submit **always** swap to the same
  "If an account exists for that address, we've sent a reset link" panel, success or
  failure. Link back to `/login`.
- **`reset-password/page.tsx`** — token from query; password + confirm with a
  client-side match check; on success toast + `router.replace("/login")`. `INVALID_RESET_TOKEN`
  → inline error plus a link back to `/forgot-password`.

Register all three by adding the directories — App Router picks them up; there is no
central route table to edit (unlike agritech's `routes/index.tsx`).

### 4.3 `login/page.tsx`

Add a "Forgot password?" link next to the password field's label, styled like the
existing `Link` to `/register`.

### 4.4 `components/verification-banner.tsx`

`useQuery({ queryKey: ["me"], queryFn: me })`; renders nothing while loading, when
`isVerified`, or when dismissed. Otherwise a `Callout`-styled amber strip (reuse
`components/callout.tsx`) with the text, a **Resend** button (mutation → toast on
success; on 429 show the cooldown message from `ApiError.message`), and a dismiss ✕
persisted in `sessionStorage` (per-tab, comes back next session — deliberately not
`localStorage`; the point is a nag).

Mount above the page content in `app/(dashboard)/layout.tsx`.

Deliberately **not** in `lib/auth/use-session.tsx`: that file is on the boot path and
already carries a subtle hydration contract (the "always starts at loading" comment).
Keeping `isVerified` in a component-local query leaves it untouched.

### 4.5 Frontend tests

`page.test.tsx` alongside each new page, matching the existing house style: verify-email
success/error/missing-token and **exactly one POST under StrictMode**; forgot-password
shows the identical panel on 200 and on 500; reset-password mismatch guard and success
redirect; banner hidden when verified, visible when not, hidden after dismiss.

**Phase 4 done when:** lint + tsc + tests clean, and `make api && make worker && make web`
gives a full click-through: register → banner appears → link from the worker log →
`/verify-email?token=…` → success → banner gone.

---

## 7. Phase 5 — Docs, ops, live smoke test

- **`docs/02-api-contract.md`** — the five new routes, four new error codes, three new
  audit actions. This is the source of truth; it must match the Go exactly.
- **`docs/10-transactional-email.md`** (new, follows the `docs/NN-` convention) — the
  outbox rationale from §2, the lease-claim mechanism, the token/TTL/cooldown table,
  the "bodies nulled on send" reasoning, and the enumeration-safety argument for the
  uniform `forgot-password` response. This is the file to update when the design
  changes, not this plan.
- **`CLAUDE.md`** — extend the **Auth** bullet (verification + reset), the **Background
  worker** bullet (second job), the **Redis keys** line (four new keys), and the
  **Environment** section (seven new vars). Add a ground rule: *rendered email bodies
  contain live bearer tokens — never log a `Message`, and null the body columns on send.*
- **`.env.example`**, **`compose.yaml`**, **`docs/09-railway-deploy.md`** env table.
- **`make swagger`** — regenerate and commit `apps/backend/docs/`.
- **Live smoke** (needs the owner's verified Resend domain): set `RESEND_API_KEY` +
  `EMAIL_FROM` on the worker service only, register with a real address, confirm the
  mail arrives, the link works, `email_outbox` shows `sent` with null bodies, and the
  API service log contains no token and no email body.

---

## 8. Risks and things that will bite

- **Redis flush loses pending tokens.** Accepted (decision 3). Users hit "resend". The
  reset flow degrades the same way. If this ever matters, the migration path is a token
  table, not a change to any caller.
- **Token in a Postgres row between enqueue and send.** Bounded to one dispatch
  interval on the happy path, and to `EMAIL_OUTBOX_RETENTION` in the worst case only
  for rows that never send. Mitigated by nulling bodies on send/fail.
- **Link scanners.** Addressed by POST-not-GET (§1). If you override that decision,
  expect intermittent "invalid token" reports from users on corporate mail.
- **StrictMode double-consume.** The single most likely bug in Phase 4. The ref latch
  is not optional.
- **`EMAIL_FROM` on an unverified domain** → Resend 403s every send, all five attempts,
  then `status='failed'`. Visible in `email_outbox.last_error`; that is the first place
  to look when "emails aren't arriving."
- **Two-store atomicity** (Postgres tx + Redis token) is not achievable and is not
  attempted. Orphan Redis keys are self-expiring and inert.
- **Cooldown observability.** `resend-verification` is authenticated, so its 429 is
  fine. `forgot-password` is public and must never distinguish states — that asymmetry
  is intentional, not an inconsistency to "clean up" later.

## 9. Build order

1 → 2 → 3 → 4 → 5. Phase 1 is a hard prerequisite for 2 and 3; 2 and 3 are independent
of each other and could swap; 4 needs both backend phases merged; 5 closes out.
