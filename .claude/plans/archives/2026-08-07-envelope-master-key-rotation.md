# Envelope Encryption / Key Management — master-key rotation support — Implementation Plan

> **Status: ✅ All 8 steps complete (2026-08-07).** Verified live, not just
> read through: `go build ./...` and `go vet ./...` are clean; `go test ./...`
> passes across the whole backend, including the 12 new envelope tests
> (21/21 in `internal/shared/envelope`) plus the unrelated `connector` and
> `server` integration suites (unaffected — confirmed by re-running them, not
> assumed). `golangci-lint run` reports only pre-existing gofmt/CRLF churn on
> files this plan never touched — a repo-wide Windows `core.autocrlf`
> artifact already recorded in project memory — and none of the files this
> plan edited are ever in that flagged set (checked directly with
> `gofmt -l`, not inferred from one lint run).
>
> What shipped: `KeyProvider.Unwrap` is now key-ID aware (new
> `ErrUnknownKeyID` sentinel); `EnvKeyProvider` holds a primary master key
> plus zero or more retired keys via `NewEnvKeyProvider(primary, retired...)`,
> loaded from the new `CONNECTOR_MASTER_KEY_PREVIOUS` env var through the new
> `DecodeMasterKeys` helper; `Encryptor.OpenAndRotate` implements
> rotate-on-read — it opens under whichever key the envelope's `kid` names
> and, only when that key is no longer primary, also returns a freshly
> re-sealed replacement for the caller to persist — with plain `Open` reduced
> to a thin wrapper over it. `config.go` and `server.go` wire the retired
> keys through end to end. `.env.example`, `.env.docker.example`,
> `README.md`, and `CLAUDE.md` all document the new variable and the
> rotation procedure (promote → retire → deploy → let rotate-on-read migrate
> rows → drop the retired key).
>
> One correction made along the way, not a deviation: `.env.example`'s
> comment claimed rotating `CONNECTOR_MASTER_KEY` "makes stored connector
> configs unreadable." That was true before this plan and is false after —
> the stale comment was rewritten rather than left in place.
>
> Explicitly out of scope, exactly as the plan's own "Out of scope" section
> called it: `connector.Service` still calls plain `Open`, not
> `OpenAndRotate` — wiring it to persist the rotated blob needs a narrow
> sqlc query touching only `encrypted_config` (the existing `UpdateConnector`
> also rewrites `name`/`status` and bumps `updated_at`, which a silent
> re-encryption must not do) and requires the `sqlc` CLI. That caller-side
> wiring, and a later bulk re-encryption `worker.Job` sweep, remain the next
> tasks — nothing in this change makes rotation active for connector rows yet,
> only possible.
>
> Target executor: **Sonnet**.

---

**Target package:** `apps/backend/internal/shared/envelope`
**Status of the requirement:** mostly already built. This plan closes the one real gap.

---

## 0. Read this first — what already exists

The requirement asks for a standalone envelope-encryption layer with a `KeyProvider`
abstraction. **That package already exists and shipped with the connector module**
(commit `59c32da` / `dfea1d0`), at `apps/backend/internal/shared/envelope/`:

| Requirement from the prompt | Status | Where |
|---|---|---|
| 1. Master key from env, behind a `KeyProvider` interface, KMS-swappable | **Done** | `provider.go`, `envkey.go` (`EnvKeyProvider`) |
| 2. Per-record AES-256-GCM data key, wrapped by master key, never stored beside it | **Done** | `envelope.go` `Seal` |
| 3. Simple public API, callers know no internals | **Done** (different names — see Deviations) | `Encryptor.Seal` / `Encryptor.Open` |
| 4. Version/key-ID tagging so master-key rotation is possible later (rotate-on-read) | **HALF DONE — this is the work** | `sealed.Version`, `sealed.KeyID` are written but `kid` is never read back, and a provider holds exactly one key |
| 5. Never log plaintext / master key / data keys | **Done** | `logger/redact.go` already lists `masterkey`, `datakey`, `connectorconfig`, `encryptedconfig`; envelope package has no logger at all |
| 6. Unit tests: round-trip, tamper detection, wrong key/version | **Done** | `envelope_test.go` (10 tests incl. tamper, wrong AAD, wrong master key, unknown version) |
| Zero dependency on the `connector` module | **Done** | envelope imports nothing from `internal/module` |

**So the actual gap:** every envelope records a `kid`, but `Open` ignores it and
`EnvKeyProvider` holds a single key. Rotating `CONNECTOR_MASTER_KEY` today
**permanently bricks every existing row** — there is no way to open a blob sealed
under the previous key. Rotate-on-read is impossible.

This plan makes rotation actually work: a provider that can hold retired keys, a
`kid`-aware unwrap path, and a rotate-on-read primitive.

