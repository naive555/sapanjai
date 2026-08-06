# spikes/mcp-gateway

Phase 0 feasibility spike for the Sapanjai Managed MCP Gateway. No database, no
Redis, no network calls.

Answers: can a Go MCP server expose per-org, permission-scoped tools to Claude,
and does the platform core's existing RBAC model carry over?

**Yes to both.** Read [`docs/FINDINGS.md`](docs/FINDINGS.md) first, then
[`docs/RBAC-MAPPING.md`](docs/RBAC-MAPPING.md). The distilled version that the
backend needs lives in [`docs/05-mcp-gateway.md`](../../docs/05-mcp-gateway.md).

> **This is a separate Go module** (`github.com/sapanjai/spikes/mcp-gateway`),
> kept out of `apps/backend` on purpose. `make test`, `make lint` and CI are all
> scoped to `apps/backend`, and Go skips nested modules in `./...`, so nothing
> here is built or linted by the main pipeline. Run its commands from this
> directory.

## Run it

```bash
cd spikes/mcp-gateway
go test ./...                              # 8 tests: handshake, RBAC filtering, tenant isolation
go run ./cmd/httpsrv -addr 127.0.0.1:8090  # the product shape
go run ./cmd/stdio                         # local dev / Claude Desktop shape
```

## Try it against Claude Code

```bash
go build -o bin/sapanjai-mcp-stdio.exe ./cmd/stdio
claude mcp add sapanjai-spike --scope local \
  --env SAPANJAI_TOKEN=tok_reader_siam \
  -- "$PWD/bin/sapanjai-mcp-stdio.exe"
claude mcp list
```

Then ask Claude *"list my overdue invoices"* and *"create an invoice for
50,000 baht"* — the second is refused, because `tok_reader_siam` holds
`invoice:read` and not `invoice:write`. Swap to `tok_bookkeeper_siam`
(`invoice:*`) and it succeeds.

Remove with `claude mcp remove sapanjai-spike`.

## Demo principals

Bearer token → principal, in `internal/principal/principal.go`. Each covers one
branch of `HasPermission`:

| Token                 | Org               | Role   | Actions                    | Sees                    |
| --------------------- | ----------------- | ------ | -------------------------- | ----------------------- |
| `tok_owner_siam`      | Siam Trading      | owner  | — (bypassed)               | all 3 tools             |
| `tok_reader_siam`     | Siam Trading      | member | `invoice:read`             | 2 read tools            |
| `tok_bookkeeper_siam` | Siam Trading      | member | `invoice:*`                | all 3 tools             |
| `tok_nogrants_siam`   | Siam Trading      | member | `report:read`              | nothing                 |
| `tok_reader_bkl`      | Bangkok Logistics | member | `invoice:read`             | 2 tools, disjoint data  |

```bash
curl -s -X POST http://127.0.0.1:8090/mcp \
  -H 'Authorization: Bearer tok_reader_siam' \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## Tools

| Tool                | Required action | |
| ------------------- | --------------- | - |
| `list_invoices`     | `invoice:read`  | optional `status`, `limit` |
| `get_invoice_by_id` | `invoice:read`  | org-scoped lookup |
| `create_invoice`    | `invoice:write` | THB, 7% VAT computed |

Mock data is Thai-shaped: THB, 13-digit tax IDs, VAT broken out — two orgs, so
tenant isolation is testable rather than asserted.

## What this is not

Not production code. `internal/principal` is a hardcoded map where the real
thing calls `VerifyAccessToken` + `ListPermissionActionsByUserOrg`;
`internal/mockdata` mutates package state with no locking; `internal/rbac` is a
port that exists so the spike stays dependency-free. The *shape* of
`internal/gateway` and `internal/tools` is what carries forward.
