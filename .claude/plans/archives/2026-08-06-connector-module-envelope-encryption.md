# Connector module — generic skeleton with envelope-encrypted config — Implementation Plan

> **Status: ✅ All 12 steps complete (2026-08-06).** All Definition-of-done
> items verified live, not just read through: `go build`/`go vet` pass;
> `go test ./...` passes both without infra (unit) and with real Postgres +
> Redis (integration, `internal/server` in ~23s not cached); `make sqlc` and
> `make swagger` produce clean regenerations; migration `00007` applies and
> rolls back cleanly. A live smoke test against a running `cmd/api` proved the
> full lifecycle end-to-end — create → 200 with no `config` key and
> `status: "inactive"` → raw DB row shows only the envelope
> (`{v,kid,dek,ct}`, no plaintext secret) → health-check → 501
> `HEALTH_CHECK_UNSUPPORTED` → delete → 404 on re-fetch — and grepped the
> server's log output to confirm no secret ever appears in it.
> `TestIntegration_ConnectorsPermissionEnforcement` exercises real RBAC roles
> (`connector:read`-only, `connector:*` wildcard, no role) rather than fakes.
>
> One deviation from the plan's assumption, not a bug: `db.Connector.LastHealthCheckAt`
> came out of sqlc as `pgtype.Timestamp`, not `*time.Time` — a nullable
> `pg_catalog.timestamp` column only gets the `time.Time` override when
> `NOT NULL`, matching how `AuditLog.UserID`/`OrganizationID` already work as
> `pgtype.UUID`. Handled with a `fromPgTimestamp` helper in `handler.go`
> mirroring `auditlog.go`'s existing `fromPgUUID` pattern — the plan's
> `Connector` field types were otherwise exactly as sqlc generated them.
>
> This is the **generic connector skeleton only**. No FlowAccount / PEAK /
> Xero-specific code. No MCP tools. No frontend. All three are correctly out
> of scope and remain so — see `docs/05-mcp-gateway.md` Phase 2 for what's next.
>
> Target executor: **Sonnet**. This plan is prescriptive — exact file paths,
> copy-paste-ready snippets, and verification commands. Read `CLAUDE.md`,
> `docs/02-api-contract.md` (the contract extended) and `docs/05-mcp-gateway.md`
> (why connectors exist) for context.

## Scope

Add `internal/module/connector/` — an org-scoped CRUD module whose rows carry
customer connection secrets encrypted at rest with envelope encryption, gated
by the (until now unused) `RequirePermission` guard.

1. Migration `00007_connectors.sql` + sqlc queries.
2. `internal/shared/envelope/` — envelope encryption with a swappable
   `KeyProvider` (env-var master key today, KMS/Vault later).
3. `internal/module/connector/` — handler → service → queries, the repo's
   standard module shape.
4. `RequirePermission("connector:read"|"connector:write"|"connector:delete")`
   on every route — the first production use of that guard.
5. A `Checker` interface + empty registry: the health-check endpoint exists,
   per-type probe logic does not.
6. Tests: envelope round-trip (unit), service CRUD + tenant scoping (unit),
   route permission enforcement (unit, fake guards), full path against real
   Postgres + Redis (integration).
7. Wiring + docs: `config`, `server`, `.env*`, `compose`, `k8s`, CI, `docs/02`,
   `docs/03`, `docs/05`, `README.md`, `CLAUDE.md`.

### Non-goals (do not build these)

- **No third-party adapters.** No HTTP client for any accounting/ERP system,
  no per-type config schemas, no `Checker` implementations. `NewRegistry()` is
  wired empty on purpose.
- **No MCP surface.** Connectors are not exposed as MCP tools in this change.
- **No frontend.** No pages, no API client methods in `apps/frontend/`.
- **No KMS/Vault client.** Only the `KeyProvider` seam plus the env-var
  implementation.
- **No key rotation / re-wrap job.** The envelope records a key id so rotation
  is *possible* later; the tooling is out of scope.
- **No secret-versioning or config history.** An update overwrites.
- **No new third-party dependencies.** stdlib `crypto/aes`, `crypto/cipher`,
  `crypto/rand` only — `golang.org/x/crypto` is already vendored but is not
  needed here.

## Decisions locked

Do not re-litigate these while implementing; if one turns out to be wrong,
stop and raise it.

1. **`type` and `status` are plain `text` columns, validated in Go** — no
   Postgres enum, no `CHECK` constraint. Mirrors `memberships.role` (text,
   validated by a `oneof` tag). "Extensible" then means adding one constant in
   `types.go`, with **no migration** — which is the point of the requirement.
2. **`encrypted_config` is `jsonb`, holding a self-describing envelope**
   (`{v, kid, dek, ct}`), not `bytea`. The repo already overrides
   jsonb → `json.RawMessage`, and a versioned JSON envelope survives a format
   change; an opaque blob does not.
3. **AAD binds ciphertext to the owning org.** The organization id is passed as
   AES-GCM additional authenticated data, so a row copied into another tenant's
   `organization_id` fails to open instead of decrypting silently. This is
   cheap, testable defence-in-depth on top of the `WHERE organization_id = $2`
   scoping every query already has.
4. **`CONNECTOR_MASTER_KEY` is required, like the JWT secrets.** A secret store
   with an optional encryption key is a footgun. `config.Load` decodes and
   validates it and aggregates the problem with the rest. Consequence: every
   `.env`, compose file, k8s secret and the CI workflow must be updated in this
   change or the API and worker stop booting.
5. **`server.New` grows an `error` return.** Building the key provider is
   fallible wiring, and swallowing that is not acceptable for a crypto seam.
   Two call sites: `cmd/api/main.go` and `setupTestServer` in
   `internal/server/auth_integration_test.go`.
6. **`type` is immutable after create.** Changing it would leave a config blob
   whose shape belongs to a different upstream. `UpdateRequest` has no `Type`.
7. **Update is read-modify-write, not `COALESCE`/`sqlc.narg`.** The service
   already reads the row (to authorize, to audit, and to keep unchanged fields),
   so it computes final values in Go and issues one plain `UPDATE`. This avoids
   sqlc's nullable-jsonb type inference entirely. Concurrent PATCHes are
   last-write-wins — acceptable, matches the rest of the codebase.
8. **Health check is `POST` and gated by `connector:write`.** It writes
   `status` and `last_health_check_at`. With no `Checker` registered it returns
   **501 `HEALTH_CHECK_UNSUPPORTED`** and touches nothing — an honest stub
   beats a fake 200.
9. **Decrypted config never leaves the service.** No response DTO has a config
   field, no log line carries config, and audit metadata records only
   `{name, type}`.

## Ground truth captured from the codebase

Verified by reading, not assumed:

- Module shape: `dto.go` (request/response structs + `validate` tags),
  `handler.go` (`Register(g *echo.Group, guards *appmw.Guards)`, swaggo
  annotations, `to*Response` mappers at the bottom), `service.go` (narrow
  `*Store` subset interface + `var _ xStore = (*database.Store)(nil)` compile
  check), `service_test.go` (hand-written mocks, no mocking library).
- Services return `*apperror.Error`; the Echo `HTTPErrorHandler` in
  `internal/server/server.go` maps code → (status, message) via
  `apperror.Resolve`. **No HTTP types inside services.**
