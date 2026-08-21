// Package mcp implements the MCP gateway: one Streamable HTTP endpoint per
// connector (POST /mcp/:connectorId) exposing an RBAC-filtered tool catalog
// to an authenticated PAT holder. This is docs/07-sheets-adapter-plan.md's
// step 3, the risk-retirement step — it proves PAT -> org -> connector ->
// RBAC-filtered tool list -> tools/call -> audit end to end against one
// trivial tool before any Google API code exists.
//
// The pattern is ported from spikes/mcp-gateway (internal/gateway +
// internal/tools), verified there against SDK v1.7.0: never import that
// module, never add it to this build.
package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/adapter/googlesheets"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/rbac"
)

// toolCallCost is the floor every tools/call charges against its
// connector's rate-limit bucket, per docs/07-sheets-adapter-plan.md step 4:
// "the bucket counts upstream Google API requests, not MCP tool calls," but
// step 3's trivial tool (and every tool up to step 5) makes zero upstream
// calls. Charging nothing would let a tool call be free right up until the
// first real adapter ships, so every tools/call is worth a floor of 1 unit
// — the gateway itself is worth limiting. A real adapter that issues N
// upstream requests (step 7's paged sheet scan) charges N by calling
// Service.ChargeRateLimit directly, mid-execution, per page fetched,
// instead of relying on this dispatch-time floor.
const toolCallCost = 1

// ServerName and ServerVersion identify this gateway to clients during the
// initialize handshake.
const (
	ServerName    = "sapanjai-mcp-gateway"
	ServerVersion = "0.1.0"
)

// connectorGetter is the subset of *connector.Service this depends on,
// narrowed so unit tests can hand-mock it. Get already does everything this
// package needs for tenant isolation: it scopes by organization_id and
// returns apperror.NotFound for a connector belonging to another org,
// byte-indistinguishable from one that does not exist at all. OpenConfig
// decrypts an already-fetched row's config scoped to its organization — the
// seam step 6's Google Sheets tools use to reach a connector's live config
// (never a value cached from connector-creation or session-start time) on
// every single tool invocation.
type connectorGetter interface {
	Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error)
	OpenConfig(ctx context.Context, organizationID uuid.UUID, encryptedConfig json.RawMessage) (map[string]any, error)
}

var _ connectorGetter = (*connector.Service)(nil)

// rateLimiter is the subset of *appredis.RateLimiter this depends on,
// narrowed so unit tests can hand-mock it without importing infra/redis
// (mirrors connectorGetter's reasoning). n is charged in whole units — see
// toolCallCost's doc comment for what "a unit" means today and how that
// changes once a real adapter lands.
type rateLimiter interface {
	Take(ctx context.Context, connectorID string, n int) (allowed bool, retryAfter time.Duration, err error)
}

// Service builds per-request *mcp.Server instances scoped to one
// authenticated principal and one resolved connector, wiring both
// enforcement layers plus best-effort audit writes.
type Service struct {
	connectors connectorGetter
	limiter    rateLimiter
	audit      *auditlog.Service
	log        *slog.Logger

	// sheetsTokens holds one OAuth TokenSource per connector id, shared
	// across every request this long-lived Service instance handles — see
	// googlesheets.TokenSourceCache's doc comment on why that reuse is
	// deliberate (a refresh-derived access token is a live customer
	// credential, kept in-process memory rather than Redis). Building it
	// unconditionally in NewService keeps that constructor's signature
	// stable for every existing call site; only Google Sheets tool
	// handlers (tools_sheets.go) ever read from it.
	sheetsTokens *googlesheets.TokenSourceCache

	// fileLinkKey is the HKDF-derived signing key drive_get_file
	// (tools_drive.go) mints download links with and Handler.downloadFile
	// verifies them against — derived once here, never masterKey itself
	// (see filelink.go's deriveFileLinkKey doc comment). nil when masterKey
	// was empty, which both call sites treat as "link minting/verification
	// disabled" rather than ever signing or checking against an empty key.
	fileLinkKey []byte
}

// NewService builds an mcp Service. limiter may be nil — every call site
// that constructs a Service without one (every unit test in this package
// today) gets no rate limiting rather than a nil-pointer panic; server.go's
// production wiring always supplies a real *appredis.RateLimiter. masterKey
// is cfg.ConnectorMasterKey; NewService derives the file-link signing key
// from it once here (rather than re-deriving per call) — an empty masterKey
// disables link minting (docs/07-sheets-adapter-plan.md step 9) rather than
// panicking, since every unit test in this package that has no need for
// drive_get_file's link-minting path passes nil.
func NewService(connectors connectorGetter, limiter rateLimiter, audit *auditlog.Service, log *slog.Logger, masterKey []byte) *Service {
	return &Service{
		connectors:   connectors,
		limiter:      limiter,
		audit:        audit,
		log:          log,
		sheetsTokens: googlesheets.NewTokenSourceCache(),
		fileLinkKey:  deriveFileLinkKey(masterKey),
	}
}