Before writing code, read: `envelope/provider.go`, `envelope/envkey.go`,
`envelope/envelope.go`, `envelope/envelope_test.go`, `internal/config/config.go:40-95`,
`internal/server/server.go:75-90`, `internal/module/connector/service.go:50-80`.

---

## 1. `provider.go` — make `Unwrap` key-ID aware

Change the interface:

```go
Unwrap(ctx context.Context, keyID string, wrapped []byte) ([]byte, error)
```

- `keyID` is the `kid` recorded in the envelope at seal time. A provider holding
  retired keys selects on it; a KMS provider may ignore it (the key ARN is inside
  the KMS ciphertext already). Document both cases in the doc comment.
- Add a sentinel next to it:
  ```go
  // ErrUnknownKeyID is returned by a provider asked to unwrap under a master key
  // it does not hold — the signature of a rotation that dropped a key still in use.
  var ErrUnknownKeyID = errors.New("envelope: unknown key id")
  ```
  `Open` still collapses it into `ErrOpen` for callers; the sentinel exists so a
  future rotation job (and the tests below) can tell "wrong key" from "corrupt data".
- `EnvKeyProvider` is the only implementer, so nothing outside this package breaks.

## 2. `envkey.go` — multi-key `EnvKeyProvider`

Signature (variadic keeps all existing call sites compiling):

```go
func NewEnvKeyProvider(primary []byte, retired ...[]byte) (*EnvKeyProvider, error)
```

- Extract the key-ID derivation into a helper `envKeyID(key []byte) string`
  (unchanged logic: `"env:" + hex(sha256(key)[:4])`).
- Struct becomes: `primary []byte`, `primaryID string`, `keys map[string][]byte`
  containing the primary **and** every retired key.
- Validate the length of *every* key; return `ErrMasterKeyLength` if any is wrong.
- Skip a retired key whose id equals `primaryID` (operator listed the same key
  twice — harmless, not worth an error).
- `Wrap` always uses `primary`. `KeyID()` returns `primaryID` — unchanged, so new
  seals always use the current key.
- `Unwrap(ctx, keyID, wrapped)`: look up `keyID` in `keys`; missing → `ErrUnknownKeyID`.
  If `keyID` is empty (an envelope predating this change — none exist in practice,
  but be explicit), fall back to the primary key.

## 3. `envkey.go` + `config.go` — load retired keys from env

- Add `func DecodeMasterKeys(encoded string) ([][]byte, error)`: split on `,`, trim
  space, skip empty entries, run each through the existing `DecodeMasterKey`, and
  wrap any error with the 1-based index of the offending entry. Empty input → `nil, nil`.
- `config.Config`: add
  ```go
  // ConnectorMasterKeysRetired are previous master keys kept for decrypt-only,
  // so rows sealed before a rotation still open. Newest first; optional.
  ConnectorMasterKeysRetired [][]byte
  ```
- In `Load()`, right after the existing `CONNECTOR_MASTER_KEY` block, decode
  `CONNECTOR_MASTER_KEY_PREVIOUS` (comma-separated base64, optional) and append to
  `problems` on failure, matching the surrounding aggregate-all-problems style.

## 4. `envelope.go` — use the `kid`, add the rotate-on-read primitive

- `Open`: pass `s.KeyID` through — `e.provider.Unwrap(ctx, s.KeyID, s.DataKey)`.
- Add the rotation primitive (one decrypt, not two):

  ```go
  // OpenAndRotate opens raw and, when it was sealed under a master key that is no
  // longer the provider's current one, also returns a freshly sealed replacement
  // for the caller to persist. rotated is nil when raw is already current, so the
  // hot path allocates nothing extra.
  //
  // Rotation re-seals with a brand-new data key rather than re-wrapping the old
  // one: same cost, and it retires the old data key too.
  //
  // The caller decides whether to persist rotated — a failed write is not a failed
  // read, and the next read will simply offer the rotation again.
  func (e *Encryptor) OpenAndRotate(ctx context.Context, raw json.RawMessage, aad []byte) (plaintext []byte, rotated json.RawMessage, err error)
  ```

- Keep `Open` as the thin wrapper (`plaintext, _, err := e.OpenAndRotate(...)`) so
  the existing `sealer` interface in `connector/service.go` is untouched.
- Leave `Version` at `1` — the wire format does not change, only how `kid` is used.

## 5. `server.go` — wire the retired keys

`internal/server/server.go:80`:

```go
keyProvider, err := envelope.NewEnvKeyProvider(cfg.ConnectorMasterKey, cfg.ConnectorMasterKeysRetired...)
```

No other wiring changes. `internal/server/auth_integration_test.go:71` keeps compiling
(single-key call).

## 6. Tests — `envelope_test.go` (+ config)

Keep every existing test passing (they encode the security properties; do not weaken
them). Add:

- `TestEnvKeyProvider_Unwrap_RetiredKey` — wrap with provider A (key 1), unwrap with
  provider B (primary key 2, retired key 1) → succeeds.