- `httpx.BindAndValidate` produces 400 `Invalid request body` / 422
  `Validation failed`.
- `guards.RequirePermission(action)` checks the permission **before**
  membership, so a non-member gets `403 Missing permission: <action>`, never
  `Not a member of this organization`. It sets `ctxOrgID` and `ctxMembership`,
  so `appmw.OrgID(c)` and `appmw.MembershipFromContext(c)` work downstream.
  Owners bypass all permission checks (`rbac.Service.HasPermission`).
- sqlc config (`apps/backend/sqlc.yaml`): `emit_interface`, `emit_json_tags`,
  `emit_pointers_for_null_types`, `uuid`→`uuid.UUID`, `timestamp`→`time.Time`,
  `jsonb`→`json.RawMessage`. A nullable `timestamp` becomes `*time.Time`.
- Migrations use `timestamp` (not `timestamptz`) with `DEFAULT now()`, and
  quote every identifier. **Phase 7 hit a real bug writing Go-side
  `time.Time` into these columns** — so `last_health_check_at` is set by SQL
  `now()`, never from Go.
- `auditlog.Service.Record(ctx, action, userID, orgID *uuid.UUID, metadata []byte)`
  is best-effort and returns nothing.
- `subscription.Service.EnforceLimit(ctx, orgID, key, currentCount)` returns
  `apperror.LimitExceeded`; a missing subscription or missing key means
  unlimited. `docs/05` already names `max_connectors` as the intended key.
- `cmd/seed` uses `UpsertPlan` with `ON CONFLICT DO NOTHING`, so adding a limit
  to `defaultPlans` only affects **fresh** databases. Existing rows keep their
  old limits, which resolves to "no `max_connectors` key" → unlimited. That is
  a safe degradation; do not add an UPDATE.
- Integration tests live in `package server_test`, self-skip when
  `DATABASE_URL`/`REDIS_URL` are unset, and share helpers
  (`setupTestServer`, `doJSON`, `doJSONList`, `createOrgWithOwner`,
  `uniqueSlug`, `uniqueEmail`) defined in `auth_integration_test.go` /
  `organization_integration_test.go`.

---

## Step 1 — Migration `00007_connectors.sql`

Create `apps/backend/migrations/00007_connectors.sql`. Match 00005's quoting
and FK-naming style exactly.

```sql
-- +goose Up
CREATE TABLE "connectors" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"organization_id" uuid NOT NULL,
	"name" text NOT NULL,
	"type" text NOT NULL,
	"encrypted_config" jsonb NOT NULL,
	"status" text DEFAULT 'inactive' NOT NULL,
	"last_health_check_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "connectors_organization_id_name_unique" UNIQUE("organization_id","name")
);
ALTER TABLE "connectors" ADD CONSTRAINT "connectors_organization_id_organizations_id_fk" FOREIGN KEY ("organization_id") REFERENCES "public"."organizations"("id") ON DELETE cascade ON UPDATE no action;
CREATE INDEX IF NOT EXISTS "idx_connectors_organization_id" ON "connectors" ("organization_id");

-- +goose Down
DROP TABLE "connectors";
```

Notes:
- `ON DELETE cascade` matches `org_subscriptions`: deleting an org takes its
  connectors (and their secrets) with it.
- The unique constraint is the backstop for the service's name check; a
  concurrent create loses the race and surfaces as a 500, exactly as a
  concurrent org-slug create does today.
- No `CHECK` on `type`/`status` — decision 1.

## Step 2 — sqlc queries

Create `apps/backend/internal/infra/database/queries/connectors.sql`:

```sql
-- name: CreateConnector :one
INSERT INTO connectors (organization_id, name, type, encrypted_config)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetConnector :one
SELECT * FROM connectors WHERE id = $1 AND organization_id = $2;

-- name: GetConnectorByName :one
SELECT * FROM connectors WHERE organization_id = $1 AND name = $2;

-- name: ListConnectorsByOrg :many
SELECT * FROM connectors WHERE organization_id = $1 ORDER BY created_at ASC;

-- name: CountConnectorsByOrg :one
SELECT count(*) FROM connectors WHERE organization_id = $1;

-- name: UpdateConnector :one
UPDATE connectors
SET name = $3, status = $4, encrypted_config = $5, updated_at = now()
WHERE id = $1 AND organization_id = $2
RETURNING *;

-- name: UpdateConnectorHealth :one
UPDATE connectors
SET status = $3, last_health_check_at = now(), updated_at = now()
WHERE id = $1 AND organization_id = $2
RETURNING *;

-- name: DeleteConnector :execrows
DELETE FROM connectors WHERE id = $1 AND organization_id = $2;
```

Then, **from the repo root**, `make sqlc`. This needs the `sqlc` CLI
(`go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`). Never hand-edit
anything under `internal/infra/database/db/`.

Confirm the generated `db.Connector` has:
`ID uuid.UUID`, `OrganizationID uuid.UUID`, `Name string`, `Type string`,
`EncryptedConfig json.RawMessage`, `Status string`,
`LastHealthCheckAt *time.Time`, `CreatedAt`/`UpdatedAt time.Time`.
If `EncryptedConfig` comes out as `[]byte` instead of `json.RawMessage`, that
is fine — adapt the service's types to whatever sqlc emitted; do not fight it.

`DeleteConnector :execrows` returns `(int64, error)` — that is how the service
distinguishes "deleted" from "not yours / not there".

## Step 3 — `internal/shared/envelope/` (envelope encryption)

New package. Name it `envelope`, not `crypto` (which shadows stdlib).

### 3a. `apps/backend/internal/shared/envelope/provider.go`

```go
// Package envelope implements envelope encryption for secrets stored at
// rest. Each payload is sealed with a freshly generated AES-256 data key,
// and that data key is itself sealed by a KeyProvider holding the master
// key. Only the KeyProvider changes when the master key moves from an
// environment variable to KMS/Vault — no stored envelope, and no caller,
// has to change.
package envelope

import "context"

// KeyProvider wraps and unwraps data keys with the master key. This is the
// swap point for a managed key service: a KMS/Vault provider implements the
// same three methods with Wrap/Unwrap as remote calls, and nothing else in
// the codebase moves.
type KeyProvider interface {
	// KeyID identifies the master key that wrapped a data key. It is
	// recorded in every envelope so a later rotation can tell which rows
	// still need re-wrapping. It must never contain key material.
	KeyID() string

	// Wrap seals a data key. The returned blob is opaque to callers and
	// carries whatever the provider needs to reverse it (nonce, key
	// version, KMS ciphertext framing, ...).
	Wrap(ctx context.Context, dataKey []byte) ([]byte, error)

	// Unwrap reverses Wrap.
	Unwrap(ctx context.Context, wrapped []byte) ([]byte, error)
}
```

### 3b. `apps/backend/internal/shared/envelope/envkey.go`

