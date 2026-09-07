# Google credential durability — service-account auth + health-check job — Implementation Plan

> **Status: 🚧 in progress (planned 2026-09-07).** 3 / 9 steps — the backend
> half of the service-account path is done and verified.
>
> **Shipped:** step 2 (`b59bcec`) — `Config.OAuth` became `Config.Credential`,
> a union of `*OAuthConfig` and the new `*ServiceAccountConfig`, with
> `ParseConfig` requiring exactly one (`ErrCredentialAmbiguous` covers both
> and neither). Step 3 (`7ab8aaf`) — `NewTokenSource` /
> `TokenSourceCache.Get` take a `Credential`; a service account goes through
> `jwt.Config.TokenSource`, the OAuth branch is untouched, and a zero
> `Credential` returns an erroring token source rather than a nil one that
> would panic inside a `tools/call`. Step 4 (`41ebc67`) — the seven
> `cfg.OAuth` readers in `internal/module/mcp`, plus three comments the
> rename made inaccurate (two of them inside tool descriptions the model
> reads in `tools/list`).
>
> **Verified live, not read through:** `go build ./...` clean, `gofmt`
> clean, `make lint` 0 issues, and the whole backend suite green against
> real postgres+redis — including the 38 `TestIntegration_MCP*` cases, which
> execute rather than skip (`internal/server` runs 3.9s→49s once
> `DATABASE_URL`/`REDIS_URL` are set; without them the suite prints `ok`
> while skipping every case, which is a trap worth knowing about before
> trusting a green run here).
>
> **A connector can already authenticate as a service account** by posting
> the config directly. What is missing is everything that lets a customer do
> it without curl: steps 5-6 (dashboard + guide) and step 7 (health job).
>
> **Deviations so far, both deliberate:** `checker.go:43` is listed under
> step 4 but landed in step 3 — it is in the adapter package, which could
> not compile or run step 3's own tests without it. And the fixture note in
> step 9 gained a fact found by probing the library: `JWTConfigFromJSON`
> does not parse the PEM, so tests need no real private key, and its error
> quotes fragments of the input — which is why `parseServiceAccount` drops
> that error instead of wrapping it.
>
> **Why this exists:** every `google_sheets` connector onboarded today dies
> after seven days. `internal/adapter/googlesheets/oauth.go:17-20` requests
> `drive.readonly`, which Google classifies as a **restricted** scope. A
> customer whose OAuth app stays in "Testing" publishing status — which is
> every customer, because leaving Testing with a restricted scope requires a
> paid third-party CASA assessment renewed annually — gets a refresh token
> that Google expires after exactly 7 days. The next refresh returns
> `invalid_grant` and the connector goes dark. Nothing in the product
> notices: the worker has no health-check job, so the first signal is the
> customer's agent failing mid-conversation.
>
> This plan does three things, in dependency order: an immediate docs-only
> stopgap so the current demo survives an interview (step 1); a
> service-account credential path that removes the expiry class of bug
> entirely (steps 2-6); and a worker job that catches a dead credential
> before the customer does (step 7).
>
> Target executor: **Sonnet** for steps 1, 4, 5-7, **Opus** for steps 2-3
> (the credential-variant refactor touches eight call sites and the
> fingerprint cache, where a mistake silently keeps minting tokens from a
> retired credential — the fingerprint was, as expected, the one place the
> refactor would have introduced a silent bug).

---

**Target packages:** `apps/backend/internal/adapter/googlesheets`,
`apps/backend/internal/job/connectorhealth` (new),
`apps/frontend/components/connectors`
**Prerequisite:** step 7 needs the `sqlc` CLI (`make sqlc`). Steps 1-6 do not.

---

## 0. Read this first — the seam that makes this cheap

The adapter was built against an `oauth2.TokenSource`, not against OAuth
specifically. `newClient(ctx, ts oauth2.TokenSource, endpoint string)`
(`client.go:53`) is the only place a Google client is constructed, and a
service account's `*jwt.Config` produces an `oauth2.TokenSource` exactly
like a refresh-token flow does. **Nothing below the token source needs to
change**: the `sheetsAPI` interface, all six tools, the allowlist checks,
the audit calls, and the download route are all credential-agnostic already.

Two properties worth confirming before writing code, because they are why
this works at all:

- **`sheets_list_spreadsheets` iterates the connector's own allowlist**, it
  does not search Drive (`list.go:33-37`). A service account, which sees only
  files explicitly shared with it, therefore returns identical results.
