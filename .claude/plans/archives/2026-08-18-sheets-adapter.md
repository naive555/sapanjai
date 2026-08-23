# Google Sheets/Drive MCP Adapter — execution tracker

> **Status: ✅ complete (planned 2026-08-18, finished 2026-08-22).** 12 / 12 steps
> shipped. The full read-only `google_sheets`/Drive MCP adapter is live end to
> end: PAT auth (`internal/module/mcpkey`), the gateway endpoint
> (`internal/module/mcp`, `POST /mcp/:connectorId`), the RBAC-filtered tool
> catalog, the Redis rate limiter, the `google_sheets` connector type +
> allowlist + health checker, all six read tools (`sheets_list_spreadsheets`,
> `sheets_describe_spreadsheet`, `sheets_query_rows`, `sheets_read_range`,
> `drive_list_folder`, `drive_get_file`) wired through RBAC/allowlist/audit
> the same way, and two dashboard pages (`/mcp-keys`, `/connectors` +
> `/connectors/:id/google-sheets`) so onboarding needs no curl. This file is
> now archived per its own instruction below — see
> [`docs/05-mcp-gateway.md`](../../../docs/05-mcp-gateway.md),
> [`docs/06-sheets-adapter.md`](../../../docs/06-sheets-adapter.md), and
> [`docs/07-sheets-adapter-decisions.md`](../../../docs/07-sheets-adapter-decisions.md)
> for this feature's current, maintained state — those are the docs to update
> if it changes again, not this file.
> **All four decisions confirmed by the owner 2026-08-18** — none were reopened.
> **Per-step detail:** §"Step detail" at the foot of this file — moved here from
> `docs/07` §2 on 2026-08-23 when that file was re-cut as
> [`docs/07-sheets-adapter-decisions.md`](../../../docs/07-sheets-adapter-decisions.md).
> **Spec:** [`docs/06-sheets-adapter.md`](../../../docs/06-sheets-adapter.md) · **Architecture:** [`docs/05-mcp-gateway.md`](../../../docs/05-mcp-gateway.md)
>
> This file was the checklist an executor worked from; since 2026-08-23 it also
> holds the goals, file lists, verification commands, and ground-rule callouts
> for each step (§"Step detail"), and `docs/07` holds only the decisions and
> scope that outlive the build. Archived here per this file's own instruction now that step 12 has
> landed — see `.claude/plans/archives/` for the other completed plans.
>
> Target executor: **Sonnet throughout.** Steps 3, 5 and 7 were tightened on
> 2026-08-18 specifically to remove the places an executor would have had to invent:
> step 3 now names the spike files to port and the request-context trap, step 5 fixes
> the Google client choice and the mock seam, step 7 resolves the `total` contradiction
> and specifies the scan loop. If any of those three still feels underdetermined when
> you reach it, that is a plan bug — say so rather than guessing.

---

## Decisions — ✅ all confirmed 2026-08-18

Confirmed by the owner; no longer blocking. Full reasoning in `docs/07` §1.

| # | Decision | Resolution | Gates |
| - | -------- | ---------- | ----- |
| 1 | MCP client auth | **Confirm PAT** (spec option A). Table `mcp_api_keys`, SHA-256 not bcrypt, `sk_live_<32B>`, `scopes` intersected with live RBAC. | Step 2 |
| 2 | OAuth onboarding | **Manual credential paste** for MVP. No dashboard consent flow — sensitive-scope verification is weeks of calendar time outside our control. | Steps 5, 11 |
| 3 | Permission matcher | **Extract `ActionMatches` + add `rbac.Principal`.** Note: the matcher is *already* in the service, not the middleware — the real gap is a bulk path for catalog filtering. | Step 1 |
| 4 | `sheets_query_rows` `total` | **Drop exact `total`; bounded scan with `scanned_rows` + `scan_complete`.** Surfaced while tightening: spec §4.2's `total` and §6's memory rule cannot both hold. Deviation from the spec — step 12 writes it back into `docs/06`. | Step 7 |

---

## Steps

- [x] **1. Extract `ActionMatches` + `rbac.Principal`** — ✅ done 2026-08-18, reviewed. Pure refactor confirmed: `service_test.go` +121/−0, `internal/middleware/` untouched, non-member still `(false, nil)`, owner still skips the permissions query. Committed as `f9c6e64`. Re-verified afterwards under the real golangci-lint v2 with the integration suite actually running — clean.
- [x] **2. `mcp_api_keys` migration + PAT module** — ✅ done 2026-08-18, reviewed. Migration `00008` additive; SHA-256 hashing verified against the DB (0/63 rows hold a raw token, all 63 are 64-char hex); list response proven not to leak `apiKey`; 6 integration tests ran with 0 skips; lint clean. Uncommitted. Note: `scopes` column ships unreachable by design — step 3 adds the write path and the RBAC intersection.
- [x] **3. `RequireMCPKey` + `POST /mcp/:connectorId` + one trivial tool** — ✅ done 2026-08-18, reviewed. ⚠️ **risk retired: the SDK mounts cleanly in Echo and the whole auth path works end to end.** Verified live by driving JSON-RPC against a running server: `initialize` → `tools/list` → `tools/call` all succeed; a connector config containing a planted secret appears in neither the tool response nor the API log; revoke flips the next call to 401; audit rows land clean. 9 integration tests, 0 skips. Uncommitted.
  - Owner+scopes resolved correctly: `Principal.Narrow` clears `Role` so an owner's bypass cannot survive scoping.
  - **Discovery — SDK v1.7.0 clients send `server/discover`, not `initialize`** (protocol ≥2026-07-28, SEP-2575). Session-start audit fires on both.
  - **Discovery — import cycle:** `internal/middleware` cannot import `internal/module/rbac` (every handler already imports `middleware`). Worked around with a resolver closure injected from `server.go`, typed `func(...) (any, error)` + a type assertion. Contained and documented; revisit if a third caller appears.
  - **Known wart (not live, fails closed):** a key scope *broader* than the user's grant collapses to nothing — scope `connector:*` held by a user with only `connector:read` yields zero permissions rather than `connector:read`. Nothing writes `scopes` yet; **fix when step 10's mint-with-scopes UI lands**, or a picker offering "all connector actions" will mint dead keys.