```go
package envelope

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// MasterKeyLen is the required decoded length of CONNECTOR_MASTER_KEY.
const MasterKeyLen = 32 // AES-256

// ErrMasterKeyLength is returned for a master key of the wrong size.
var ErrMasterKeyLength = errors.New("master key must be 32 bytes")

// DecodeMasterKey decodes and validates a base64 (standard encoding) master
// key. config.Load calls this so a bad key is reported at boot alongside
// every other configuration problem, not on the first request.
func DecodeMasterKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode master key: %w", err)
	}
	if len(key) != MasterKeyLen {
		return nil, ErrMasterKeyLength
	}
	return key, nil
}

// EnvKeyProvider wraps data keys with a master key supplied through the
// process environment (CONNECTOR_MASTER_KEY). It is the self-hosted and
// development default; see KeyProvider for the managed-KMS path.
type EnvKeyProvider struct {
	key   []byte
	keyID string
}

// NewEnvKeyProvider builds a provider from an already-decoded master key.
func NewEnvKeyProvider(key []byte) (*EnvKeyProvider, error) {
	if len(key) != MasterKeyLen {
		return nil, ErrMasterKeyLength
	}
	// A truncated hash of the key: enough to tell two master keys apart
	// across a rotation, far too little to attack the key itself.
	sum := sha256.Sum256(key)
	return &EnvKeyProvider{key: key, keyID: "env:" + hex.EncodeToString(sum[:4])}, nil
}

func (p *EnvKeyProvider) KeyID() string { return p.keyID }

func (p *EnvKeyProvider) Wrap(_ context.Context, dataKey []byte) ([]byte, error) {
	return sealAESGCM(p.key, dataKey, nil)
}

func (p *EnvKeyProvider) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	return openAESGCM(p.key, wrapped, nil)
}

var _ KeyProvider = (*EnvKeyProvider)(nil)
```

### 3c. `apps/backend/internal/shared/envelope/envelope.go`

```go
package envelope

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the envelope format version stamped into every sealed blob.
// Open rejects versions it does not know, so a future format change is a
// bump here plus a branch in Open, never a silent misread.
const Version = 1

const dataKeyLen = 32 // AES-256

// ErrOpen is returned for every failure to open a sealed envelope — wrong
// master key, tampered ciphertext, wrong AAD. Callers must not be able to
// tell these apart.
var ErrOpen = errors.New("envelope: cannot open")

// sealed is the stored (jsonb) shape. Field names are short because this
// lives on every row; it is not a human-facing document.
type sealed struct {
	Version int    `json:"v"`
	KeyID   string `json:"kid"` // master key that wrapped DataKey
	DataKey []byte `json:"dek"` // data key, wrapped by the KeyProvider
	Payload []byte `json:"ct"`  // nonce || ciphertext, sealed with the data key
}

// Encryptor seals and opens payloads. Safe for concurrent use.
type Encryptor struct {
	provider KeyProvider
}

// New builds an Encryptor over the given master-key provider.
func New(provider KeyProvider) *Encryptor { return &Encryptor{provider: provider} }

// Seal encrypts plaintext under a data key generated for this call alone
// and returns the JSON envelope to store.
//
// aad is additional authenticated data bound into the ciphertext but not
// stored in it; callers pass the owning tenant id, so a row copied into
// another organization fails to open rather than decrypting silently. Open
// must be given the identical aad.
func (e *Encryptor) Seal(ctx context.Context, plaintext, aad []byte) (json.RawMessage, error) {
	dataKey := make([]byte, dataKeyLen)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("generate data key: %w", err)
	}

	payload, err := sealAESGCM(dataKey, plaintext, aad)
	if err != nil {
		return nil, err
	}

	wrapped, err := e.provider.Wrap(ctx, dataKey)
	if err != nil {
		return nil, fmt.Errorf("wrap data key: %w", err)
	}

	raw, err := json.Marshal(sealed{
		Version: Version,
		KeyID:   e.provider.KeyID(),
		DataKey: wrapped,
		Payload: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return raw, nil
}

// Open reverses Seal. Every failure mode collapses to ErrOpen.
func (e *Encryptor) Open(ctx context.Context, raw json.RawMessage, aad []byte) ([]byte, error) {
	var s sealed
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, ErrOpen
	}
	if s.Version != Version {
		return nil, ErrOpen
	}

	dataKey, err := e.provider.Unwrap(ctx, s.DataKey)
	if err != nil {
		return nil, ErrOpen
	}

	plaintext, err := openAESGCM(dataKey, s.Payload, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

// sealAESGCM returns nonce || ciphertext.
func sealAESGCM(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// openAESGCM reverses sealAESGCM.
func openAESGCM(key, blob, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, ErrOpen
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
```

`[]byte` fields marshal to base64 in JSON automatically — no manual encoding.

## Step 4 — Config

`apps/backend/internal/config/config.go`:

1. Add to the `Config` struct, under the JWT block:
   ```go
   // ConnectorMasterKey is the decoded master key wrapping every connector's
   // data key. Decoded (not raw base64) so a malformed value fails at boot.
   ConnectorMasterKey []byte
   ```
2. In `Load`, after the JWT secret checks, add to the `problems` accumulation:
   ```go
   masterKey, err := envelope.DecodeMasterKey(os.Getenv("CONNECTOR_MASTER_KEY"))
   if err != nil {
       problems = append(problems, fmt.Sprintf("CONNECTOR_MASTER_KEY must be a base64-encoded %d-byte key: %v", envelope.MasterKeyLen, err))
   } else {
       cfg.ConnectorMasterKey = masterKey
   }
   ```
   (An empty variable decodes to zero bytes and fails the length check, so
   "missing" and "malformed" are both covered by one branch.)
3. Import `"github.com/sapanjai/backend/internal/shared/envelope"`. There is no
   cycle: `envelope` imports only stdlib.

Note the package doc already promises Load "fails fast on ANY missing or
invalid required variable and reports all problems at once" — this keeps that
promise. `cmd/worker` also calls `Load`, so the worker now needs the variable
too; it shares `.env.docker` and the k8s secret, so Step 10 covers it.

## Step 5 — New apperror codes

`apps/backend/internal/shared/apperror/apperror.go` — add to the const block
and to `Map`:

```go
ConnectorNameTaken     = "CONNECTOR_NAME_TAKEN"
InvalidConnectorType   = "INVALID_CONNECTOR_TYPE"
HealthCheckUnsupported = "HEALTH_CHECK_UNSUPPORTED"
```

```go
ConnectorNameTaken:     {409, "Connector name already taken"},
InvalidConnectorType:   {422, "Unsupported connector type"},
HealthCheckUnsupported: {501, "Health check not supported for this connector type"},
```

`NOT_FOUND` (404 "Resource not found") already exists and is what every
missing/other-org connector resolves to — do not add a `CONNECTOR_NOT_FOUND`.

## Step 6 — `internal/module/connector/`

Four source files, mirroring `internal/module/organization/`.

### 6a. `types.go`