- **The allowlist model and service-account sharing are the same shape.**
  Google requires the customer to share each spreadsheet or folder with the
  service-account address; the connector's `scope.spreadsheet_ids` /
  `scope.drive_folder_ids` already require the same enumeration. The two
  become independent layers — a file shared but not allowlisted stays
  unreachable, and vice versa.

Read before starting: `googlesheets/config.go`, `googlesheets/oauth.go`,
`googlesheets/checker.go`, `googlesheets/client.go:53-100`,
`internal/module/mcp/tools_sheets.go:95` (one representative `cfg.OAuth`
call site), `components/connectors/google-sheets-form.tsx`,
`internal/job/sessioncleanup/` (the shape step 7 copies),
`.claude/plans/archives/2026-08-18-sheets-adapter.md` §5.

---

## 1. Stopgap — publishing status, docs only

No code. `apps/frontend/app/(dashboard)/connectors/google-sheets-setup/page.tsx`
currently tells the reader to add themselves as a test user and warns they
must "redo step 4 every week."

Replace that with an explicit instruction to set the OAuth consent screen's
publishing status to **In production** before step 4, and state plainly what
they trade for it: a one-time "Google hasn't verified this app" interstitial
they clear with _Advanced → Go to (unsafe)_. Because the customer is the sole
user of their own OAuth client, the 100-test-user cap and the verification
requirement do not bite; only the warning screen does.

**Verify this by hand before writing the copy.** Google has tightened
unverified-app handling repeatedly, and an unverified production app holding
a _restricted_ scope is exactly the case most likely to be blocked. Create a
throwaway Cloud project, publish it, complete the flow, and confirm a
refresh token issued that way still works on day 8. If Google refuses,
delete this step and say so in the tracker — steps 2-6 are the real fix
regardless, and this stopgap exists only to buy time for them.

Keep the OAuth path documented after step 5 lands. It stays supported.

## 2. `config.go` — a second credential variant

Introduce a credential union alongside the existing scope config. Additive:
a stored config carrying `oauth` keeps parsing exactly as it does today.

```go
// Credential is a google_sheets connector's upstream identity. Exactly one
// variant is ever populated — ParseConfig rejects both and neither.
type Credential struct {
    OAuth          *OAuthConfig
    ServiceAccount *ServiceAccountConfig
}

// ServiceAccountConfig is a Google service-account key. The customer shares
// each spreadsheet or folder with Email the way they would share it with a
// colleague; there is no consent screen, no refresh token, and so nothing
// for Google to expire after seven days.
type ServiceAccountConfig struct {
    // Email is the service account's address, kept for display and for the
    // health check's error messages. Not a credential.
    Email string
    // jwt is the parsed key. Unexported and never serialized: the private
    // key lives here and must not reach a DTO, a log, or an error string.
    jwt *jwt.Config
}
```

- `Config.OAuth OAuthConfig` becomes `Config.Credential Credential`.
- `ParseConfig` accepts `raw["oauth"]` **or** `raw["service_account"]`:
  - both present → `"googlesheets: config must carry exactly one of oauth or service_account"`
  - neither → the same error
- `parseServiceAccount` takes `{"key_json": "<the full JSON key file, as a string>"}`,
  runs it through `google.JWTConfigFromJSON(data, scopes...)` **at parse time**,
  and stores the result. Parsing eagerly is deliberate: it means
  `NewTokenSource` cannot fail, so step 3 does not have to add an error
  return to eight call sites. Extract `client_email` for `Email`.
- Follow the existing error discipline exactly: name the field, never the
  value. A malformed key JSON must not have its bytes wrapped into the
  error — `ParseConfig`'s doc comment already explains why
  (`connector.Service.CheckHealth` logs what a Checker returns).
- Take the key as a JSON **string** rather than a nested object. It is what
  the customer downloads and pastes, and it avoids a marshal/unmarshal round
  trip that could reorder or drop fields in a credential.

## 3. `oauth.go` — branch the token source, widen the fingerprint

```go
func NewTokenSource(ctx context.Context, cred Credential) oauth2.TokenSource
func (c *TokenSourceCache) Get(ctx context.Context, connectorID uuid.UUID, cred Credential) oauth2.TokenSource
```

- `NewTokenSource`: service account → `cred.ServiceAccount.jwt.TokenSource(ctx)`
  (already reuse-caching internally); OAuth → today's
  `ReuseTokenSource` over the refresh token, unchanged.