// openGoogleSheetsConfig decrypts conn's stored config and parses it as a
// google_sheets Config. Called by every sheets_* tool handler on every
// invocation — never cached across calls — so a narrowed allowlist in the
// stored config takes effect on the very next call
// (docs/07-sheets-adapter-plan.md step 5's design point, first exercised by
// a real tool here in step 6). conn.OrganizationID is trusted as-is: it
// came from ResolveConnector, which already scoped the row lookup to the
// caller's own organization.
func (s *Service) openGoogleSheetsConfig(ctx context.Context, conn db.Connector) (*googlesheets.Config, error) {
	raw, err := s.connectors.OpenConfig(ctx, conn.OrganizationID, conn.EncryptedConfig)
	if err != nil {
		return nil, err
	}
	return googlesheets.ParseConfig(raw)
}

// ChargeRateLimit charges n units against connectorID's upstream-request
// budget directly, bypassing the per-tools/call dispatch check in enforce.
// This is the seam docs/07-sheets-adapter-plan.md step 4 asks for and step
// 7 will use: a tool whose handler makes N upstream Google API calls (the
// paged sheet scan) calls this once per page fetched, mid-execution,
// instead of paying a single floor-of-1 charge at dispatch time. No caller
// exists yet — step 3's trivial tool and this step's dispatch-time floor
// are the only rate-limit consumers today — but the method is exported now
// so a future tools_sheets.go can reach the same bucket enforce already
// checks, without either duplicating RateLimiter wiring or reaching past
// Service's own encapsulation of it.
func (s *Service) ChargeRateLimit(ctx context.Context, connectorID uuid.UUID, n int) (allowed bool, retryAfter time.Duration, err error) {
	if s.limiter == nil {
		return true, 0, nil
	}
	return s.limiter.Take(ctx, connectorID.String(), n)
}

// ResolveConnector fetches connectorID scoped to organizationID — always the
// authenticated principal's own organization (from RequireMCPKey), never a
// value read from the request path or body beyond the id itself. A
// connector belonging to another org is apperror.NotFound, the same code a
// nonexistent id produces.
func (s *Service) ResolveConnector(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	return s.connectors.Get(ctx, organizationID, connectorID)
}

// BuildServer returns an *mcp.Server exposing only the catalog tools p is
// permitted to use against conn. Constructing a fresh server per request is
// cheap (no I/O, no goroutines) and is what makes a per-tenant, per-connector
// tool surface possible in Stateless mode at all.
//
// Two independent enforcement layers, on purpose (see Service.enforce's
// doc comment for why layer 2 is not redundant once permissions can change
// mid-session or a client caches a stale tools/list):
//
//  1. Construction-time filtering, right here: a tool the principal lacks
//     permission for is never registered, so it never appears in
//     tools/list and the SDK reports its own "unknown tool" error if a
//     client calls it anyway.
//  2. Request-time enforcement, Service.enforce, an mcp.Middleware on
//     tools/call and tools/list.
//
// req is forwarded to every Entry this registers, permitted or not — see
// RequestInfo's doc comment for why (drive_get_file, step 9, is the one
// tool that reads it today).
func (s *Service) BuildServer(p *rbac.Principal, conn db.Connector, req RequestInfo) *gomcp.Server {
	srv := gomcp.NewServer(&gomcp.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, &gomcp.ServerOptions{
		Instructions: "Read-only tools scoped to one Sapanjai connector. The organization and " +
			"connector are fixed by the caller's credentials; there is no tool to select another.",
		Logger: s.log,
	})

	var granted, denied []string
	for _, e := range Catalog() {
		if !e.appliesTo(conn) {
			continue
		}
		if p.Allows(e.Permission) {
			e.Register(srv, s, conn, req)
			granted = append(granted, e.Name)
			continue
		}
		denied = append(denied, e.Name)
	}

	srv.AddReceivingMiddleware(s.enforce(p, conn))

	if s.log != nil {
		s.log.Info("built scoped mcp server",
			"user_id", p.UserID,
			"organization_id", p.OrganizationID,
			"connector_id", conn.ID,
			"tools_granted", granted,
			"tools_denied", denied,
		)
	}

	return srv
}