```go
package connector

import "sort"

// Type identifies a connector's upstream system. The column is plain text
// with no database-level enum, so extending the set is one constant here and
// no migration — see .claude/plans/plan.md decision 1.
type Type string

// TypeGeneric is the placeholder the skeleton ships with. Real types
// (flowaccount, peak, ...) land with their adapters; see docs/05-mcp-gateway.md
// Phase 2.
const TypeGeneric Type = "generic"

var validTypes = map[Type]struct{}{
	TypeGeneric: {},
}

// Status is a connector's last known health.
type Status string

const (
	StatusActive   Status = "active"
	StatusInactive Status = "inactive"
	StatusError    Status = "error"
)

var validStatuses = map[Status]struct{}{
	StatusActive:   {},
	StatusInactive: {},
	StatusError:    {},
}

// RBAC actions gating the connector routes. Owners bypass these entirely
// (rbac.Service.HasPermission); any other member needs a role granting the
// exact action, "connector:*", or "*".
const (
	PermissionRead   = "connector:read"
	PermissionWrite  = "connector:write"
	PermissionDelete = "connector:delete"
)

// IsValidType reports whether s names a known connector type. Used by the
// "connectortype" request validator and re-checked in the service.
func IsValidType(s string) bool {
	_, ok := validTypes[Type(s)]
	return ok
}

// IsValidStatus reports whether s names a known status.
func IsValidStatus(s string) bool {
	_, ok := validStatuses[Status(s)]
	return ok
}

// AllTypes returns every known type, sorted, for docs and error text.
func AllTypes() []string {
	out := make([]string, 0, len(validTypes))
	for t := range validTypes {
		out = append(out, string(t))
	}
	sort.Strings(out)
	return out
}
```

### 6b. `dto.go`

```go
package connector

import (
	"time"

	"github.com/google/uuid"
)

// CreateRequest is the POST /connectors body. Config carries the upstream's
// connection secrets (DB credentials, API keys, ...) — it is sealed before
// it touches the database and is never echoed back in any response.
type CreateRequest struct {
	Name   string         `json:"name" validate:"required,min=1,max=100"`
	Type   string         `json:"type" validate:"required,connectortype"`
	Config map[string]any `json:"config" validate:"required,min=1"`
}

// UpdateRequest is the PATCH /connectors/:connectorId body. Every field is
// optional; an absent field leaves the stored value untouched. There is
// deliberately no Type — changing it would orphan a config blob shaped for a
// different upstream.
type UpdateRequest struct {
	Name   *string        `json:"name" validate:"omitempty,min=1,max=100"`
	Status *string        `json:"status" validate:"omitempty,oneof=active inactive error"`
	Config map[string]any `json:"config" validate:"omitempty,min=1"`
}

// ConnectorResponse is the connector row as clients see it.
//
// It has no config field and must never grow one: the decrypted config is
// the customer's upstream credential, and this module's whole job is that it
// only ever travels service → upstream.
type ConnectorResponse struct {
	ID                uuid.UUID  `json:"id"`
	OrganizationID    uuid.UUID  `json:"organizationId"`
	Name              string     `json:"name"`
	Type              string     `json:"type"`
	Status            string     `json:"status"`
	LastHealthCheckAt *time.Time `json:"lastHealthCheckAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// SuccessResponse is the response body for delete.
type SuccessResponse struct {
	Success bool `json:"success"`
}
```

### 6c. `health.go`

```go
package connector

import "context"

// Checker probes one connector type's upstream. Implementations land with
// their adapters (docs/05-mcp-gateway.md, Phase 2); the skeleton registers
// none, so CheckHealth currently returns HEALTH_CHECK_UNSUPPORTED for every
// type.
type Checker interface {
	// Type is the connector type this checker handles.
	Type() Type

	// Check probes the upstream using the connector's decrypted config. A
	// nil error means healthy.
	//
	// Two contracts implementations must honour: config is the caller's
	// only copy of a customer credential — do not retain it, log it, or
	// send it anywhere but the upstream; and any returned error is logged
	// by the service, so it must not embed credential material (no raw
	// request URLs with keys in them, no echoed request bodies).
	Check(ctx context.Context, config map[string]any) error
}

// Registry maps a connector type to the Checker that probes it.
type Registry map[Type]Checker

// NewRegistry builds a Registry keyed by each checker's Type(). Called with
// no arguments today — see server.New.
func NewRegistry(checkers ...Checker) Registry {
	r := make(Registry, len(checkers))
	for _, c := range checkers {
		r[c.Type()] = c
	}
	return r
}
```

### 6d. `service.go`

Follow `organization/service.go` closely: package doc, compile-time interface
assertions, narrow store interface, documented check order.

```go
// Package connector implements the /connectors module: org-scoped CRUD over
// upstream connections whose credentials are sealed at rest with envelope
// encryption (internal/shared/envelope), plus a health-check stub.
//
// The one invariant worth stating twice: decrypted config never leaves this
// package. It is sealed on the way in, opened only to hand to a Checker, and
// no response DTO or log line carries it.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/envelope"
)

var (
	_ connectorStore = (*database.Store)(nil)
	_ limitEnforcer  = (*subscription.Service)(nil)
	_ sealer         = (*envelope.Encryptor)(nil)
)

// maxConnectorsLimitKey is the plans.limits key gating connector creation,
// mirroring max_members for org invites. An org whose plan omits it is
// unlimited (subscription.Service.EnforceLimit).
const maxConnectorsLimitKey = "max_connectors"

// connectorStore is the subset of *database.Store this service depends on.
type connectorStore interface {
	CreateConnector(ctx context.Context, arg db.CreateConnectorParams) (db.Connector, error)
	GetConnector(ctx context.Context, arg db.GetConnectorParams) (db.Connector, error)
	GetConnectorByName(ctx context.Context, arg db.GetConnectorByNameParams) (db.Connector, error)
	ListConnectorsByOrg(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error)
	CountConnectorsByOrg(ctx context.Context, organizationID uuid.UUID) (int64, error)
	UpdateConnector(ctx context.Context, arg db.UpdateConnectorParams) (db.Connector, error)
	UpdateConnectorHealth(ctx context.Context, arg db.UpdateConnectorHealthParams) (db.Connector, error)
	DeleteConnector(ctx context.Context, arg db.DeleteConnectorParams) (int64, error)
}

// sealer is the subset of *envelope.Encryptor this service depends on,
// narrowed so tests can substitute a fake and so the KMS swap stays a
// constructor-level change.
type sealer interface {
	Seal(ctx context.Context, plaintext, aad []byte) (json.RawMessage, error)
	Open(ctx context.Context, raw json.RawMessage, aad []byte) ([]byte, error)
}

// limitEnforcer is the subset of *subscription.Service create depends on.
type limitEnforcer interface {
	EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error
}

// Service implements connector CRUD and the health-check entry point.
type Service struct {
	store    connectorStore
	crypto   sealer
	audit    *auditlog.Service
	limits   limitEnforcer
	checkers Registry
	log      *slog.Logger
}