- [x] **4. Redis token-bucket rate limiter** — ✅ done 2026-08-18, reviewed. `internal/infra/redis/ratelimit.go`: one Lua `EVAL` script (`tokenBucketScript`) does refill + take atomically, keyed on Redis's own `TIME` (not a client timestamp, so replicas can't skew it), key `mcp:ratelimit:<connectorId>`, idle TTL 2 min. `RateLimiter.Take(ctx, connectorID, n)` charges n (floor 1 in `NewRateLimiter` too, for a misconfigured `perMinute<=0`). Wired into `mcp.Service.enforce`'s `tools/call` case, after the permission check and before dispatch — floor of 1 unit per call (`toolCallCost`) since no adapter makes upstream calls yet; `Service.ChargeRateLimit(ctx, connectorID, n)` is the exported seam step 7's paged scan will call directly, mid-execution, to charge N per page. `mcp/errors.go` gained `RateLimited(retryAfter)` (byte-stated retry-after in whole seconds); `apperror.RateLimited` = 429 exists for a future REST caller but no REST route emits it — the gateway's own denial is a `CallToolResult{IsError:true}`, never an HTTP status. `auditlog.ActionMCPRateLimitHit` = `mcp.ratelimit.hit`, written best-effort on every denial. Config: `MCP_RATE_LIMIT_PER_MIN` (default 60), validated in `config.Load`; `setupTestServer` gained an optional `configure ...func(*config.Config)` hook (backward compatible — 35 existing call sites untouched) so the new integration test can lower the limit to 2 instead of issuing 61 real calls. 6 new unit tests in `internal/infra/redis/ratelimit_test.go` against real Redis (allow-under-budget, deny-at-limit, n-charges-n-in-one-call, refill-over-time, retry-after-is-sane, n<1 treated as 1); 1 new integration test (`TestIntegration_MCP_RateLimitTripsAndAudits`) exhausts a 2-token bucket, asserts the 3rd call is `IsError` with `RATE_LIMITED` + a stated retry-after, and exactly one `mcp.ratelimit.hit` audit row. Full suite: 0 skips, `make lint` still exactly the one pre-existing `cmd/seed/main.go` gofmt issue. No `make swagger` needed — no new REST route.
  - **Design call not fully specified by the plan:** on a genuine Redis/script error (not "bucket empty"), `enforce` fails closed with `ErrorResult(err)` (a readable `IsError`, "Internal error") rather than either failing open (admitting an unmetered call) or returning a bare Go error (which the SDK turns into a JSON-RPC protocol error and aborts the agent's turn). This mirrors the codebase's existing fail-closed convention for Redis infra errors (`middleware.Guards.verify`'s blacklist check, `auth.Service.Login`'s login-attempt limiter both propagate rather than swallow) while still honoring step 4's "never a panic or a 500" instruction for the actual exhaustion case.
- [x] **5. `google_sheets` connector type** — ✅ done 2026-08-18, reviewed. New `internal/adapter/googlesheets/` (`config.go`, `oauth.go`, `api.go`, `client.go`, `checker.go` + one test file each). `TypeGoogleSheets = "google_sheets"` added to `connector.validTypes`; `connector.NewRegistry(googlesheets.NewChecker())` wired in `server.go`. `go.mod` gained `google.golang.org/api` (sheets/v4, drive/v3, option) and promoted `golang.org/x/oauth2`(`/google`) to direct — `go mod tidy` run. `make test`: 0 skips with real Postgres/Redis (`internal/server` 19.5s, exercising the new `TestIntegration_ConnectorsGoogleSheetsTypeAccepted`); `make lint` via real `golangci-lint` (not just `go vet` — the binary was present but not on `PATH`): exactly the one pre-existing `cmd/seed/main.go` gofmt issue, nothing new. `make swagger` run — no diff, since no route/DTO shape changed.
  - **Decision not specified by the plan — connector.Checker has no connector id.** The plan's design point says "keep one TokenSource per connector id in a mutex-guarded map" and `oauth.go` builds exactly that (`TokenSourceCache`), but `connector.Checker.Check(ctx, config map[string]any)` — the interface step 5 must implement — receives only the decrypted config, never an id. Resolution: `TokenSourceCache` is built as the seam step 6+ is expected to use (its MCP tool handlers do have a connector id in scope for a whole session); `checker.go`'s health-check path builds a fresh, uncached `TokenSource` per call via `NewTokenSource` directly, since a health check is an infrequent, single-request operation where cache reuse buys nothing. Documented in both files' doc comments.
  - **Decision not specified by the plan — allowlist non-emptiness.** `ParseConfig` rejects a `scope` naming neither a spreadsheet nor a folder (`googlesheets: config.scope must allowlist at least one spreadsheet or folder`). Not stated in the spec or plan, but required for `checker.Check`'s probe to have anything to read, and a config that allowlists nothing can never do anything useful — treating it as a validation error (rather than an inert connector) seemed more honest than silently accepting configs that can never pass a health check. `POST /connectors` still does not shape-validate `config` against the type (matches the existing skeleton's behavior for every type) — an invalid `google_sheets` config only surfaces at health-check time, same as any other type-specific config error would.
  - **Decision not specified by the plan — what the health check probes.** With four fixed `sheetsAPI` methods and no fifth "just check auth" call, `Checker.Check` reads metadata for the first allowlisted spreadsheet, falling back to listing the first allowlisted folder's files when no spreadsheet is allowlisted (allowlist non-emptiness above guarantees one of the two always applies).
  - **Bug found in review, fixed — every Google call was going out unauthenticated.** `newClient` originally passed `option.WithTokenSource(ts)` alongside `option.WithHTTPClient(&http.Client{Timeout: ...})`. `google.golang.org/api`'s transport returns a `WithHTTPClient` client *verbatim* and never applies `WithTokenSource` to it (`transport/http/dial.go`: `if settings.HTTPClient != nil { return settings.HTTPClient, endpoint, nil }`), so the bearer token was silently dropped and every upstream request would have 401'd in production. The contract test passed anyway because it only asserted response mapping, never the `Authorization` header. Fix: build the client as `oauth2.NewClient(ctx, ts)` and set `.Timeout` on that same client, so one client carries both the token and the timeout. `client_test.go` now asserts `Authorization: Bearer ...` reaches the httptest server — verified failing against the old wiring (header arrived empty) and passing against the new.
  - **Local environment note, not a code issue:** this machine's `sapanjai-db` container had a `POSTGRES_PASSWORD` value shaped like an md5 hash (`md5f1d5f...`), which Postgres interprets as a pre-hashed password incompatible with the container's `scram-sha-256` auth method, and root `.env`'s `DATABASE_URL` didn't match `DATABASE_USER`/`DATABASE_PASSWORD` either. Fixed locally via `ALTER ROLE postgres WITH PASSWORD 'password'` and correcting `DATABASE_URL` in the (git-ignored) `.env`, so `make test` could actually exercise Postgres/Redis instead of silently skipping every integration test. Nothing in the repo changed.