// enforce is the request-time enforcement layer and the audit hook.
// Redundant against BuildServer's construction-time filtering today, it
// becomes load-bearing the moment permissions can change mid-session, tools
// are registered dynamically, or a client caches a stale tool list — never
// treat tools/list as an authorization boundary (mirrors
// spikes/mcp-gateway/internal/gateway.EnforcePermissions, verified there
// against SDK v1.7.0).
func (s *Service) enforce(p *rbac.Principal, conn db.Connector) gomcp.Middleware {
	return func(next gomcp.MethodHandler) gomcp.MethodHandler {
		return func(ctx context.Context, method string, req gomcp.Request) (result gomcp.Result, err error) {
			switch method {
			case "initialize", "server/discover":
				// One HTTP POST per JSON-RPC call in Stateless mode, so
				// "session" here means one handshake, not a long-lived
				// connection — the closest analogue this transport has to
				// session-start.
				//
				// Two methods, not one: SDK v1.7.0 clients negotiate up to
				// protocol version 2026-07-28 (SEP-2575) by default, which
				// replaces the classic "initialize" call with
				// "server/discover" whenever the server also advertises
				// that range — true for every server built here, since
				// Stateless mode is required for the new protocol and this
				// gateway is Stateless unconditionally. Verified against
				// this exact SDK build: a plain `gomcp.NewClient` talking
				// to this handler sends server/discover, not initialize —
				// confirmed via internal/server/mcp_integration_test.go's
				// audit assertion, which only passed once this case listed
				// both methods. A pre-2026-07-28 client (older Claude
				// Code/Desktop builds, or a hand-rolled curl request that
				// only names "initialize") still sends the classic method,
				// so both must be handled for "mcp.session.started" to be
				// reliable across client vintages.
				s.auditSessionStarted(ctx, p, conn)

			case "tools/call":
				params, ok := req.GetParams().(*gomcp.CallToolParamsRaw)
				if !ok {
					return next(ctx, method, req)
				}
				action, known := PermissionFor(params.Name)
				if !known {
					// Not ours to judge — let the SDK produce its own
					// "unknown tool" protocol error.
					return next(ctx, method, req)
				}
				if !p.Allows(action) {
					s.auditToolDenied(ctx, p, conn, params.Name, action)
					return PermissionDenied(action), nil
				}
				// Rate-limit check runs after the permission check (an
				// unpermitted call should never spend budget) and before
				// dispatch — landed here, ahead of any real adapter, so no
				// tool ever ships able to reach a Google API unguarded
				// (docs/07-sheets-adapter-plan.md step 4). toolCallCost is
				// the dispatch-time floor; a real adapter charges its own
				// per-upstream-request cost mid-execution via
				// Service.ChargeRateLimit instead of relying solely on
				// this floor.
				if s.limiter != nil {
					allowed, retryAfter, limitErr := s.limiter.Take(ctx, conn.ID.String(), toolCallCost)
					if limitErr != nil {
						// A genuine infra failure (Redis unreachable, a
						// malformed script result) is not the same outcome
						// as an exhausted bucket: fail closed with a
						// readable IsError rather than either admitting an
						// unmetered call or aborting the whole JSON-RPC
						// turn with a protocol error the model can't act
						// on (see this file's package doc comment on the
						// two failure channels).
						if s.log != nil {
							s.log.Error("mcp rate limiter check failed", "error", limitErr,
								"connector_id", conn.ID, "tool", params.Name)
						}
						return ErrorResult(limitErr), nil
					}
					if !allowed {
						s.auditRateLimitHit(ctx, p, conn, params.Name)
						return RateLimited(retryAfter), nil
					}
				}
				// Audited via defer, *after* dispatch, not before —
				// docs/07-sheets-adapter-plan.md step 7 surfaced a real
				// gap here: spec §7's mcp.tool.called metadata wants
				// row_count and duration_ms, and neither exists until the
				// tool handler has actually run. Auditing before dispatch
				// (steps 3-6's design) could only ever record
				// spreadsheet_id/sheet_name; getting the two post-hoc
				// fields into the *same* row means waiting for next() to
				// return rather than emitting a second row per call or
				// permanently dropping them. This still fires exactly once
				// per permitted, budgeted call, before OR after a panic:
				// the defer runs during unwind, ahead of Echo's Recover
				// middleware (server.go) further up the call stack, so a
				// handler bug still leaves an audit trail. filter_columns
				// is extractable pre-dispatch from arguments (unchanged);
				// row_count is peeled out of the tool's own StructuredContent
				// post-dispatch by auditToolCalled, and only ever a column
				// count/row count — never a cell value.
				fields := auditableToolFields(params.Arguments)
				start := time.Now()
				// context.WithoutCancel, because moving this audit after
				// dispatch put it on the far side of a scan that can run
				// for seconds across several upstream page fetches: a
				// client that hangs up or times out mid-scan cancels ctx,
				// and auditlog.Record's write is synchronous on that same
				// ctx, so the row would simply never land. Before-dispatch
				// auditing never had that exposure — it wrote before the
				// slow part. Dropping the record for exactly the abandoned
				// and timed-out calls would gut the audit trail for the
				// most data-intensive tool in the catalog, which is
				// precisely where it matters most. Values are preserved;
				// only cancellation is detached.
				auditCtx := context.WithoutCancel(ctx)
				defer func() {
					s.auditToolCalled(auditCtx, p, conn, params.Name, time.Since(start), fields, result)
				}()
				result, err = next(ctx, method, req)
				return result, err

			case "tools/list":
				res, err := next(ctx, method, req)
				if err != nil {
					return res, err
				}
				list, ok := res.(*gomcp.ListToolsResult)
				if !ok {
					return res, nil
				}
				kept := make([]*gomcp.Tool, 0, len(list.Tools))
				for _, t := range list.Tools {
					action, known := PermissionFor(t.Name)
					if known && !p.Allows(action) {
						continue
					}
					kept = append(kept, t)
				}
				list.Tools = kept
				return list, nil
			}

			return next(ctx, method, req)
		}
	}
}