// NewService builds a connector Service. checkers is empty today; see
// health.go.
func NewService(store connectorStore, crypto sealer, audit *auditlog.Service, limits limitEnforcer, checkers Registry, log *slog.Logger) *Service {
	return &Service{store: store, crypto: crypto, audit: audit, limits: limits, checkers: checkers, log: log}
}
```

Methods — implement exactly these, with the documented check order:

```go
// Create seals config and inserts a connector, in this order: type validity,
// plan limit, name collision, seal, insert. New connectors start "inactive"
// (the column default) — nothing has probed the upstream yet.
func (s *Service) Create(ctx context.Context, organizationID, actorID uuid.UUID, name, typ string, config map[string]any) (db.Connector, error)
```
- `!IsValidType(typ)` → `apperror.InvalidConnectorType`.
- `CountConnectorsByOrg` + `s.limits.EnforceLimit(ctx, organizationID, maxConnectorsLimitKey, int(count))`.
- `GetConnectorByName`: `err == nil` → `apperror.ConnectorNameTaken`;
  `!errors.Is(err, pgx.ErrNoRows)` → return err.
- `sealed, err := s.sealConfig(ctx, organizationID, config)`.
- `CreateConnector`.
- audit: `s.audit.Record(ctx, auditlog.ActionConnectorCreated, &actorID, &organizationID, metadata)`
  where `metadata, _ := json.Marshal(map[string]string{"name": name, "type": typ})`.
  **Never put config in metadata.**

```go
// List returns organizationID's connectors, oldest first.
func (s *Service) List(ctx context.Context, organizationID uuid.UUID) ([]db.Connector, error)
```

```go
// Get returns one connector scoped to organizationID. A connector belonging
// to another org is indistinguishable from one that does not exist: both are
// NOT_FOUND, so an id cannot be probed for existence across tenants.
func (s *Service) Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error)
```
Implement once as an unexported `get` and reuse from Update/Delete/CheckHealth;
`pgx.ErrNoRows` → `apperror.New(apperror.NotFound)`.

```go
// Update applies the non-nil fields of a patch. It reads the row first (which
// also authorizes it against organizationID), merges in Go, and issues one
// write — see plan decision 7. A supplied config is sealed under a brand-new
// data key; the previous ciphertext is overwritten, not versioned.
func (s *Service) Update(ctx context.Context, organizationID, actorID, connectorID uuid.UUID, name, status *string, config map[string]any) (db.Connector, error)
```
- `existing, err := s.get(...)`.
- `if status != nil && !IsValidStatus(*status)` → `apperror.Forbidden`? **No** —
  use `apperror.New(apperror.InvalidConnectorType)`? Also no. The DTO's
  `oneof=active inactive error` already rejects it with 422; the service does
  not re-check status. Keep the defensive re-check only for `type`, which is
  the extensible set.
- merge: `newName := existing.Name` / `newStatus := existing.Status` /
  `newConfig := existing.EncryptedConfig`, overridden by non-nil inputs.
- `UpdateConnector` → if it returns `pgx.ErrNoRows` (row deleted between read
  and write) → `apperror.NotFound`.
- audit `ActionConnectorUpdated`, metadata `{"name": <final name>}` plus
  `{"config_rotated": "true"}` when config was supplied. Strings only, to match
  the existing metadata shape.

```go
// Delete removes a connector scoped to organizationID.
func (s *Service) Delete(ctx context.Context, organizationID, actorID, connectorID uuid.UUID) error
```
- `rows, err := s.store.DeleteConnector(...)`; `rows == 0` → `apperror.NotFound`.
- audit `ActionConnectorDeleted`, metadata `{"connector_id": connectorID.String()}`.

```go
// CheckHealth probes connectorID's upstream and records the outcome on the
// row. The skeleton registers no Checkers, so every type returns
// HEALTH_CHECK_UNSUPPORTED today and the row is left untouched — the decrypt
// path below only starts executing once a real adapter lands.
func (s *Service) CheckHealth(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error)
```
- `row, err := s.get(...)`.
- `checker, ok := s.checkers[Type(row.Type)]`; `!ok` →
  `apperror.New(apperror.HealthCheckUnsupported)`.
- `config, err := s.openConfig(ctx, organizationID, row.EncryptedConfig)`; on
  error log `s.log.Error("failed to open connector config", "connector_id", row.ID, "error", err)`
  and return the error (→ 500). `envelope.ErrOpen` carries no key material.
- run `checker.Check`; on error log at warn with `connector_id`/`type`/`error`
  and set `status = StatusError`, else `StatusActive`.
- `UpdateConnectorHealth` (SQL sets `last_health_check_at = now()` — never a
  Go-side `time.Time`, per the Phase 7 timestamp bug).

Helpers at the bottom:

```go
// sealConfig marshals and seals a plaintext config for organizationID.
func (s *Service) sealConfig(ctx context.Context, organizationID uuid.UUID, config map[string]any) (json.RawMessage, error) {
	plaintext, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return s.crypto.Seal(ctx, plaintext, aad(organizationID))
}

// openConfig reverses sealConfig.
func (s *Service) openConfig(ctx context.Context, organizationID uuid.UUID, raw json.RawMessage) (map[string]any, error) {
	plaintext, err := s.crypto.Open(ctx, raw, aad(organizationID))
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := json.Unmarshal(plaintext, &config); err != nil {
		return nil, err
	}
	return config, nil
}

// aad binds a connector's ciphertext to its owning organization: a row moved
// to another tenant fails to open instead of decrypting. Defence in depth —
// every query is already scoped by organization_id.
func aad(organizationID uuid.UUID) []byte {
	id := organizationID // avoid taking the address of the parameter's array
	return id[:]
}
```

### 6e. `handler.go`

Mirror `organization/handler.go`: swaggo annotations on every route, mappers at
the bottom.

```go
// Register mounts the six /connectors routes. Every route is gated by an
// RBAC permission rather than plain membership — this is the first
// production use of RequirePermission (see docs/02-api-contract.md).
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.POST("", h.create, guards.RequirePermission(PermissionWrite))
	g.GET("", h.list, guards.RequirePermission(PermissionRead))
	g.GET("/:connectorId", h.get, guards.RequirePermission(PermissionRead))
	g.PATCH("/:connectorId", h.update, guards.RequirePermission(PermissionWrite))
	g.DELETE("/:connectorId", h.delete, guards.RequirePermission(PermissionDelete))
	// Health check writes status and last_health_check_at, so it needs
	// write, not read.
	g.POST("/:connectorId/health-check", h.healthCheck, guards.RequirePermission(PermissionWrite))
}
```

Shared id parsing, mirroring `removeMember`'s comment style:

```go
func connectorID(c echo.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("connectorId"))
	if err != nil {
		// A malformed id can never match a connector row.
		return uuid.Nil, apperror.New(apperror.NotFound)
	}
	return id, nil
}
```

Handlers: bind with `httpx.BindAndValidate`, read the caller with
`appmw.UserID(c)` and the org with `appmw.OrgID(c)` (both set by
`RequirePermission`), return `c.JSON(http.StatusOK, ...)`. `delete` returns
`SuccessResponse{Success: true}`; every other route returns
`ConnectorResponse` (or `[]ConnectorResponse`, built with
`make([]ConnectorResponse, len(rows))` so it serializes as `[]`, not `null`).

Swagger failure annotations per route, e.g. for `create`:
```
// @Failure  400  {object}  httpx.ErrorResponse  "Missing x-organization-id header"
// @Failure  403  {object}  httpx.ErrorResponse  "Missing permission: connector:write / LIMIT_EXCEEDED"
// @Failure  409  {object}  httpx.ErrorResponse  "CONNECTOR_NAME_TAKEN"
// @Failure  422  {object}  httpx.ErrorResponse  "Validation failed / INVALID_CONNECTOR_TYPE"
```
and for `healthCheck` also `// @Failure 501 {object} httpx.ErrorResponse "HEALTH_CHECK_UNSUPPORTED"`.

Mapper:
```go
func toConnectorResponse(row db.Connector) ConnectorResponse {
	return ConnectorResponse{
		ID:                row.ID,
		OrganizationID:    row.OrganizationID,
		Name:              row.Name,
		Type:              row.Type,
		Status:            row.Status,
		LastHealthCheckAt: row.LastHealthCheckAt,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
	// Intentionally does not map EncryptedConfig. Do not add it.
}
```