- `TestEnvKeyProvider_Unwrap_UnknownKeyIDFails` → `ErrUnknownKeyID`.
- `TestEnvKeyProvider_Wrap_AlwaysUsesPrimary` — provider with primary 2 + retired 1;
  the `kid` on a fresh seal is primary 2's id, and provider A (key 1 only) cannot open it.
- `TestEncryptor_OpenAndRotate_AfterKeyRotation` — seal under key 1; open with a
  provider {primary 2, retired 1}: plaintext matches, `rotated != nil`, and the
  rotated blob's `kid` is key 2's id, opens under a key-2-only provider, and no
  longer opens under a key-1-only provider.
- `TestEncryptor_OpenAndRotate_NoRotationWhenCurrent` — `rotated` is nil.
- `TestEncryptor_OpenAndRotate_RetiredKeyDroppedFails` — seal under key 1, open with
  a key-2-only provider → `ErrOpen` (the "you rotated too early" failure).
- `TestEncryptor_Open_TamperedWrappedDataKeyFails` — flip a byte in `dek` → `ErrOpen`
  (complements the existing ciphertext-tamper test).
- `TestNewEnvKeyProvider_RejectsWrongLengthRetiredKey` → `ErrMasterKeyLength`.
- `TestDecodeMasterKeys` — table: empty → nil; one valid; two valid; whitespace and
  trailing comma tolerated; one bad entry → error mentioning its index.
- If `internal/config` has tests, add a case for `CONNECTOR_MASTER_KEY_PREVIOUS`
  (absent → nil, one key, malformed → boot error). Grep for `config_test.go` first.

## 7. Docs

- `.env.example` and `.env.docker.example` — add a commented-out
  `CONNECTOR_MASTER_KEY_PREVIOUS=` with a one-line note that it is decrypt-only and
  is removed once every row has been read at least once under the new key.
- `CLAUDE.md`:
  - Environment section — list `CONNECTOR_MASTER_KEY_PREVIOUS` as optional.
  - The envelope ground-rule bullet — add that rotation is: promote the new key to
    `CONNECTOR_MASTER_KEY`, move the old one to `CONNECTOR_MASTER_KEY_PREVIOUS`,
    deploy, let rotate-on-read migrate rows, then drop the retired key.
- `README.md` — only if it enumerates env vars; check before editing.

## 8. Verify

```
make test     # apps/backend
make lint     # golangci-lint
```

Local-env gotchas that have bitten before: files written with CRLF break `make lint` —
keep LF endings. Redis must be `127.0.0.1`, not `localhost`, if you run integration tests.

---

## Out of scope — explicitly the next task

**Caller-side write-back.** After this lands, `OpenAndRotate` exists but nothing calls
it: `connector/service.go` still uses `Open`. Finishing rotate-on-read means switching
the connector service to `OpenAndRotate` and persisting `rotated` best-effort (same
rule as audit-log writes: log the failure, never fail the request). That needs a new
narrow sqlc query — the existing `UpdateConnector` also rewrites `name`/`status` and
bumps `updated_at`, which a silent re-encryption must not do — so it costs a
`RewrapConnectorConfig` query plus `make sqlc` (requires the sqlc CLI). Left out
deliberately: the prompt scopes this task to the key-management layer, and the
requirement says "full rotation logic can be a later task."

A bulk re-encryption `worker.Job` (sweep rows whose `kid` is stale so the retired key
can be dropped on a schedule rather than whenever a row happens to be read) is the
task after that.

---

## Deviations from the prompt — decided, do not re-litigate

- **Location is `internal/shared/envelope`, not `internal/pkg/crypto`.** The prompt
  says `internal/pkg/crypto/` "or match existing pkg conventions". This repo uses
  `internal/shared/` (per `CLAUDE.md` layout), and the package already exists there
  with a production caller. Do not move or rename it.
- **API is `Seal`/`Open`, not `Encrypt`/`Decrypt`.** The prompt asks for
  `Encrypt(plaintext) (ciphertext, wrappedKey, err)`. The shipped API instead takes a
  `ctx` (KMS calls are network calls) and an `aad` tenant binding, and returns one
  self-describing JSON envelope rather than two loose byte slices. The AAD is a real
  security property — a connector row copied into another org fails to open instead of
  decrypting silently (`TestEncryptor_Open_WrongAADFails`) — and the single blob is
  what makes the `v`/`kid` tagging this plan depends on possible. Renaming would churn
  the connector service and the DB column shape for no gain. Keep `Seal`/`Open`.

## Commit history

Target executor was Sonnet, so intermediate commits followed a repo-configured
auto-commit hook after each edit burst rather than a hand-curated commit plan
(matching the pattern already noted in the connector-module plan's archive).
The work landed as `dfbc86e` (this plan) and `e169153` (`feat(envelope):
implement master key rotation support with previous key handling`) — every
step builds and passes tests individually.