// auditSessionStarted, auditToolCalled, auditToolDenied, and
// auditRateLimitHit all funnel through recordAudit. Every metadata map here
// is deliberately tiny and
// explicit — connector_id, tool, missing_permission — never the request
// envelope, never a tool argument (which, once real adapters land, could be
// business data such as a spreadsheet cell value or a partner name; see
// docs/07-sheets-adapter-plan.md step 7's "audit metadata carries
// filter_columns[] but never filter values" ground rule, which starts here).

func (s *Service) auditSessionStarted(ctx context.Context, p *rbac.Principal, conn db.Connector) {
	s.recordAudit(ctx, auditlog.ActionMCPSessionStarted, p, map[string]any{
		"connector_id": conn.ID.String(),
	})
}

// auditToolCalled records mcp.tool.called for a permitted, budgeted call
// once it has actually finished dispatching (see enforce's tools/call case
// for why this fires from a defer rather than before next() runs).
// duration_ms is always recorded now that the timing is available;
// row_count is added only when result's own StructuredContent carries a
// top-level "count" or "row_count" field (sheets_query_rows and
// sheets_read_range, respectively, as of docs/07 step 8) — extracted via
// rowCountFromResult, never by re-deriving it from anything the caller
// passed in, so a future tool without either field simply omits it rather
// than reporting a misleading 0.
func (s *Service) auditToolCalled(ctx context.Context, p *rbac.Principal, conn db.Connector, tool string, elapsed time.Duration, extra map[string]any, result gomcp.Result) {
	metadata := map[string]any{
		"connector_id": conn.ID.String(),
		"tool":         tool,
		"duration_ms":  elapsed.Milliseconds(),
	}
	for k, v := range extra {
		metadata[k] = v
	}
	if rowCount, ok := rowCountFromResult(result); ok {
		metadata["row_count"] = rowCount
	}
	s.recordAudit(ctx, auditlog.ActionMCPToolCalled, p, metadata)
}