## Step 7 — Audit actions

`apps/backend/internal/module/auditlog/service.go` — add to the const block:

```go
ActionConnectorCreated = "connector.created"
ActionConnectorUpdated = "connector.updated"
ActionConnectorDeleted = "connector.deleted"
```

Unlike the parity-only constants above them, all three **are** written.

## Step 8 — Validator registration

`apps/backend/internal/server/validator.go` — inside `newRequestValidator`,
after the `orgslug` registration:

```go
// registered as "connectortype"; used by connector.CreateRequest.Type. The
// valid set lives in the connector module so adding a type is one constant
// there, not a tag edit here.
_ = v.RegisterValidation("connectortype", func(fl validator.FieldLevel) bool {
	return connector.IsValidType(fl.Field().String())
})
```

Import `"github.com/sapanjai/backend/internal/module/connector"`. No cycle —
`connector` does not import `server`.

## Step 9 — Server wiring (`server.New` returns an error)

`apps/backend/internal/server/server.go`:

```go
// New builds a fully configured Echo instance ... It returns an error when
// wiring that depends on validated-but-fallible configuration fails (today:
// the connector master key).
func New(cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool, rdb *redis.Client) (*echo.Echo, error) {
```

After the existing module wiring, before `return`:

```go
	keyProvider, err := envelope.NewEnvKeyProvider(cfg.ConnectorMasterKey)
	if err != nil {
		return nil, fmt.Errorf("connector master key: %w", err)
	}
	// No Checkers are registered: per-type health probes land with their
	// adapters (docs/05-mcp-gateway.md, Phase 2). Until then every
	// health-check call resolves to 501 HEALTH_CHECK_UNSUPPORTED.
	connectorSvc := connector.NewService(store, envelope.New(keyProvider), auditSvc, subSvc, connector.NewRegistry(), log)
	connector.NewHandler(connectorSvc).Register(e.Group("/connectors"), guards)

	return e, nil
```

Every earlier `return e` in `New` becomes `return e, nil` — there is only one.

Update the two call sites:
- `apps/backend/cmd/api/main.go`: capture the error and exit the way the
  neighbouring bootstrap failures do (`log.Error(...); os.Exit(1)`).
- `apps/backend/internal/server/auth_integration_test.go`, `setupTestServer`:
  ```go
  e, err := server.New(cfg, log, pool, rdb)
  if err != nil {
      t.Fatalf("server.New: %v", err)
  }
  ```
  and add a master key to the hand-built `config.Config`:
  ```go
  ConnectorMasterKey: bytes.Repeat([]byte{7}, envelope.MasterKeyLen), // fixed test key
  ```