- **`fingerprint` is the one place a mistake is dangerous.** It currently
  digests `(ClientID, ClientSecret, RefreshToken)`; a service-account
  credential would fingerprint identically for every connector, so a
  rotated key would keep serving from a cached token source that holds the
  retired one. Widen it to cover the variant tag and the service-account key
  material, keeping the existing length-prefixing so `("ab","c")` and
  `("a","bc")` still cannot collide. Digest the raw `key_json` string —
  store it on `ServiceAccountConfig` as an unexported field for exactly this
  purpose, and note in a comment that it exists only to be hashed.
- Update the `scopes` doc comment: it is now the scope set for both
  credential variants, and for a service account it is what
  `JWTConfigFromJSON` signs the assertion for.

## 4. Mechanical: rename the eight `cfg.OAuth` readers

`cfg.OAuth` → `cfg.Credential` at:

- `checker.go:43`
- `internal/module/mcp/handler.go:239`
- `internal/module/mcp/tools_sheets.go:95,173,317,505`
- `internal/module/mcp/tools_drive.go:97,204`

No behavior change; the compiler finds any straggler. Do this as its own
commit so the review of step 3 is not buried under it.

## 5. Frontend — a credential-kind toggle

`components/connectors/google-sheets-form.tsx` holds the shared schema and
fields consumed by both the create dialog (`connectors/page.tsx`) and the
edit page (`connectors/[id]/google-sheets/`), so both pick up the change
from one edit.

- `googleSheetsConfigFieldsSchema` gains `credentialKind: z.enum(["service_account", "oauth"])`
  and `serviceAccountKeyJson: z.string().optional()`.
- Default `credentialKind` to `"service_account"` — the recommended path
  should be the one a customer falls into without choosing.
- `refineGoogleSheetsConfig` branches: service account requires
  `serviceAccountKeyJson` to be non-empty and to `JSON.parse` into an object
  whose `type` is `"service_account"` and which has a `client_email`. Reject
  early and clearly here; a paste error caught in the browser is far cheaper
  than one surfaced as a failed health check.
- `toGoogleSheetsConfig` emits `{service_account: {key_json}, scope: {...}}`
  or today's `{oauth: {...}, scope: {...}}`.
- `GoogleSheetsFormFields` renders the toggle and swaps the credential
  fieldset. Show the parsed `client_email` back to the user under the
  textarea once it parses — it is the address they must share their sheet
  with, and having it echoed removes a copy-paste step.
- The edit page must keep its current behavior of never rendering a stored
  secret back (`[id]/google-sheets/page.test.tsx:107,116` asserts this for
  "Client secret"). Add the equivalent assertion for the service-account key.

## 6. Rewrite the setup guide

`connectors/google-sheets-setup/page.tsx` drops from seven steps to roughly
four for the service-account path:

1. Create a Google Cloud project
2. Enable the Sheets API and the Drive API
3. Create a service account, download its JSON key
4. Share each spreadsheet/folder with the service-account address (Viewer)

Keep the existing OAuth walkthrough, moved below and collapsed, labelled as
the path for customers whose Google Workspace admin blocks sharing outside
the domain. Say that plainly rather than presenting two equal options — the
guide's job is to make the good path obvious.

Call out the one real failure mode up front: **a Workspace tenant with
external sharing disabled cannot share to a `@…gserviceaccount.com`
address.** Those customers need either an admin policy exception or the
OAuth path. Better they learn it in paragraph one than in step 4.

## 7. `internal/job/connectorhealth` — catch a dead credential first

Model on `internal/job/sessioncleanup/`. Nothing here is specific to Google
— any future adapter's `Checker` is swept the same way.

- **New sqlc query** in `queries/connectors.sql` — the existing set is all
  org-scoped and this job crosses orgs deliberately:
  ```sql
  -- name: ListConnectorsForHealthCheck :many
  ```
  Select id, organization_id, type, status, ordered by `last_checked_at`
  nulls first, limited to a batch size. Do **not** select
  `encrypted_config`; the job re-reads each connector through
  `connector.Service` so decryption stays inside the owning service, per
  CLAUDE.md's rule that decrypted config never leaves it. Then `make sqlc`.
- The job calls the existing `CheckHealth` path per connector, so
  `UpdateConnectorHealth` and the `Checker` registry are reused rather than
  reimplemented.
- **On an active→error transition only**, enqueue a notification into
  `email_outbox` in the same style as `internal/module/auth`'s verification
  mail. Transition-triggered, not state-triggered — a connector that is
  already broken must not email the org every interval.
- Recipient: the organization's owner. Keep the body free of any connector
  config, and of the upstream error string; name the connector and link to
  `/connectors/:id`. The upstream error can quote a credential-adjacent
  detail, and CLAUDE.md forbids that reaching a mail body.