// rowCountFromResult peeks a completed tools/call's StructuredContent for a
// top-level "count" or "row_count" field: sheets_query_rows' output shape
// (docs/07 step 7's Decision 4: count/offset/has_more/...) names its field
// "count", while sheets_read_range's (docs/07 step 8) names its
// "row_count" — the more descriptive name for a tool whose output is a
// literal row count, not a match count. Rather than force read_range's
// public output schema to match query_rows' naming just so this internal
// extractor could stay narrower, the probe checks both field names; either
// way, the only thing ever pulled out is a single integer the tool itself
// already decided was safe to name in its own output schema — never
// "rows"/"cells" or any other field, so there is still no path from here
// to a cell value landing in an audit row.
func rowCountFromResult(result gomcp.Result) (int, bool) {
	ctr, ok := result.(*gomcp.CallToolResult)
	if !ok || ctr == nil || ctr.IsError {
		return 0, false
	}
	raw, ok := ctr.StructuredContent.(json.RawMessage)
	if !ok || len(raw) == 0 {
		return 0, false
	}
	var probe struct {
		Count    *int `json:"count"`
		RowCount *int `json:"row_count"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0, false
	}
	if probe.Count != nil {
		return *probe.Count, true
	}
	if probe.RowCount != nil {
		return *probe.RowCount, true
	}
	return 0, false
}

// auditableToolFields extracts the small, explicit set of tools/call
// argument keys that are ever copied into mcp.tool.called audit metadata:
// spreadsheet_id and sheet_name identify *which* sheets resource a call
// touched, file_id and folder_id do the same for the drive_* tools (step
// 9), and filter_columns lists the column *names* a sheets_query_rows call
// filtered on — exactly what an audit trail needs — without ever touching
// the argument's actual business data (a filter value, a cell value, or —
// for the drive tools — a file's contents). This is deliberately a fixed
// allowlist of key names, not "log the whole arguments object":
// docs/06-sheets-adapter.md §7, "log the column name filtered on, but never
// the value," and CLAUDE.md's "log individual fields, never a whole
// request/body struct" both apply to tool arguments exactly as they do to
// HTTP request bodies. Unknown fields (a future tool's own arguments) are
// silently ignored rather than logged, and a non-object or malformed
// Arguments payload yields no fields at all rather than an error — this is
// audit best-effort, not request validation.
func auditableToolFields(rawArgs json.RawMessage) map[string]any {
	if len(rawArgs) == 0 {
		return nil
	}
	var parsed struct {
		SpreadsheetID string `json:"spreadsheet_id"`
		SheetName     string `json:"sheet_name"`
		FileID        string `json:"file_id"`
		FolderID      string `json:"folder_id"`
		Filters       []struct {
			Column string `json:"column"`
			// Value is deliberately never decoded into this struct — see
			// the doc comment above and docs/07 step 7's ground rule:
			// filter_columns[] carries column names only, never values.
		} `json:"filters"`
	}
	if err := json.Unmarshal(rawArgs, &parsed); err != nil {
		return nil
	}
	fields := make(map[string]any, 5)
	if parsed.SpreadsheetID != "" {
		fields["spreadsheet_id"] = parsed.SpreadsheetID
	}
	if parsed.SheetName != "" {
		fields["sheet_name"] = parsed.SheetName
	}
	if parsed.FileID != "" {
		fields["file_id"] = parsed.FileID
	}
	if parsed.FolderID != "" {
		fields["folder_id"] = parsed.FolderID
	}
	if len(parsed.Filters) > 0 {
		columns := make([]string, 0, len(parsed.Filters))
		for _, f := range parsed.Filters {
			if f.Column != "" {
				columns = append(columns, f.Column)
			}
		}
		if len(columns) > 0 {
			fields["filter_columns"] = columns
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func (s *Service) auditToolDenied(ctx context.Context, p *rbac.Principal, conn db.Connector, tool, action string) {
	s.recordAudit(ctx, auditlog.ActionMCPToolDenied, p, map[string]any{
		"connector_id":       conn.ID.String(),
		"tool":               tool,
		"missing_permission": action,
	})
}

// auditRateLimitHit records mcp.ratelimit.hit when a tools/call is refused
// at dispatch time because its connector's upstream-request bucket is
// exhausted — a quota failure, not an authorization one, hence no
// missing_permission field (contrast auditToolDenied above).
func (s *Service) auditRateLimitHit(ctx context.Context, p *rbac.Principal, conn db.Connector, tool string) {
	s.recordAudit(ctx, auditlog.ActionMCPRateLimitHit, p, map[string]any{
		"connector_id": conn.ID.String(),
		"tool":         tool,
	})
}

// recordAudit is best-effort, exactly like auditlog.Service.Record itself:
// a marshal or write failure is logged and swallowed, never propagated —
// an MCP call must never fail because its own audit trail couldn't be
// written (CLAUDE.md, "Audit-log writes are best-effort"). metadata is
// map[string]any (widened from map[string]string in step 7) so a value can
// be an int (duration_ms, row_count) or a []string (filter_columns), not
// just a string — every value is still either a small scalar or a name
// list, never a whole struct or a filter's actual value.
func (s *Service) recordAudit(ctx context.Context, action string, p *rbac.Principal, metadata map[string]any) {
	if s.audit == nil {
		return
	}
	metaBytes, err := json.Marshal(metadata)
	if err != nil {
		if s.log != nil {
			s.log.Error("failed to marshal mcp audit metadata", "error", err, "action", action)
		}
		return
	}
	userID := p.UserID
	orgID := p.OrganizationID
	s.audit.Record(ctx, action, &userID, &orgID, metaBytes)
}