Also update the stale sentence in `middleware/auth.go`'s `RequirePermission`
doc comment ("No route in docs/02-api-contract.md currently uses this guard —
it exists for parity ... and is exercised by unit tests only") — the
`/connectors` routes now use it.

## Step 10 — Environment, compose, k8s, CI

`CONNECTOR_MASTER_KEY` is a base64-encoded 32-byte key. Generate with
`openssl rand -base64 32`.

1. **`.env.example`** — after the JWT block:
   ```
   # Master key wrapping every connector's per-row data key (envelope
   # encryption). base64 of exactly 32 bytes: openssl rand -base64 32
   # Rotating this without re-wrapping existing rows makes stored connector
   # configs unreadable.
   CONNECTOR_MASTER_KEY=ZGV2LW9ubHktbWFzdGVyLWtleS0zMi1ieXRlcy0hIQ==
   ```
   Use a real 32-byte base64 value — generate one and paste it; do not ship a
   placeholder that fails to decode, or `make api` breaks out of the box.
2. **`.env.docker`** — same variable, same throwaway-value framing as the JWT
   secrets there (extend that file's "rotate before real use" header comment to
   name it).
3. **`.env`** (untracked, local) — add it too, or nothing runs locally.
4. **`compose.yaml`** — no change needed: `api` and `worker` both read
   `.env.docker` via `env_file`.
5. **`k8s/secret.example.yaml`** — add `CONNECTOR_MASTER_KEY: change-me-base64-32-bytes`
   under `stringData`. It arrives at both pods through the existing
   `secretRef: sapanjai-secret` in `k8s/api/deployment.yaml` and
   `k8s/worker/deployment.yaml`; no deployment edit is needed.
6. **`k8s/README.md`** — add it to the `sapanjai-secret` row of the table.
7. **`.github/workflows/ci.yml`** — add to the `backend` job's `env:` block
   (next to the JWT secrets):
   ```yaml
   CONNECTOR_MASTER_KEY: Y2ktbWFzdGVyLWtleS1ub3QtYS1yZWFsLXNlY3JldCE=
   ```
   Verify the value is valid base64 decoding to exactly 32 bytes before
   committing, e.g. `printf '%s' "$V" | base64 -d | wc -c`.
8. **`cmd/seed/main.go`** — add `max_connectors` to `defaultPlans`:
   `free: 2`, `pro: 10`, `enterprise: -1`. `UpsertPlan` is `DO NOTHING`, so
   this only affects fresh databases; existing rows keep their limits and are
   treated as unlimited. That is intended — do not add an UPDATE.

## Step 11 — Tests

### 11a. `internal/shared/envelope/envelope_test.go` (unit, no infra)

Table-driven where it reads well; explicit cases where it does not.

- **Round-trip**: seal a JSON payload, open it, get the same bytes back.
- **Ciphertext is not plaintext**: the marshalled envelope contains neither the
  secret value nor its key (`bytes.Contains` on the raw JSON).
- **Fresh data key per call**: sealing the same plaintext twice produces
  different `ct` *and* different `dek`.
- **Envelope shape**: unmarshal into a map and assert `v == 1` and `kid` is
  non-empty and has the `env:` prefix.
- **Tampered ciphertext fails**: flip one byte inside `ct` (decode base64,
  mutate, re-encode) → `ErrOpen`.
- **Wrong AAD fails**: seal with org A's id, open with org B's → `ErrOpen`.
  This is the tenant-binding test; name it so.
- **Wrong master key fails**: open with an Encryptor built on a different
  32-byte key → `ErrOpen`.
- **Malformed / unknown version fails**: `[]byte("not json")` and a hand-built
  envelope with `"v": 999` → `ErrOpen`.
- **`DecodeMasterKey`**: valid 32-byte base64 → ok; non-base64 → error;
  base64 of 16 bytes → `ErrMasterKeyLength`; empty string → `ErrMasterKeyLength`.
- **`KeyID` stability**: same key → same id; different key → different id; the
  id never contains the raw key bytes.

### 11b. `internal/module/connector/service_test.go` (unit, hand-mocked store)

Copy the mock idiom from `organization/service_test.go`: a `mockConnectorStore`
struct of func fields, `var _ connectorStore = (*mockConnectorStore)(nil)`, a
`spyQuerier` embedding `db.Querier` for audit assertions, and the
`appErrorCode(t, err)` helper.

Use a **real** `envelope.Encryptor` over a fixed test key rather than a fake
sealer — the round-trip is what these tests should be exercising end to end.
Add a `failingSealer` only for the "decrypt failure surfaces as an error" case.

Cases:
- `Create` happy path: asserts the `CreateConnectorParams.EncryptedConfig`
  passed to the store (a) does not contain the plaintext secret, and (b) opens
  back to the original map under the org's AAD; status is left to the column
  default (params carry no status); one `connector.created` audit row whose
  metadata contains `name`/`type` and **not** the secret.
- `Create` with an unknown type → `INVALID_CONNECTOR_TYPE`, store never called.
- `Create` when `EnforceLimit` returns `LIMIT_EXCEEDED` → propagated, no insert.
- `Create` when the name exists → `CONNECTOR_NAME_TAKEN`, no insert.
- `Create` when `GetConnectorByName` returns a non-`ErrNoRows` error →
  propagated as-is (not swallowed into a 409).
- `Get`/`Update`/`Delete`/`CheckHealth` with `pgx.ErrNoRows` → `NOT_FOUND`
  (this is the cross-tenant case: the store is scoped by org, so another org's
  id simply misses).
- `Get` passes the caller's org id through to `GetConnectorParams` — assert the
  captured params, since that scoping is the whole tenant boundary.
- `List` returns `[]` (non-nil) for an org with no connectors.
- `Update` with only `Name` set: params carry the existing status and the
  **byte-identical** existing `EncryptedConfig` (no gratuitous re-seal).
- `Update` with `Config` set: params carry a *different* ciphertext that opens
  to the new map.
- `Delete` returning 0 rows → `NOT_FOUND`, no audit row.
- `Delete` returning 1 row → `connector.deleted` audit row.
- `CheckHealth` with an empty registry → `HEALTH_CHECK_UNSUPPORTED`, and
  `UpdateConnectorHealth` is never called.
- `CheckHealth` with a fake `Checker` returning nil → status `active` written;
  returning an error → status `error` written. Assert the fake received the
  decrypted config map (proves the seal/open path is wired), and that
  `UpdateConnectorHealthParams` carries no Go-side timestamp.

### 11c. `internal/module/connector/handler_test.go` (unit, no infra)

This is the "authorized vs unauthorized" test that runs without Postgres.
`appmw.NewGuards` takes four interface parameters, so a test in this package
can pass fakes for all of them:

- fake token verifier returning a fixed `(userID, email)`;
- fake blacklist returning `false`;
- fake membership store returning a `db.Membership`;
- fake permission checker whose `HasPermission` consults a
  `map[string]bool` of granted actions.

Build an `echo.New()` with `newRequestValidator`-equivalent validation not
needed for the denial cases, register the handler on `e.Group("/connectors")`,
and drive it with `httptest`. Assert:

- caller granted `connector:read` → `GET /connectors` reaches the service
  (200); `POST /connectors` → 403 with body message exactly
  `Missing permission: connector:write`.
- caller granted `connector:write` but not `connector:delete` →
  `DELETE /connectors/:id` → 403 `Missing permission: connector:delete`.
- caller granted nothing (including non-members, whose `HasPermission` is
  false) → every route 403 with the per-route action in the message, and the
  service is never invoked (assert via a store mock that panics/fails the test
  if called).
- missing `x-organization-id` → 400 `Missing x-organization-id header`.
- a successful `GET /connectors/:id` response body has **no** `config` or
  `encryptedConfig` key (decode into `map[string]any` and assert absence).

### 11d. `internal/server/connector_integration_test.go` (real Postgres + Redis)

`package server_test`, reusing `setupTestServer`, `doJSON`, `doJSONList`,
`createOrgWithOwner`, `uniqueSlug`. Self-skips without `DATABASE_URL`/`REDIS_URL`.

- **Owner CRUD**: owner (permission bypass) creates → 200 with
  `status: "inactive"`, `lastHealthCheckAt: null`, and no config key; lists
  (contains it); gets by id; patches the name; patches the config (200, still
  no config key echoed); health-check → **501** `Health check not supported for
  this connector type`; deletes → `{success: true}`; get after delete → 404
  `Resource not found`.
- **Config is encrypted at rest**: read the row directly through the returned
  `*database.Store` (`store.GetConnector`) and assert the raw
  `encrypted_config` bytes contain neither the secret value nor the plaintext
  key name, and that they parse as an object with `v`/`kid`/`dek`/`ct`.
- **Duplicate name** in the same org → 409 `Connector name already taken`; the
  same name in a *different* org → 200 (the constraint is per-org).
- **Invalid type** → 422 `Validation failed` (the request validator fires
  before the service).
- **Permission enforcement with real RBAC**: invite a second user as `member`,
  create a role granting `["connector:read"]` via `POST /rbac/roles`, assign it
  with `POST /rbac/assign`, then as that member: `GET /connectors` → 200,
  `POST /connectors` → 403 `Missing permission: connector:write`,
  `DELETE /connectors/:id` → 403 `Missing permission: connector:delete`.
  Repeat with a role granting `["connector:*"]` → read and write both 200,
  delete 200 (the wildcard covers all three verbs).
- **Member with no role** → 403 `Missing permission: connector:read` (note:
  *not* `Not a member of this organization` — `RequirePermission` checks the
  permission first).
- **Cross-org isolation**: org B's owner requesting org A's connector id (with
  `x-organization-id: <orgB>`) → 404 `Resource not found`, for GET, PATCH,
  DELETE and health-check alike.
- **Guard basics**: no `Authorization` → 401 `Unauthorized`; no
  `x-organization-id` → 400 `Missing x-organization-id header`.

## Step 12 — Documentation

### `docs/02-api-contract.md`

1. New **Connectors (`/connectors`)** section after Subscription:

   | Method/Path | Guard | Body | Behavior |
   | ----------- | ----- | ---- | -------- |
   | `POST /connectors` | perm:`connector:write` | `{ name: 1–100, type: "generic", config: object }` | Seals `config` with envelope encryption and stores it; new rows start `inactive`. Enforces `max_connectors`. 409 `CONNECTOR_NAME_TAKEN`, 422 `INVALID_CONNECTOR_TYPE`. |
   | `GET /connectors` | perm:`connector:read` | — | Org's connectors, oldest first. Never includes config. |
   | `GET /connectors/:connectorId` | perm:`connector:read` | — | One connector. 404 `NOT_FOUND` for another org's id. |
   | `PATCH /connectors/:connectorId` | perm:`connector:write` | `{ name?, status?, config? }` | Partial update; a supplied `config` is re-sealed under a fresh data key. `type` is immutable. |
   | `DELETE /connectors/:connectorId` | perm:`connector:delete` | — | `{ success: true }`. |
   | `POST /connectors/:connectorId/health-check` | perm:`connector:write` | — | Probes the upstream and records `status`/`lastHealthCheckAt`. No per-type checker exists yet → 501 `HEALTH_CHECK_UNSUPPORTED`. |

   Plus prose: the response shape
   (`{ id, organizationId, name, type, status, lastHealthCheckAt, createdAt, updatedAt }`),
   an explicit **"`config` is never returned by any endpoint"**, and a note
   that these are the first routes using the `perm:<action>` guard level (so a
   non-member gets `Missing permission: <action>`, not `Not a member of this
   organization`).
2. Service error map: add the three new codes.
3. Audit actions line: add `connector.created`, `connector.updated`,
   `connector.deleted` and adjust the "only the first four are currently
   written" clause.
4. Environment table: add `CONNECTOR_MASTER_KEY` — "base64, exactly 32 bytes;
   master key for connector envelope encryption".

### `docs/03-target-architecture.md`

Add `connector` to the module list and a short **Secrets at rest** note: data
key per row, master key from env, `KeyProvider` is the KMS/Vault seam, AAD
binds ciphertext to the org.

### `docs/05-mcp-gateway.md`

Under "What this repo needs to add" / Phase 2, note that the generic connector
skeleton (schema, envelope encryption, RBAC-gated CRUD, `Checker` interface)
has landed and Phase 2 is now the adapter work only. Keep it to two or three
sentences — this plan is not that document.

### `CLAUDE.md`

- "What the core already provides": a **Connectors** bullet.
- Ground rules: connector config is envelope-encrypted, decrypted config never
  leaves the service and never appears in a response or a log; new key
  providers implement `envelope.KeyProvider`.
- The RBAC bullet's "no route currently uses it" is now false — `/connectors`
  does.
- Environment section: `CONNECTOR_MASTER_KEY` in the required list.

### `README.md`

Quickstart env step: mention generating `CONNECTOR_MASTER_KEY` with
`openssl rand -base64 32`.

### Redaction

`internal/shared/logger/redact.go` — add `connectorconfig`, `encryptedconfig`,
`masterkey`, and `datakey` to `sensitiveKeys` (they normalize with `_`/`-`
stripped, so `master_key` and `masterKey` both match). This is a backstop, not
the primary control: the primary control is that no call site logs config at
all.

## Verification

```bash
# from apps/backend unless noted
go build ./...

# 1. sqlc regeneration is clean and committed (root)
make sqlc && git status --porcelain internal/infra/database/db

# 2. migration applies and rolls back (root; needs `make up` first)
make migrate
cd apps/backend && go run ./cmd/migrate down && go run ./cmd/migrate up   # adapt to the repo's migrate CLI flags

# 3. unit tests only (integration ones self-skip without DATABASE_URL)
go test ./...

# 4. full suite against real infra (root: make up first)
DATABASE_URL=... REDIS_URL=... go test ./...

# 5. lint (root) — CRLF line endings break golangci-lint on this box
make lint

# 6. swagger regeneration, spec committed
make swagger && git status --porcelain docs/

# 7. boot check: unset CONNECTOR_MASTER_KEY and confirm the API refuses to
#    start with "CONNECTOR_MASTER_KEY must be a base64-encoded 32-byte key",
#    aggregated with any other config problems.
```

Live smoke test (`make up`, `make migrate`, `make seed`, `make api`), as the
org owner so the permission bypass applies:

```bash
# register + create an org first (see README), then:
curl -sX POST localhost:3000/connectors \
  -H "Authorization: Bearer $TOKEN" -H "x-organization-id: $ORG" \
  -H 'Content-Type: application/json' \
  -d '{"name":"warehouse-db","type":"generic","config":{"host":"db.example.com","password":"hunter2"}}'
# expect 200, status "inactive", lastHealthCheckAt null, NO config key

psql "$DATABASE_URL" -c 'SELECT encrypted_config FROM connectors;'
# expect {"v":1,"kid":"env:...","dek":"<base64>","ct":"<base64>"} — and no "hunter2"

curl -sX POST localhost:3000/connectors/$ID/health-check \
  -H "Authorization: Bearer $TOKEN" -H "x-organization-id: $ORG"
# expect 501 {"message":"Health check not supported for this connector type"}
```

Then grep the API's log output for `hunter2` and for `password` — both must be
absent.

## Definition of done

- [x] `go build ./...` and `go test ./...` pass, both with and without infra
      (`internal/server`'s integration suite ran against real Postgres +
      Redis, ~23s not cached). `make lint` → `golangci-lint run` reports only
      pre-existing `gofmt`/CRLF churn on files this plan never touched
      (`cmd/healthcheck/main.go`, `internal/shared/httpx/{bind,response}.go`
      on one run; `internal/config/config.go`, `internal/infra/database/database.go`,
      `internal/infra/redis/auth.go` on another) — the known Windows
      `core.autocrlf` gotcha already recorded in project memory, not a defect
      in this change. No connector-module file was ever flagged.
- [x] `make sqlc` and `make swagger` produce no uncommitted diff (verified:
      regenerating both after the module was in place changed nothing beyond
      what `git status` already showed as committed).
- [x] Migration `00007` applies and rolls back cleanly (`go run ./cmd/migrate up`
      then `down` then `up` again, verified against the live `sapanjai-db`
      container, plus `\d connectors` inspected directly).
- [x] Every `/connectors` route is gated by the documented permission, verified
      against real RBAC roles in the integration test — including the
      `connector:*` wildcard and the no-role denial
      (`TestIntegration_ConnectorsPermissionEnforcement`).
- [x] A connector's config is unreadable in the database (raw-row assertion in
      `TestIntegration_ConnectorsConfigEncryptedAtRest` plus a live `psql`
      check during the smoke test), absent from every response body (asserted
      in both `handler_test.go` and the integration CRUD test), and absent
      from the logs (grepped live server stdout for the secret and host
      values during the Step 9 smoke test — none found).