- [x] **6. `sheets_list_spreadsheets` + `sheets_describe_spreadsheet`** — ✅ done 2026-08-20, reviewed. New `internal/adapter/googlesheets/{list.go,describe.go}` (+ tests) implement both tools' Google-facing logic against the `sheetsAPI` seam step 5 built, never the network; new `internal/module/mcp/tools_sheets.go` registers them with permission `sheets:read` and `ReadOnlyHint: true`. `make test`: 0 skips across the whole suite (292 tests), `internal/adapter/googlesheets` and `internal/module/mcp` both green, `internal/server` 7.3s including 6 new/updated MCP integration tests; `make lint` via real `golangci-lint`: exactly the one pre-existing `cmd/seed/main.go` gofmt issue. `make swagger` run — no diff, since no REST route/DTO shape changed (this step is MCP-only).
  - **Decrypted-config gap, resolved as the plan flagged it might need to be — more files touched than the plan's step 6 list.** `Entry.Register`'s signature widened to `func(s *gomcp.Server, svc *Service, conn db.Connector)` (was `func(s *gomcp.Server, conn db.Connector)`) so a tool handler can call back into its owning `*Service`. `connector.Service` gained an exported `OpenConfig(ctx, organizationID, encryptedConfig)` wrapping the existing private `openConfig` (`internal/module/connector/service.go` + 3 new unit tests, one proving the AAD organization-scoping survives the exported path). `mcp.connectorGetter` widened to require `OpenConfig` too. `Service.openGoogleSheetsConfig(ctx, conn)` (new, `internal/module/mcp/service.go`) decrypts+parses fresh **on every call** — never once at `BuildServer`/session-start — so a narrowed allowlist takes effect on the very next `tools/call`, per step 5's design point. `Service` also gained an unexported `sheetsTokens *googlesheets.TokenSourceCache`, built inside `NewService` itself (constructor signature and all 7 existing call sites unchanged) — this is the *token* cache (an OAuth access token is a live credential, correctly cached in-process per step 5), kept strictly separate from the allowlist re-check so caching one never stales the other.
  - **Bug found in review, fixed — a rotated OAuth credential was ignored until process restart.** Step 6 is the first step to actually *use* `googlesheets.TokenSourceCache`, and it wired only `Get`, never `Delete`. `Get` keyed purely on connector id and returned the cached source "regardless of the oauthCfg passed" (step 5's own doc comment said callers must `Delete` on rotation), `mcp.Service` is built once in `server.New` so its cache lives for the whole process, and nothing anywhere called `Delete`. Net effect: a customer whose refresh token leaked could rotate it through `PATCH /connectors/:id` and the gateway would keep minting access tokens from the retired credential indefinitely. The allowlist was unaffected (config is genuinely re-decrypted per call, as designed) — this was specific to the cached credential. Fix: `TokenSourceCache` entries are now keyed by connector id **plus a SHA-256 fingerprint of the OAuth config**, so a changed credential no longer matches and the source is rebuilt on the next call; the digest is one-way and never logged, returned, or persisted. Manual `Delete` was rejected as the fix because the rotating caller (a REST handler) and the reading caller (an MCP tool handler, another module, another request) are different code paths — a missed `Delete` fails silently and in the unsafe direction. Regression tests `TestTokenSourceCache_RotatedCredentialRebuilds` / `TestTokenSourceCache_RotatedClientSecretRebuilds` in `oauth_test.go`, both verified failing against the old keying. Note this makes step 5's `Delete` method unused by production code; left in place as it is still the right tool for explicit eviction.
  - **Decision not specified by the plan — connector-type gating.** `Entry` gained a `ConnectorType connector.Type` field (empty = every type) and `BuildServer` now filters on `Entry.appliesTo(conn)` before the RBAC check. Without this, `sheets_list_spreadsheets`/`sheets_describe_spreadsheet` would register against *any* connector (a `generic` one included) for a caller holding `sheets:read` — nonsensical, since the handler would try to parse an unrelated connector's config as Google Sheets and always fail. This kept every pre-existing test (including `TestIntegration_MCP_HappyPath`'s `tools/list == [sapanjai_describe_connector]` for a `generic` connector) green with zero changes, which is why it's judged the correct resolution rather than a scope overreach.
  - **Decision not specified by the plan — check ordering inside `sheets_describe_spreadsheet`.** `include_sample_rows` bounds (0-5) and `spreadsheet_id` presence are checked *before* config decryption or the allowlist; the allowlist check itself runs before any upstream call. This lets both required tests (bound rejection, `SPREADSHEET_NOT_ALLOWED`) run as real integration tests against a connector with a fake-but-plausible OAuth refresh token, with a hard guarantee neither code path ever reaches the actual Google token endpoint — verified by construction (`DescribeSpreadsheet`'s allowlist check runs before `newClient` is ever called; unit-tested by passing a `nil` `oauth2.TokenSource` and confirming no panic).
  - **Decision not specified by the plan — `sheets_list_spreadsheets` failure semantics.** One allowlisted spreadsheet id that the OAuth token can no longer reach (revoked share, deleted file) is reported as `{ spreadsheet_id, accessible: false }` rather than failing the whole call — a stale allowlist entry shouldn't hide visibility into every other spreadsheet the connector can still reach. Only a client-construction failure (a broken token source) fails the tool outright.
  - **Decision not specified by the plan — rate-limit charging for these two tools.** Both rely solely on `enforce`'s existing dispatch-time floor of 1 unit per `tools/call` (already in place since step 4); per-upstream-call charging via `Service.ChargeRateLimit` is **not** wired here. `service.go`'s own doc comment already named step 7's paged scan as `ChargeRateLimit`'s first real caller, and `sheets_describe_spreadsheet` can in principle issue one upstream call per tab (metadata + one header read per sheet, +1 more per sheet if `include_sample_rows > 0`) for a single floor-of-1 charge — a real but small gap (bounded by a spreadsheet's tab count, not its row count, so it doesn't reopen the 87GB concern step 7 exists for). Flagged here rather than silently left; worth a one-line fix (charge `len(sheets)` or `len(sheets)*2` via `ChargeRateLimit` before the per-tab loop) if it proves to matter before step 7 lands.
  - **Plan-doc inconsistency noticed, not acted on.** `docs/07-sheets-adapter-decisions.md`'s own step 6 file list says "modify `docs/07-sheets-adapter-decisions.md` — tick the step in the tracker," but the tracker is *this* file, `.claude/plans/2026-08-18-sheets-adapter.md` — every prior step's own completion note lives here, not in `docs/07`. Ticked here per the established pattern and the executor's explicit instructions; `docs/07-sheets-adapter-decisions.md` itself was left unmodified.
  - **Not implemented, out of step 6's required scope.** `docs/06-sheets-adapter.md` §8's other error codes (`SHEET_NOT_FOUND`, `UPSTREAM_AUTH_FAILED`, etc.) have no dedicated detection yet — any adapter-level error other than the allowlist miss surfaces through `mcp.ErrorResult`'s generic "Internal error" fallback. Fine for now (no tool in this step can raise `SHEET_NOT_FOUND`, since `describe` enumerates every tab rather than naming one), but `UPSTREAM_AUTH_FAILED` specifically (an expired/revoked refresh token) will need real detection once these tools run against production credentials — left for a later step since nothing in step 6 depends on it.
- [x] **7. `sheets_query_rows`** — ✅ done 2026-08-20, reviewed. New `internal/adapter/googlesheets/{filter.go,filter_test.go,query.go,query_test.go}` implement the DSL (`eq/neq/contains/gt/lt/gte/lte/in`, AND-ed) and the bounded scan loop entirely against the `sheetsAPI` seam (never the network); `internal/module/mcp/tools_sheets.go` registers `sheets_query_rows` (gated `sheets:read` + `connector.TypeGoogleSheets`, same as steps 6's two tools) and `internal/module/mcp/errors.go` gained `SheetNotFound`/`ColumnNotFound`/`ResultTooLarge`/`invalidQueryRowsInput`/`invalidResponseFormat`/`missingSheetName`. `make test`: 0 skips across the whole suite; `internal/adapter/googlesheets` 22 tests including the plan's full required list, `internal/module/mcp` and `internal/server` (49 MCP tests, 0 skips/0 fails) all green. `go test ./internal/adapter/googlesheets/ -run 'TestFilter|TestQuery' -count=1 -race`: all 22 pass clean under the race detector. `make lint` via real `golangci-lint` (`/Users/nonny/go/bin/golangci-lint`, not on `PATH`): exactly the one pre-existing `cmd/seed/main.go` gofmt issue (two `staticcheck` QF1012 hits in the new markdown renderer were fixed — `fmt.Fprintf(&b, ...)` instead of `b.WriteString(fmt.Sprintf(...))`). `make swagger` (via `/Users/nonny/go/bin/swag`, also not on `PATH`) produced no diff — step 7 is MCP-only, no REST route or DTO shape changed.
  - **Decision 4 built exactly as specified.** `QueryRowsOutput` carries `count/offset/has_more/next_offset/scanned_rows/scan_complete` unconditionally and `total *int` only when `scan_complete` is true (`omitempty` on the wire, so the field is absent, not `null`, when false) — verified by a dedicated test (`TestQueryRows_ScanBudgetExceeded_NoTotal`) that a mocked sheet larger than the scan budget returns `scan_complete: false` and a nil `Total`. Both the tool description (`sheetsQueryRowsDescription`) and the result text (`queryRowsPartialScanWarning`, appended as a second `TextContent` block so a `response_format: json` caller's first block is still pure parseable JSON) spell out that a partial scan's count is a lower bound and name the fix (narrow the filter, or page with `next_offset`).
  - **Scan loop matches the plan's four steps verbatim**, with one addition needed for correctness: a labeled `pageLoop`/priority order — "enough matches" (the `offset+limit+1` retention cap) is checked *first* and stops immediately, even mid-page, so `scan_complete` is never set on that path (we deliberately don't know whether that page also happened to be the sheet's last); "end of sheet" (a short page) is checked next, ahead of the scan budget, so a sheet whose true size ties the budget still reports `scan_complete: true`; the rate-limit charge happens once per page, before that page's `Values.Get`, so an exhausted bucket never triggers a wasted fetch. `ChargeRateLimit` is reached via a narrow `RateCharger` interface declared in `query.go` (mirrors `connectorGetter`'s/`rateLimiter`'s house style) that `*mcp.Service` satisfies as-is — `tools_sheets.go` passes `svc` directly as the charger, no adapter code imports `internal/module/mcp`.
  - **Memory bound: actually measured, not asserted by reading the code.** `query.go` has a test-only `retainedPeakHook func(n int)` (nil in production) invoked every time a row is appended to the retained buffer; `TestQueryRows_ManyMatches_RetentionBounded` drives a scan against a generator whose available matches vastly exceed `offset+limit+1` (the generator never terminates on its own within the test's page count) and asserts the *observed* peak equals `offset+limit+1` exactly, not merely "did not panic" or "returned quickly." A second test, `TestQueryRows_ScanBudgetExceeded_NoTotal`, separately proves the budget-exhaustion path (retention cap set far above the budget so budget, not the cap, is what stops the scan) returns `scan_complete: false` with no `Total`. `TestQueryRows_RateLimitExhaustedMidScan_EndsCleanly` uses a `fakeCharger` that denies after N charges and asserts (a) no error, (b) exactly N pages were actually fetched (a denied charge attempt never reaches `Values.Get`), and (c) `ScannedRows` reflects only the fetched pages.
  - **Formula-injection literal: proven at two levels.** `filter_test.go`'s `TestFilter_FormulaValueIsALiteral` proves a single `Filter.Matches` call treats `=IMPORTRANGE(...)` as an exact-string literal (matches only byte-identical cell text, not a "starts with =" heuristic). `query_test.go`'s `TestQueryRows_FilterValueNeverReachesUpstream` proves it at the scan-loop level: a formula-shaped filter value correctly matches the one planted cell, and every `a1Range` string the mocked `sheetsAPI.Values` was ever called with is asserted to never contain the literal "IMPORTRANGE" substring — the filter value never reaches anything that talks to Google, by construction (the scan loop only ever builds A1 row ranges from the sheet name and row numbers; it has no code path that builds a query string from a filter value at all).
  - **Bug found in review, fixed — the post-dispatch audit was lost whenever the client hung up.** Moving `mcp.tool.called` after dispatch (the bullet below) was the right call for `row_count`/`duration_ms`, but it put the write on the far side of a scan that can run for seconds across several page fetches, and `auditlog.Record` writes synchronously on the request context. A client that disconnected or timed out mid-scan cancelled that context, `CreateAuditLog` failed with `context.Canceled`, and the row silently never landed — best-effort auditing swallowing the error by design. Net effect: the audit trail went missing for exactly the abandoned and timed-out calls that most warrant one, on the most data-intensive tool in the catalog. Before-dispatch auditing never had this exposure because it wrote before the slow part. Confirmed against the real database prior to fixing (a `Record` under a pre-cancelled context wrote **0 rows**). Fix: `enforce` captures `auditCtx := context.WithoutCancel(ctx)` and the deferred write uses that — values preserved, only cancellation detached. Regression test `TestIntegration_MCP_ToolCallAuditSurvivesClientDisconnect` in `mcp_integration_test.go`.
  - **Review note — two upstream calls per query sit outside the rate-limit bucket.** `queryRows` charges one unit per *page* as the plan specifies, but `SpreadsheetMeta` and the header-row `Values` read happen before the page loop and are never charged. Same undercharge class as step 6's `sheets_describe_spreadsheet` floor-of-1, and small (a fixed 2 per call, not proportional to sheet size), but it means a caller looping cheap zero-match queries can issue ~3x the upstream calls the bucket thinks it authorized. Not fixed here to keep step 7 scoped; worth folding into whichever step revisits the describe undercharge.
  - **The audit-timing gap, resolved: move to post-dispatch, not a second row.** `Service.enforce`'s `tools/call` case now records `mcp.tool.called` via `defer`, after `next(ctx, method, req)` returns, instead of before dispatch (steps 3–6's design). This was chosen over "emit a second row" because the spec's table (§7) wants one row per call carrying every field together, and a second row would either duplicate `connector_id`/`tool` or force a reader to join two rows to answer "how long did this call take and how many rows did it return" — worse ergonomics for the exact audit trail this exists to support. Cost: the enclosing closure's signature became named returns (`result gomcp.Result, err error`) so the deferred call can read what `next` produced; the defer still runs during panic unwind (ahead of Echo's `Recover` middleware in `server.go`, which sits above this call stack), so a handler bug still leaves an audit row — the "attempted" guarantee steps 3–6 relied on is preserved, arguably strengthened (it now also captures a real duration up to the panic). `duration_ms` is `time.Since(start).Milliseconds()`, always present now. `row_count` is recovered from the *already-serialized* `StructuredContent` the SDK attaches to the result — `rowCountFromResult` peeks only a top-level `"count"` key (present on `sheets_query_rows`' own output shape, absent on every other tool's, so `row_count` is simply omitted for `sheets_list_spreadsheets`/`sheets_describe_spreadsheet`/`sapanjai_describe_connector` rather than reporting a misleading 0) — it never touches `Content` (the human/agent-facing text) or any other field, so there is no path from this peek to a cell value. `filter_columns` stays exactly what it was designed to be pre-dispatch: `auditableToolFields` now also parses `arguments.filters[].column` (never `.value`) into a `[]string`, extracted before `next` runs, same as `spreadsheet_id`/`sheet_name` always were. `recordAudit`'s metadata type widened from `map[string]string` to `map[string]any` (only this file; `auditlog.Service.Record` itself is untouched) so `duration_ms`/`row_count` can be ints and `filter_columns` a string slice rather than everything being pre-stringified — verified end to end by `TestIntegration_MCP_QueryRows_SpreadsheetNotAllowed`'s audit sub-test, which also asserts the secret filter value used in the test (`"หจก. ก่อสร้าง จำกัด"`) never appears anywhere in the raw audit-log JSON.
  - **Result-size cap.** `checkResultSize` marshals only the rows actually being returned (post `offset`/`limit` windowing, since the retained scan buffer is already far smaller than 256KB by construction) and returns `ErrResultTooLarge` — not a silent truncation — mapped to the `RESULT_TOO_LARGE` `CallToolResult` naming both fixes §8 calls for (`columns` projection, or a narrower filter/limit). Not in the plan's explicit required-test list, but covered anyway (`TestQueryRows_ResultTooLarge`) since it's one of the "other fixed requirements."
  - **Optional floor-of-1 fix on `sheets_describe_spreadsheet`, left alone.** The plan flagged this as a plausible one-liner now that `ChargeRateLimit` has a real caller, but explicitly gated doing it on "genuinely trivial and destabilizes nothing." Left as-is: fixing it means deciding *when* to charge (before vs. interleaved with the per-tab loop, and whether `include_sample_rows` adds a second charge per tab), which is a real design point step 6's own note already flagged rather than a one-line change, and touching step 6's tool risked destabilizing its own green tests for no requirement of this step.
  - **Underspecified in the plan, decided here.** (1) *Response body format for `response_format: json`* — built explicitly as a `CallToolResult` with the marshaled DTO as one `TextContent` block (not left to the SDK's default auto-fill), so behavior is deterministic regardless of internal SDK fallback rules; `StructuredContent` is still populated by the SDK from the same typed return value either way, which is what `rowCountFromResult` reads. (2) *Where `limit`/`offset`/filter-shape validation lives* — both at the MCP layer (`tools_sheets.go`, before config decryption, matching `sheets_describe_spreadsheet`'s existing house style) and inside the adapter (`googlesheets.ValidateQueryRowsInput`, directly unit-testable without a mock `sheetsAPI` at all) — deliberate duplication rather than picking one, since the plan's own verify command (`go test ./internal/adapter/googlesheets/ -run 'TestFilter|TestQuery'`) implies `limit=201` is tested at the adapter level. (3) *Numeric vs. string comparison* — `gt/lt/gte/lte/eq/neq` try a numeric comparison first (when both the cell and the filter value parse as a number) and fall back to lexicographic string comparison otherwise; not specified by the spec, but needed for `gt`/`lt` to be useful on amount/date-like columns at all.
  - **Not implemented, out of step 7's scope.** `UPSTREAM_AUTH_FAILED` and any other §8 error code besides the three this step adds still fall through to `ErrorResult`'s generic "Internal error" — unchanged from step 6's note, still deferred to a step that runs against real credentials. `sheets_read_range` (step 8) and the Drive tools (step 9) are untouched.
- [x] **8. `sheets_read_range`** — ✅ done, committed `c06996a`. New `internal/adapter/googlesheets/{readrange.go,readrange_test.go}` implement the A1-range escape hatch: the range is parsed and re-validated against the allowlist in our own code (never handed to Google opaquely), rejecting anything without explicit numeric row bounds on both ends (a bare sheet name or a column-only range like `A:D`), normalizing reversed bounds (`D10:A1`), and rejecting a column reference past `ZZZ` or a span over 1,000 rows/20,000 cells before any network call. `checkResultSize` was generalized (extracted `checkEncodedSize`) so both this tool and `sheets_query_rows` share the same 256KB `RESULT_TOO_LARGE` cap. `internal/module/mcp/tools_sheets.go` registers `sheets_read_range` (`sheets:read` + `connector.TypeGoogleSheets`); `docs/02-api-contract.md` updated with the tool's input/output shape.
- [x] **9. `drive_list_folder` + `drive_get_file`** — ✅ done, committed `c4992e7`. New `internal/adapter/googlesheets/drive.go` (+ test) and `internal/module/mcp/tools_drive.go` implement the Drive half, gated `drive:read` — distinct from `sheets:read`, neither grants the other. `drive_get_file` returns a signed, short-lived download URL (TTL ≤ 15 min per §4.3) via `internal/module/mcp/filelink.go`: an HMAC-SHA256 signature over `(org, connector, user, file, exp)`, keyed by a key derived from `CONNECTOR_MASTER_KEY` via HKDF-SHA256 (deliberately not the master key itself, and deliberately not a new secret — see `docs/05`/`CLAUDE.md`). Served from a new unauthenticated route, `GET /mcp/files/:connectorId/:fileId` (the signature *is* the credential there), added to `internal/module/mcp/handler.go`. New audit action `mcp.file.downloaded` (`internal/module/auditlog/service.go`). `docs/02-api-contract.md` updated with both tools and the download route.
- [x] **10. Frontend: MCP key management** — ✅ done, committed `5673b64`. `apps/frontend/app/(dashboard)/mcp-keys/page.tsx` + `lib/api/endpoints.ts` additions: create (name + optional `expiresInDays`), list (name/scopes/status/last-used/expires/created), and revoke. The raw key is shown exactly once in the create dialog with a copy affordance and an explicit "cannot be recovered" warning; tests assert it is never written to `localStorage`/`sessionStorage`. Nav entry added to `app/(dashboard)/layout.tsx`. **Known gap carried forward, not fixed here:** there is no scope picker — every key mints with `scopes: null` (the creator's full live grant, intersected at call time), which is the "known wart" step 3 flagged ("fix when step 10's mint-with-scopes UI lands"). Left as `docs/07` §3 out-of-scope-adjacent; a scope picker would need its own step.
- [x] **11. Frontend: `google_sheets` connector setup** — ✅ done, committed `f66bf66`. Scope grew beyond the plan's own file list, **with the owner's approval**: there was no connectors list page at all before this step (`GET /connectors` had a REST API but zero frontend surface), so `apps/frontend/app/(dashboard)/connectors/page.tsx` (create/list/health-check/delete, 428 lines) had to be built first, alongside the plan's originally-scoped `components/connectors/google-sheets-form.tsx` and `app/(dashboard)/connectors/[id]/google-sheets/page.tsx` (the dedicated paste-path credentials + allowlist + `header_rows` form). The config fields (`clientId`/`clientSecret`/`refreshToken`/`spreadsheetIdsText`/`driveFolderIdsText`/`headerRowsText`) are write-only in the UI, matching the write-only API — no stored secret is ever rendered back. `lib/api/endpoints.ts` gained the connector CRUD + health-check calls. Nav entry added alongside step 10's.
- [x] **12. Docs consolidation** — ✅ done. `docs/05-mcp-gateway.md`'s Phase 1/2/3 status rewritten to match what actually shipped (Phase 1 done, Phase 2 done read-only for one connector, Phase 3 done in part — no per-key tool scoping, no plan limits). `docs/06-sheets-adapter.md` §10's four questions marked resolved with pointers into the code, and §4.2's example output corrected to the bounded-scan shape (no unconditional `total`). `CLAUDE.md` gained MCP gateway/MCP keys bullets, the `google_sheets` connector-type update, `internal/adapter/` in the docs list, `mcp`/`mcpkey` in the module-convention rule, and an MCP testing-expectations bullet — no new env vars or Redis keys were needed (`internal/config` and `internal/module/mcp/filelink.go` checked directly: the file-link signing key derives from the existing `CONNECTOR_MASTER_KEY` via HKDF, not a new secret). `README.md` gained a full "MCP client setup" walkthrough (org → mint a key → configure the connector → health check → point a client at `POST /mcp/:connectorId`, with a concrete `claude mcp add --transport http` example) plus API-table, frontend, and documentation-table updates. This file archived to `.claude/plans/archives/`, per its own instruction.

**Every step must end green:** repo builds, `make test` and `make lint` pass, nothing
half-wired. A step that leaves the build broken until the next one is wrong — split it.

**Stop between steps.** The owner is asked before each step starts — confirming the
plan was not standing authorization to run it end to end. Finish a step, have the work
reviewed, report it, then wait. Never queue several steps in one go.

---

## Standing constraints (do not violate)

- Migrations additive-forward only. `make sqlc` is an explicit action inside the step, never a side effect.
- handler → service → sqlc queries. Services return `apperror` codes and never import `net/http`.
- Decrypted connector config never leaves the owning service — no DTO field, no log line, no audit metadata.
- Audit writes best-effort: log the failure, never fail the request.
- Log individual fields, never whole request/body structs. New sensitive keys go in `internal/shared/logger/redact.go`.
- Audit `filter_columns[]` but **never** filter `value`s — those are real business data.
- New routes go in `docs/02-api-contract.md` + swaggo annotations + `make swagger`, in the same step.
- No CORS middleware. MCP clients are not browsers; the browser only calls same-origin `/api/*`.
- Nothing may depend on `spikes/mcp-gateway/` or promote it into `apps/backend`. It is evidence attached to `docs/05`.

## Out of scope

Write tools (`sheets_append_row`, `sheets_update_cells`, `drive_upload_file`), the LINE
adapter, OAuth 2.1 authorization server, the Google consent flow, `max_mcp_calls_per_month`
plan limits, multi-tab join/workflow tools, Redis caching of PAT lookups, and the
unrelated `connector.Service` rotate-on-read wiring. Rationale and triggers in `docs/07` §3.

## Open risks

Six, with the earliest signal for each, in `docs/07` §4. The two worth knowing before
step 1: **the MCP SDK has never run inside Echo** (retired at step 3, which is why it
comes before any Google code), and **87 GB of spreadsheet may defeat the Sheets API**
regardless of how `sheets_query_rows` is written (measured at step 7 against the
customer's real largest sheet, not a fixture).

---

## Step detail

Moved here from `docs/07` §2 on 2026-08-23, when that file was re-cut as a
decisions document. This is the plan an executor worked from, verbatim; the
checklist above records what actually happened at each step. Written in the
future tense of a plan that has since shipped — read it as history.

### Step 1 — Extract `ActionMatches` + `rbac.Principal`

**Goal.** Make the RBAC semantics callable in bulk from a handler without changing
any existing behaviour, so later steps can filter a tool catalog in one query.

**Files.**
- create `apps/backend/internal/module/rbac/permission.go` — `ActionMatches`, `Principal`, `Principal.Allows`
- create `apps/backend/internal/module/rbac/permission_test.go`
- modify `apps/backend/internal/module/rbac/service.go` — add `Authorize`, reduce `HasPermission` to a wrapper
- modify `apps/backend/internal/module/rbac/service_test.go` — cover `Authorize`

**Depends on.** Nothing. **Gated by Decision 3.**

**Verify.**
```
cd apps/backend && go test ./internal/module/rbac/... ./internal/middleware/... -count=1
make test && make lint
```
The existing `HasPermission` and `RequirePermission` tests must pass **unmodified** —
that is the proof this is a pure refactor. If a middleware test needs editing, the
refactor changed behaviour and is wrong.

**Ground rules.** No `net/http` in the service. `Principal` carries actions only —
it must never grow a credential field.

---

### Step 2 — `mcp_api_keys` migration + PAT module

**Goal.** Ship mint/list/revoke for long-lived, org-scoped, revocable MCP keys. No
MCP protocol code yet — this step is only the credential.

**Files.**
- create `apps/backend/migrations/00008_mcp_api_keys.sql`
- create `apps/backend/internal/infra/database/queries/mcp_api_keys.sql`
- **run `make sqlc`** (regenerates `internal/infra/database/db/`) — an explicit action in this step, not a side effect
- create `apps/backend/internal/module/mcpkey/{service.go,handler.go,dto.go}`
- create `apps/backend/internal/module/mcpkey/{service_test.go,handler_test.go}`
- create `apps/backend/internal/server/mcpkey_integration_test.go`
- modify `apps/backend/internal/shared/apperror/apperror.go` — `MCP_KEY_NOT_FOUND` (404), `MCP_KEY_NAME_TAKEN` (409)
- modify `apps/backend/internal/server/server.go` — wire the module
- modify `apps/backend/internal/shared/logger/redact.go` — nothing new needed if the
  raw-token JSON field is named `apiKey` (already in the sensitive set); **verify
  this rather than assume it**, and add the key there if the field is named otherwise
- modify `docs/02-api-contract.md` — new "MCP keys" section + the two error codes
- **run `make swagger`**

**Routes** (all `perm:` guarded, reusing the existing pattern):

| Method/Path | Guard | Behaviour |
| --- | --- | --- |
| `POST /mcp-keys` | perm:`mcpkey:write` | `{ name, expiresInDays? }` → `{ id, name, apiKey, expiresAt, createdAt }`. **`apiKey` returned here and nowhere else.** |
| `GET /mcp-keys` | perm:`mcpkey:read` | Org's keys, no hash, no raw token. |
| `DELETE /mcp-keys/:keyId` | perm:`mcpkey:delete` | Sets `revoked_at`. `{ success: true }`. 404 for another org's id. |

**Depends on.** Step 1 (not strictly — it compiles without it — but keeping the order
avoids a merge conflict in `rbac`). **Gated by Decision 1.**

**Verify.**
```
make migrate && make sqlc && make test
go test ./internal/server/ -run TestMCPKey -count=1
curl -X POST localhost:3000/mcp-keys -H "Authorization: Bearer $TOK" \
  -H "x-organization-id: $ORG" -d '{"name":"laptop"}'
```
Tests: unit (service, mocked store — token generated once, hash stored, raw token
never persisted) **and** integration (every route × happy path × 401/403/404/409).

**Ground rules.** Additive-forward migration — `00008` is new, nothing earlier is
touched. `make sqlc` is run in this step. The raw token is never logged at any level.

---

### Step 3 — `RequireMCPKey` + `POST /mcp/:connectorId` + one trivial tool ⚠️ **risk retirement**

**Goal.** Prove the whole authorization path end to end — PAT → org → connector →
RBAC-filtered tool list → `tools/call` → audit — against one trivial tool, before any
Google code exists. This is the step that either validates or invalidates the plan.

**Files.**
- create `apps/backend/internal/middleware/mcpkey.go` — `RequireMCPKey`, resolving a bearer PAT to `(userID, orgID, principal)` via `rbac.Authorize` ∩ `scopes`
- create `apps/backend/internal/middleware/mcpkey_test.go`
- create `apps/backend/internal/module/mcp/{handler.go,service.go,catalog.go,errors.go}`
- create `apps/backend/internal/module/mcp/{service_test.go,catalog_test.go}`
- create `apps/backend/internal/server/mcp_integration_test.go`
- modify `apps/backend/internal/module/auditlog/service.go` — `mcp.session.started`, `mcp.tool.called`, `mcp.tool.denied`
- modify `apps/backend/internal/server/server.go` — mount `POST /mcp/:connectorId`
- modify `apps/backend/go.mod` — `github.com/modelcontextprotocol/go-sdk v1.7.0`
- modify `docs/02-api-contract.md` — new "MCP gateway" section (JSON-RPC envelope, its own section shape, per `docs/05`)
- **run `make swagger`**

**Read this before writing any code in this step.**
`spikes/mcp-gateway/` is a **working reference implementation of exactly this
pattern**, verified against SDK v1.7.0. Read these three files and port the *pattern*
— do not import the module, do not copy it into `apps/backend`, do not add it to the
build (it is a separate Go module and must stay one):

| Spike file | What to take from it |
| ---------- | -------------------- |
| `cmd/httpsrv/main.go` | `mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server, opts)` — the per-request server construction that stateless mode requires, plus the `WWW-Authenticate: Bearer realm="sapanjai"` header on 401 that makes MCP clients offer to re-authenticate instead of erroring out. |
| `internal/gateway/gateway.go` | `BuildServer` (construction-time filtering) and `EnforcePermissions` (the `mcp.Middleware` on `tools/call` + `tools/list`), registered via `s.AddReceivingMiddleware`. The `IsError` denial body is already written correctly there. |
| `internal/tools/tools.go` | The `Entry{Name, Permission, Description, Register}` catalog shape — one declaration site binding tool to RBAC action, consumed by both enforcement layers. |
| `internal/principal/principal.go` | Its package comment names the two real calls that replace the spike's fixture table. In our case it is one: `rbac.Service.Authorize` from step 1. |

**Design points fixed here.**
- `StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: ...}` — per `docs/05` constraint 2.
- **Mount into Echo with `echo.WrapHandler(mcpHandler)`.** The SDK gives an ordinary `http.Handler`; Echo wraps it directly.
- ⚠️ **The principal must be put on the *request* context, not `echo.Context`.** `c.Set(...)` writes to Echo's own map, which `mcp.NewStreamableHTTPHandler`'s `func(r *http.Request)` callback **cannot see** — it only receives `r.Context()`. `RequireMCPKey` must therefore end with:
  ```go
  ctx := context.WithValue(c.Request().Context(), principalKey{}, p)
  c.SetRequest(c.Request().WithContext(ctx))
  ```
  This is the single most likely way to lose an hour in this step. The spike does the equivalent at `cmd/httpsrv/main.go`'s `withAuth`.
- SDK surface to expect (all verified in the spike against v1.7.0, but re-check the godoc if a signature does not compile — do **not** invent an adaptation): `mcp.NewServer`, `mcp.Implementation`, `mcp.ServerOptions{Instructions, Logger}`, `mcp.Middleware`, `mcp.MethodHandler`, `mcp.Request`, `mcp.CallToolParamsRaw`, `mcp.ListToolsResult`, `mcp.CallToolResult`, `mcp.Content`, `mcp.TextContent`, `mcp.AddTool`.
- The connector in the path is resolved **and its `organization_id` checked against the PAT's org**. A mismatch is a not-found, indistinguishable from a nonexistent id.
- **Both enforcement layers** from `docs/05`: unpermitted tools are never registered (invisible in `tools/list`), *and* a receiving middleware re-checks the action on every `tools/call`.
- Denials return `CallToolResult{IsError: true}` with the text `Missing permission: <action>` — byte-identical to the REST 403 body. `errors.go` owns the `apperror` → `IsError` mapping; it is a second mapping alongside the HTTP one, not a replacement.
- The trivial tool is **`sapanjai_describe_connector`** (permission `connector:read`): returns the connector's `name` / `type` / `status` only. Chosen because it exercises connector resolution and tenant isolation while being structurally incapable of leaking `config`.

**Depends on.** Steps 1, 2.

**Verify.**
```
go test ./internal/module/mcp/... ./internal/middleware/... -count=1
go test ./internal/server/ -run TestMCP -count=1
npx @modelcontextprotocol/inspector          # initialize → tools/list → tools/call
```
Plus a real client: add to `~/.claude.json` as an HTTP MCP server with
`Authorization: Bearer sk_live_...` and confirm the tool appears and returns.
Integration tests must cover: no key → 401; revoked key → 401; expired key → 401;
key for org A + connector of org B → not-found; principal without `connector:read` →
tool absent from `tools/list` **and** `IsError` on a direct `tools/call`.

**Ground rules.** Audit writes are best-effort — a failed audit insert never fails an
MCP call. Log individual fields only; never log the request envelope. No CORS
middleware — MCP clients are not browsers.

---

### Step 4 — Redis token-bucket rate limiter

**Goal.** Land the limiter **before** any tool can reach a Google API, so no real
tool ever ships unguarded.

**Files.**
- create `apps/backend/internal/infra/redis/ratelimit.go` — token bucket on `mcp:ratelimit:<connectorId>`, exposing `Take(ctx, connectorID, n int)` so a caller can charge more than one unit
- create `apps/backend/internal/infra/redis/ratelimit_test.go`
- modify `apps/backend/internal/module/mcp/service.go` — check before dispatch
- modify `apps/backend/internal/module/mcp/errors.go` — `RATE_LIMITED` with retry-after seconds
- modify `apps/backend/internal/module/auditlog/service.go` — `mcp.ratelimit.hit`
- modify `apps/backend/internal/config/config.go` + `.env.example` + `.env.docker.example` + `README.md` + `CLAUDE.md` — `MCP_RATE_LIMIT_PER_MIN` (default 60)
- modify `docs/02-api-contract.md` — the new env var and error code

⚠️ **The bucket counts _upstream Google API calls_, not MCP tool calls.** This is the
non-obvious part and it must be built this way from the start. A single
`sheets_query_rows` (step 7) issues a paged scan — potentially many Google requests —
so a limiter that charges one unit per tool call would let one agent burn the org's
Google quota while reporting itself well under budget. Step 7's scan loop charges the
bucket per page fetched and aborts mid-scan when the bucket is empty. The public
Google quotas this defends (~60 req/min/user, ~300 req/min/project) are counted in
*API requests*, so anything else measures the wrong thing.

**Depends on.** Step 3.

**Verify.**
```
go test ./internal/infra/redis/... -count=1
go test ./internal/server/ -run TestMCPRateLimit -count=1
```
Integration test: 61 calls in a minute → the 61st returns `IsError` with
`RATE_LIMITED` and a retry-after, and one `mcp.ratelimit.hit` audit row exists.

**Ground rules.** Reuses the existing Redis key convention — no new infra. The audit
write stays best-effort.

---

### Step 5 — `google_sheets` connector type: config schema, allowlist, OAuth exchange, health checker

**Goal.** Teach the platform the connector type and its credential shape, and land
the allowlist — the spec's single most important security boundary (§3) — in the same
step, before any tool can read a cell.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/{config.go,oauth.go,api.go,client.go,checker.go}`
- create `apps/backend/internal/adapter/googlesheets/{config_test.go,oauth_test.go,client_test.go,checker_test.go}`
- modify `apps/backend/go.mod` — `google.golang.org/api`, `golang.org/x/oauth2`
- modify `apps/backend/internal/module/connector/types.go` — `TypeGoogleSheets Type = "google_sheets"` in `validTypes`
- modify `apps/backend/internal/server/server.go` — `connector.NewRegistry(googlesheets.NewChecker(...))`
- modify `apps/backend/internal/server/connector_integration_test.go` — type is now accepted
- modify `docs/02-api-contract.md` — `type` accepts `google_sheets`; health-check no longer universally 501
- modify `CLAUDE.md` — the layout gains `internal/adapter/`
- **run `make swagger`**

**Design points fixed here.**
- New top-level `internal/adapter/` package. It imports `connector` for the `Checker`
  interface; `connector` does not import it (the registry is assembled in `server.go`),
  so there is no cycle, and step 6+ can import the client without touching `connector`.
- `config.go` parses and validates the §3 shape and exposes
  `IsSpreadsheetAllowed(id)` / `IsFolderAllowed(id)`. **Every** tool calls these on
  every request against the stored config — never against a value cached from
  connector-creation time.
- **Use the official Google clients, not hand-rolled HTTP:**
  `google.golang.org/api/sheets/v4`, `google.golang.org/api/drive/v3`, and
  `golang.org/x/oauth2/google` for the token. Construct with
  `option.WithTokenSource(ts)` plus `option.WithHTTPClient(...)` for an explicit
  timeout. They are pure Go, so the distroless image is unaffected.
- Google access tokens are cached **in-process**, deliberately *not* in Redis — a
  refresh-derived access token is a live customer credential and Redis is not the
  right custody boundary for it. **`oauth2.ReuseTokenSource` already is this cache**:
  it holds the token in memory and refreshes on expiry. Keep one `TokenSource` per
  connector id in a mutex-guarded map; do not write a bespoke TTL cache.
- **Test seam — `api.go`.** Declare a narrow interface for exactly the four upstream
  operations the adapter needs, in the house style of `connectorStore` / `sealer`:
  ```go
  type sheetsAPI interface {
      SpreadsheetMeta(ctx context.Context, spreadsheetID string) (*Meta, error)
      Values(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error)
      ListFiles(ctx context.Context, folderID string, pageToken string) (*FilePage, error)
      File(ctx context.Context, fileID string) (*File, error)
  }
  ```
  `client.go` is the thin implementation over the Google SDKs. Unit tests in steps 6–9
  mock `sheetsAPI` and never touch the network. **One** contract test in
  `client_test.go` exercises the real wrapper against an `httptest` server via
  `option.WithEndpoint`, proving the mapping is right — that is the only place a
  Google response shape is asserted.
- `checker.Check` performs a token refresh plus one cheap metadata read, turning the
  501 stub into a real health check for this type.
- **Header-row handling** (spec open question 3, resolved here since `describe` needs
  it): row 1 is the header by default, with an optional per-spreadsheet override
  `scope.header_rows: {"<spreadsheetId>": 3}` in config. Cheap, additive, and real
  customer sheets do carry title rows above the header.

**Depends on.** Step 4 (ordering only). **Gated by Decision 2.**

**Verify.**
```
go test ./internal/adapter/googlesheets/... -count=1
go test ./internal/server/ -run TestConnector -count=1
curl -X POST localhost:3000/connectors/$ID/health-check -H "Authorization: Bearer $TOK" -H "x-organization-id: $ORG"
```
Allowlist tests must include the negative case explicitly: a spreadsheet id the OAuth
token *can* reach but the allowlist omits is rejected.

**Ground rules.** Decrypted config never leaves the owning service — the `Checker`
contract already states this, and the adapter must not log, retain, or return it.
No config field on any DTO.

---

### Step 6 — `sheets_list_spreadsheets` + `sheets_describe_spreadsheet`

**Goal.** The first real data tools, and the schema-discovery tool the spec calls
non-negotiable (§4.1) — without it an agent guesses ranges and fails every time.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/{list.go,describe.go}` + tests
- create `apps/backend/internal/module/mcp/tools_sheets.go` — catalog entries, `sheets:read`, `ReadOnlyHint: true`
- modify `apps/backend/internal/module/mcp/catalog.go`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md` — tool catalog table
- modify `docs/07-sheets-adapter-decisions.md` — tick the step in the tracker

**Depends on.** Step 5.

**Verify.**
```
go test ./internal/adapter/googlesheets/... ./internal/module/mcp/... -count=1
npx @modelcontextprotocol/inspector    # describe a real allowlisted spreadsheet
```
Tests: `SPREADSHEET_NOT_ALLOWED` for a non-allowlisted id; tool invisible without
`sheets:read`; `include_sample_rows` bounded 0–5.

**Ground rules.** Tool and field descriptions are prompt surface (`docs/05`) — budget
review time for their wording. Audit `mcp.tool.called` records `spreadsheet_id` and
`sheet_name`, never cell values.

---

### Step 7 — `sheets_query_rows` (the workhorse)

**Goal.** The structured filter DSL of §4.2, with projection, pagination, result caps,
and formula-injection handling — the tool the product actually sells.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/query.go` + `query_test.go`
- create `apps/backend/internal/adapter/googlesheets/filter.go` + `filter_test.go` — `eq/neq/contains/gt/lt/gte/lte/in`
- modify `apps/backend/internal/module/mcp/tools_sheets.go`
- modify `apps/backend/internal/module/mcp/errors.go` — `COLUMN_NOT_FOUND`, `SHEET_NOT_FOUND`, `RESULT_TOO_LARGE`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md`

> ### ⚠️ Decision 4 — `total` is dropped in favour of a bounded scan
> **Confirm with the owner before starting this step.**
>
> Spec §4.2's example output has `"total": 340` — the count of *all* matching rows.
> That is not compatible with §6's "never load the whole sheet into memory", and the
> two cannot both be honoured: the Sheets API has no server-side filter and no count
> endpoint, so an exact total requires evaluating the filter over **every** row. At the
> spec's own target scale that is either unbounded memory or hundreds of upstream calls
> per tool invocation — which step 4's limiter would (correctly) cut off mid-answer.
>
> **Resolution: bounded scan with an honest signal.** Replace `total` with:
> ```jsonc
> { "count": 50, "offset": 0, "has_more": true, "next_offset": 50,
>   "scanned_rows": 5000, "scan_complete": false }
> ```
> `total` is emitted **only** when `scan_complete` is true (i.e. the scan reached the
> end of the sheet within budget), where it is exact and free. When `scan_complete` is
> false the tool description and the result text tell the agent the answer is partial
> and to narrow the filter — an agent that knows its count is a floor is strictly
> better off than one handed a confidently wrong total.
>
> This is a deliberate deviation from the spec's §4.2 example. It needs the owner's
> sign-off, and step 12 must write it back into `docs/06`.

**Design points fixed here.**
- **No Google Visualization Query Language is ever exposed** (§6). Filters are a
  structured DSL evaluated in our code; any agent-supplied value beginning `=`, `+`,
  `-`, or `@` is treated as a literal string and never interpolated into anything
  Google will evaluate.
- **Never load a whole sheet into memory** (§6) — 87 GB makes this load-bearing. The
  scan loop is the core of this step:
  1. Fetch a bounded page of rows (**5,000 rows per `Values.Get`**, tune later) via the
     `sheetsAPI` seam from step 5.
  2. Charge step 4's rate-limit bucket **one unit per page fetched**, not one per tool
     call. An empty bucket ends the scan early with `scan_complete: false`, not an error.
  3. Evaluate filters in-process, retaining at most `offset + limit + 1` matched rows —
     the `+1` is what sets `has_more` without a second pass.
  4. Stop at whichever comes first: enough matches, end of sheet, the scan budget
     (**50,000 rows**, configurable), or an empty bucket.
  Peak memory is one page plus the retained window, independent of sheet size.
- `limit` max 200, default 50. Response body cap ~256 KB → `RESULT_TOO_LARGE` telling
  the agent to use `columns` projection or narrow the filter.
- `response_format: markdown | json`, default markdown.

**Depends on.** Step 6.

**Verify.**
```
go test ./internal/adapter/googlesheets/ -run 'TestFilter|TestQuery' -count=1 -race
go test ./internal/server/ -run TestMCPQueryRows -count=1
```
Tests must include: a filter value of `=IMPORTRANGE(...)` round-trips as a literal and
reaches no evaluator; `limit=201` rejected; `has_more`/`next_offset` correct across a
page boundary; unknown column → `COLUMN_NOT_FOUND`; a mocked sheet larger than the scan
budget returns `scan_complete: false` with **no** `total` field and peak retained rows
≤ `offset + limit + 1`; an exhausted rate-limit bucket mid-scan ends the scan cleanly
rather than erroring.

**Ground rules.** Audit metadata carries `filter_columns[]` but **never filter
`value`s** (§7) — those are real business data (partner names, contract numbers).

---

### Step 8 — `sheets_read_range` (escape hatch)

**Goal.** Direct A1-range reads for the cases the DSL cannot express. The smallest
step in the plan; kept separate so step 7 stays reviewable.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/readrange.go` + test
- modify `apps/backend/internal/module/mcp/tools_sheets.go`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md`

**Depends on.** Step 7 (shares the cap/truncation helpers).

**Verify.** `go test ./internal/adapter/googlesheets/ -run TestReadRange -count=1`.
The A1 range must be parsed and re-validated against the allowlist — a range string is
agent-supplied input, not a trusted identifier.

---

### Step 9 — `drive_list_folder` + `drive_get_file`

**Goal.** The Drive half of the adapter, with short-lived links.

**Files.**
- create `apps/backend/internal/adapter/googlesheets/drive.go` + `drive_test.go`
- create `apps/backend/internal/module/mcp/tools_drive.go` — `drive:read`
- modify `apps/backend/internal/module/mcp/catalog.go`
- modify `apps/backend/internal/server/mcp_integration_test.go`
- modify `docs/02-api-contract.md`

**Design point.** `drive_get_file` returns a **signed URL with TTL ≤ 15 minutes**
(§4.3), never a permanent link — agent conversation logs are a leak channel.

**Depends on.** Step 5 (shares OAuth/allowlist); independent of steps 6–8.

**Verify.**
```
go test ./internal/adapter/googlesheets/ -run TestDrive -count=1
go test ./internal/server/ -run TestMCPDrive -count=1
```
Tests: folder outside the allowlist rejected; issued URL expires; tools invisible
without `drive:read` (and specifically **not** granted by `sheets:read`).

---

### Step 10 — Frontend: MCP key management

**Goal.** Let an owner mint and revoke keys without curl. The raw key is shown once,
with a copy affordance and an explicit "you will not see this again".

**Files.**
- create `apps/frontend/app/(dashboard)/mcp-keys/page.tsx`
- create `apps/frontend/lib/api/mcp-keys.ts`
- create `apps/frontend/components/mcp-keys/{key-list.tsx,create-key-dialog.tsx}`
- modify `apps/frontend/components/nav/*` — nav entry
- create `apps/frontend/app/(dashboard)/mcp-keys/page.test.tsx`

**Depends on.** Step 2.

**Verify.**
```
cd apps/frontend && pnpm lint && pnpm exec tsc --noEmit && pnpm test
```
Manual: mint → copy → use in Claude Code → revoke → next call 401.

**Ground rules.** Same-origin `/api/*` proxy only. The raw key is held in component
state and never written to `localStorage`.

---

### Step 11 — Frontend: `google_sheets` connector setup

**Goal.** A form for the paste-path credentials plus the allowlist, so onboarding is
a page rather than a curl transcript.

**Files.**
- create `apps/frontend/app/(dashboard)/connectors/[id]/google-sheets/page.tsx`
- create `apps/frontend/components/connectors/google-sheets-form.tsx`
- modify `apps/frontend/lib/api/connectors.ts`

**Depends on.** Steps 5, 10.

**Verify.** `pnpm lint && pnpm exec tsc --noEmit && pnpm test`; manual round-trip
create → health-check green → tool call succeeds.

**Ground rules.** `config` is write-only in the UI, because it is write-only in the
API — the form never renders a stored secret back, since no endpoint returns one.

---

### Step 12 — Docs consolidation + `docs/05` status update

**Goal.** Leave the design docs true. Small, but it is the step that stops the next
person re-deriving all of this.

**Files.**
- modify `docs/05-mcp-gateway.md` — Phase 1 and Phase 2 status
- modify `docs/06-sheets-adapter.md` — mark the four §10 questions resolved, with pointers, **and correct §4.2's example output to the bounded-scan shape** (Decision 4). Leaving the contradicted `total` in the spec is how the next reader re-introduces the bug.
- modify `CLAUDE.md` — MCP module, `internal/adapter/`, new env vars, new Redis keys
- modify `README.md` — MCP client setup quickstart
- move this plan to `.claude/plans/archives/` per repo convention

**Depends on.** Steps 1–11.

**Verify.** `make test && make lint`; `cd apps/frontend && pnpm lint`. Docs-only
otherwise — the check is that a reader can set up an MCP client from `README.md` alone.
