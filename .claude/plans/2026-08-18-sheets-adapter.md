# Google Sheets/Drive MCP Adapter — execution tracker

> **Status: 🟡 in progress (planned 2026-08-18).** 4 / 12 steps complete — the riskiest unknown (MCP handshake + RBAC end to end) is retired, and the rate limiter is landed ahead of any real Google adapter.
> **All four decisions confirmed by the owner 2026-08-18** — no step is decision-blocked.
> **Full plan with per-step detail:** [`docs/07-sheets-adapter-plan.md`](../../docs/07-sheets-adapter-plan.md)
> **Spec:** [`docs/06-sheets-adapter.md`](../../docs/06-sheets-adapter.md) · **Architecture:** [`docs/05-mcp-gateway.md`](../../docs/05-mcp-gateway.md)
>
> This file is the checklist an executor works from; `docs/07` holds the goals,
> file lists, verification commands, and ground-rule callouts for each step.
> Update the checkboxes here as steps merge, then archive this file to
> `.claude/plans/archives/` when step 12 lands.
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
- [ ] **5. `google_sheets` connector type** — config schema, **allowlist**, OAuth exchange, real health checker. New `internal/adapter/` package; official Google clients + `oauth2.ReuseTokenSource`; narrow `sheetsAPI` interface as the mock seam. *(gated by Decision 2)*
- [ ] **6. `sheets_list_spreadsheets` + `sheets_describe_spreadsheet`** — first real data tools; `describe` is the prerequisite for everything after.
- [ ] **7. `sheets_query_rows`** — structured filter DSL, projection, bounded scan loop, caps, formula-injection literals. No Google Query Language exposed, ever. *(gated by Decision 4)*
- [ ] **8. `sheets_read_range`** — escape hatch; smallest step, kept separate so 7 stays reviewable.
- [ ] **9. `drive_list_folder` + `drive_get_file`** — `drive:read`, signed URLs with TTL ≤ 15 min.
- [ ] **10. Frontend: MCP key management** — raw key shown once, never persisted.
- [ ] **11. Frontend: `google_sheets` connector setup** — write-only config form.
- [ ] **12. Docs consolidation** — `docs/05` status, `docs/06` §10 marked resolved, `CLAUDE.md`, `README.md`; archive this file.

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