- [x] Cross-org access to a connector id returns 404, on all four id-bearing
      routes (`TestIntegration_ConnectorsCrossOrgIsolation`).
- [x] The health-check route exists, returns 501, and the `Checker` interface
      is documented with the "no credentials in errors" contract
      (`health.go`'s doc comment on `Checker.Check`).
- [x] Swapping `EnvKeyProvider` for a KMS provider requires changes in exactly
      one place (`server.New`) — confirmed by reading: every other caller
      depends on the `envelope.KeyProvider`/`sealer` interfaces, never the
      concrete type.
- [x] `CONNECTOR_MASTER_KEY` is present in `.env.example`, `.env.docker`,
      `.env` (local, gitignored), `k8s/secret.example.yaml`, `k8s/README.md`
      and CI, and its absence fails boot with a clear message aggregated
      alongside every other missing required var (verified live via
      `cmd/migrate`, which shares `config.Load`).
- [x] `docs/02`, `docs/03`, `docs/05`, `README.md` and `CLAUDE.md` updated;
      the stale "no route uses RequirePermission" claims are gone from both
      `CLAUDE.md` and `middleware/auth.go`.

## Commit plan

Small, buildable commits — each one compiles and passes tests. This is what
was planned going in; a repo-configured hook auto-committed after most edit
bursts, so the actual history is more granular (10 commits, `ffe3a4e`..`65746db`)
than the 6 curated here — every one of them still builds and passes tests
individually, so the finer grain cost nothing.

1. `feat(db): add connectors table and queries` — migration 00007, `connectors.sql`, regenerated sqlc.
2. `feat(crypto): add envelope encryption with swappable key provider` — `internal/shared/envelope/` + its tests.
3. `feat(config): require CONNECTOR_MASTER_KEY` — config + `.env*` + compose/k8s/CI.
4. `feat(connector): add org-scoped connector module` — the module, apperror codes, audit actions, validator, server wiring, `server.New` signature.
5. `test(connector): cover encryption round-trip and permission enforcement` — service, handler and integration tests.
6. `docs: document the connector module` — `docs/02`, `docs/03`, `docs/05`, README, CLAUDE.md, regenerated swagger.