- Env vars, all optional with defaults, matching the existing naming:
  `CONNECTOR_HEALTH_INTERVAL` (6h), `CONNECTOR_HEALTH_BATCH_SIZE` (50).
- Register in `cmd/worker/main.go` next to the other two — one line, per the
  comment already sitting at `main.go:74`.

## 8. Docs

- `CLAUDE.md` — the Connectors bullet: `google_sheets` accepts either a
  service-account key or an OAuth refresh token, service account being the
  default. The Background-worker bullet: a third job. The Environment
  section: the two new vars.
- `docs/06-sheets-adapter.md` §3 — the config shape now has two variants.
- `docs/07-sheets-adapter-decisions.md` — append a decision recording _why_
  service account became the default: `drive.readonly` is restricted, CASA
  is not viable for an SMB customer, and Testing-mode tokens expire in 7
  days. Include the dates and the Google policy this rests on, so a future
  reader can tell whether it still holds.
- `.env.example` and `.env.docker.example` — the two new vars, commented.
- `docs/02-api-contract.md` — only if it documents the connector config
  body; check before editing.

## 9. Verify

```
make test                         # apps/backend
make lint
cd apps/frontend && pnpm test && pnpm lint && pnpm exec tsc --noEmit
```

New tests required:

- `config_test.go` — service-account parse happy path; both variants present
  → error; neither → error; malformed key JSON → error, and **assert the
  error string contains none of the key's bytes**.
- `oauth_test.go` — two different service-account keys fingerprint
  differently; a service-account credential and an OAuth credential
  fingerprint differently; `TokenSourceCache.Get` returns a _new_ source
  after the key JSON changes (the regression step 3 warns about).
- `checker_test.go` — `probe` runs unchanged under a service-account
  credential (mocked `sheetsAPI`, no network).
- `connectorhealth` — a unit test that active→error enqueues exactly one
  outbox row and error→error enqueues none.
- Frontend — the create dialog submits a `service_account` config; the edit
  page never renders a stored key back.

The MCP integration suite (`internal/server/mcp_integration_test.go`, 37
cases) must pass untouched. If any case needs editing, something in steps
2-4 changed behavior that was supposed to be credential-agnostic — stop and
find out what.

Local gotchas that have bitten before: keep LF line endings or `make lint`
flags files this plan never touched; use `127.0.0.1`, not `localhost`, for
Redis in integration tests.

---

## Out of scope — deliberately

**A hosted OAuth consent flow ("Connect with Google" in the dashboard).**
This is the right end state for self-serve onboarding and it is not this
plan. Two viable shapes, neither cheap: keep `drive.readonly` and pay for a
CASA assessment (four figures annually, weeks of lead time, renewed every
year), or drop to `drive.file` plus a Google Picker so the customer selects
files in a Google-hosted dialog — `drive.file` is not a restricted scope, so
no assessment, but it means building the Picker UI, an OAuth callback route,
and per-file grant storage. **Do not start either until customer interviews
establish that self-serve onboarding is what blocks a sale.** White-glove
onboarding for the first handful of design partners costs nothing and
teaches more.

**Write tools.** Unchanged from `docs/07-sheets-adapter-decisions.md` §3.

**Credential rotation reminders.** Service-account keys do not expire, which
is the point of this plan; a policy of rotating them on a schedule is a
later conversation, and the health job above is what would carry it.

---

## Decisions — do not re-litigate without the owner

- **Service account is the default, OAuth stays supported.** Not a
  replacement. The Workspace-external-sharing block is real and the OAuth
  path is the only answer for those customers.
- **The key is stored as a JSON string, not a parsed object.** It is what
  the customer pastes, and round-tripping a credential through a decode and
  re-encode is a way to corrupt one for no benefit.
- **`ParseConfig` parses the JWT config eagerly.** It keeps `NewTokenSource`
  infallible and so keeps the error handling at eight call sites unchanged.
  The alternative — parsing lazily and returning an error from
  `NewTokenSource` — spreads a new error path across the whole mcp module to
  catch a failure that `ParseConfig` already had to catch anyway.
- **The health job re-reads config through `connector.Service`** rather than
  selecting `encrypted_config` and decrypting in the job. Decrypted config
  not leaving the owning service is a CLAUDE.md ground rule, and a
  background job is precisely the kind of caller that erodes it quietly.
- **Health emails fire on transition, not on state.** A connector broken for
  a week must produce one email, not twenty-eight.
