// Package mcp implements the MCP gateway: one Streamable HTTP endpoint per
// connector (POST /mcp/:connectorId) exposing an RBAC-filtered tool catalog
// to an authenticated PAT holder.
//
// The pattern is ported from spikes/mcp-gateway, which is a separate module
// deliberately outside this build — never import it.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/adapter/googlesheets"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/module/rbac"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// toolCallCost is the floor every tools/call charges against its connector's
// bucket. The bucket counts upstream Google API requests, but a tool making
// none is still worth limiting. Adapters that issue N upstream requests
// charge N themselves via Service.ChargeRateLimit, per page, mid-execution.
const toolCallCost = 1

// maxToolCallsPerMonthLimitKey is the plans.limits key gating tool-call
// volume — step 5 of .claude/plans/2026-09-13-billing-and-usage-metering.md,
// decision 1: a tool call is a cap, not a charge. Mirrors
// connector.Service's maxConnectorsLimitKey. An org whose plan omits it, or
// whose org has no subscription at all, is unlimited
// (subscription.Service.EnforceLimit's existing semantics — no new ones
// added here).
const maxToolCallsPerMonthLimitKey = "max_tool_calls_per_month"

// ServerName and ServerVersion identify this gateway to clients during the
// initialize handshake.
const (
	ServerName    = "sapanjai-mcp-gateway"
	ServerVersion = "0.1.0"
)

// connectorGetter is the subset of *connector.Service this depends on,
// narrowed so unit tests can hand-mock it. Get scopes by organization_id and
// returns apperror.NotFound for another org's connector, indistinguishable
// from one that never existed. OpenConfig is how tool handlers reach a
// connector's live config on every invocation, never a cached copy.
type connectorGetter interface {
	Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error)
	OpenConfig(ctx context.Context, organizationID uuid.UUID, encryptedConfig json.RawMessage) (map[string]any, error)
}

var _ connectorGetter = (*connector.Service)(nil)

// rateLimiter is the subset of *appredis.RateLimiter this depends on,
// narrowed so unit tests can mock it without importing infra/redis.
type rateLimiter interface {
	Take(ctx context.Context, connectorID string, n int) (allowed bool, retryAfter time.Duration, err error)
}

// usageRecorder is the subset of *database.Store this depends on, narrowed
// so unit tests can hand-mock it. No usage-ledger service exists to narrow
// instead (unlike connectorGetter/rateLimiter), so *database.Store is what
// gets narrowed here. CountUsageEventsForOrgSince is the quota check's read
// side (step 5); CreateUsageEvent is the metering write side (step 3) —
// grouped in one interface because both back onto the same usage_events
// table and the same injected *database.Store.
type usageRecorder interface {
	CreateUsageEvent(ctx context.Context, arg db.CreateUsageEventParams) error
	CountUsageEventsForOrgSince(ctx context.Context, arg db.CountUsageEventsForOrgSinceParams) (int64, error)
}

var _ usageRecorder = (*database.Store)(nil)

// quotaEnforcer is the subset of *subscription.Service the quota check
// depends on, narrowed exactly like connector.Service's limitEnforcer — same
// shape, same reasoning: EnforceLimit already treats a missing subscription,
// a missing limit key, or -1 as unlimited, so this package adds no new
// semantics on top of it.
type quotaEnforcer interface {
	EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error
}

var _ quotaEnforcer = (*subscription.Service)(nil)

// Service builds per-request *mcp.Server instances scoped to one
// authenticated principal and one resolved connector, wiring both
// enforcement layers plus best-effort audit writes.
type Service struct {
	connectors connectorGetter
	limiter    rateLimiter
	audit      *auditlog.Service
	usage      usageRecorder
	quota      quotaEnforcer
	log        *slog.Logger

	// sheetsTokens holds one OAuth TokenSource per connector id, shared
	// across every request — see googlesheets.TokenSourceCache.
	sheetsTokens *googlesheets.TokenSourceCache

	// fileLinkKey signs and verifies drive_get_file download links. nil when
	// masterKey was empty, which both call sites treat as "disabled".
	fileLinkKey []byte

	// usageWriteFailures counts failed usage_events inserts since process
	// start (never reset on read), for a future health/metrics surface and
	// for tests to assert a failure was counted, not just logged.
	usageWriteFailures atomic.Int64
}

// NewService builds an mcp Service. limiter may be nil (unit tests), which
// means no rate limiting rather than a nil-pointer panic; usage and quota
// follow the same nil-tolerant convention, meaning no usage recording and no
// quota enforcement respectively. Neither billing nor rate limiting may ever
// be a hard dependency of building a Service. An empty masterKey disables
// file-link minting rather than signing under an empty key.
func NewService(connectors connectorGetter, limiter rateLimiter, audit *auditlog.Service, usage usageRecorder, quota quotaEnforcer, log *slog.Logger, masterKey []byte) *Service {
	return &Service{
		connectors:   connectors,
		limiter:      limiter,
		audit:        audit,
		usage:        usage,
		quota:        quota,
		log:          log,
		sheetsTokens: googlesheets.NewTokenSourceCache(),
		fileLinkKey:  deriveFileLinkKey(masterKey),
	}
}

// currentBillingPeriodStart returns the start of the current UTC calendar
// month — the same boundary internal/job/usagerollup buckets on
// (date_trunc('month', occurred_at)), so the quota check below counts
// exactly the window the rollup and the frontend usage meter will later
// agree is "this month". Counting usage_events directly for this window
// (rather than reading usage_rollups) costs one indexed query
// (idx_usage_events_organization_id_occurred_at, migration 00014) per
// tools/call, on the gateway's latency path — accepted because the
// alternative, reading the rollup, is stale by up to USAGE_ROLLUP_INTERVAL
// for the current, still-open month and would let a customer overshoot by a
// whole interval's traffic. A cached/buffered counter is explicitly
// deferred per the plan's §Risks ("measure before building it").
func currentBillingPeriodStart() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// UsageWriteFailures reports how many usage_events inserts have failed since
// process start. Read-only; recordUsage is the sole writer.
func (s *Service) UsageWriteFailures() int64 {
	return s.usageWriteFailures.Load()
}

// openGoogleSheetsConfig decrypts conn's stored config and parses it as a
// google_sheets Config. Called fresh on every tool invocation, never cached,
// so a narrowed allowlist takes effect on the very next call.
// conn.OrganizationID is trusted: ResolveConnector already scoped the lookup.
func (s *Service) openGoogleSheetsConfig(ctx context.Context, conn db.Connector) (*googlesheets.Config, error) {
	raw, err := s.connectors.OpenConfig(ctx, conn.OrganizationID, conn.EncryptedConfig)
	if err != nil {
		return nil, err
	}
	return googlesheets.ParseConfig(raw)
}

// ChargeRateLimit charges n units against connectorID's upstream-request
// budget directly, bypassing enforce's dispatch-time floor. A tool handler
// making N upstream calls charges once per page, mid-execution, instead of
// paying a single unit up front. Satisfies googlesheets.RateCharger.
func (s *Service) ChargeRateLimit(ctx context.Context, connectorID uuid.UUID, n int) (allowed bool, retryAfter time.Duration, err error) {
	if s.limiter == nil {
		return true, 0, nil
	}
	return s.limiter.Take(ctx, connectorID.String(), n)
}

// ResolveConnector fetches connectorID scoped to organizationID — always the
// authenticated principal's own org (from RequireMCPKey), never a value read
// off the request. Another org's connector yields apperror.NotFound, the
// same code a nonexistent id produces.
func (s *Service) ResolveConnector(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	return s.connectors.Get(ctx, organizationID, connectorID)
}

// BuildServer returns an *mcp.Server exposing only the catalog tools p is
// permitted to use against conn. Building a fresh server per request is
// cheap (no I/O, no goroutines) and is what makes a per-tenant tool surface
// possible in Stateless mode.
//
// Two enforcement layers, on purpose: an unpermitted tool is never
// registered here, so it never reaches tools/list; Service.enforce re-checks
// at call time, which is what matters once permissions change mid-session or
// a client replays a stale tool list.
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
			e.Register(srv, s, p, conn, req)
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

// enforce is the request-time enforcement layer and the audit hook. Never
// treat tools/list as an authorization boundary: this is the check that
// still holds when permissions change mid-session or a client replays a
// cached tool list.
func (s *Service) enforce(p *rbac.Principal, conn db.Connector) gomcp.Middleware {
	return func(next gomcp.MethodHandler) gomcp.MethodHandler {
		return func(ctx context.Context, method string, req gomcp.Request) (result gomcp.Result, err error) {
			switch method {
			case "initialize", "server/discover":
				// "Session" means one handshake here — Stateless mode is one
				// HTTP POST per JSON-RPC call, with no long-lived connection.
				//
				// Both methods, not one: clients on protocol ≥2026-07-28
				// (SEP-2575) send server/discover instead of initialize, and
				// this gateway always advertises that range. Older clients
				// still send initialize, so mcp.session.started needs both to
				// be reliable across client vintages.
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
				// Quota check runs after the permission check (an
				// unpermitted call should never spend budget — same
				// principle the rate-limit check below states) and before
				// it, so a call refused for quota never spends rate-limit
				// budget either: quota is the "can this org call at all
				// this month" gate, rate-limiting is "how fast can it call
				// right now", and the cheaper, less contended check goes
				// first.
				if s.quota != nil && s.usage != nil {
					count, countErr := s.usage.CountUsageEventsForOrgSince(ctx, db.CountUsageEventsForOrgSinceParams{
						OrganizationID: p.OrganizationID,
						Since:          currentBillingPeriodStart(),
					})
					if countErr != nil {
						// Invariant 3 ("billing never blocks the gateway")
						// cuts the OPPOSITE way from the rate limiter's
						// infra-failure branch just below: that check
						// protects Google's API from us, so it fails
						// closed. This one protects our own revenue
						// bookkeeping, and a failure to *determine* the
						// quota (a DB error counting usage) is not a
						// genuinely exhausted quota — it is a billing
						// bookkeeping failure, and CLAUDE.md/invariant 3
						// forbid that from ever taking down a customer's
						// data access. So: log loudly, then let the call
						// through.
						if s.log != nil {
							s.log.Error("mcp quota count failed; allowing call through", "error", countErr,
								"organization_id", p.OrganizationID, "connector_id", conn.ID, "tool", params.Name)
						}
					} else if enforceErr := s.quota.EnforceLimit(ctx, p.OrganizationID, maxToolCallsPerMonthLimitKey, int(count)); enforceErr != nil {
						var appErr *apperror.Error
						if errors.As(enforceErr, &appErr) && appErr.Code == apperror.LimitExceeded {
							// The quota is genuinely exhausted: this is the
							// one branch that actually refuses the call.
							s.auditQuotaExceeded(ctx, p, conn, params.Name)
							return QuotaExceeded(), nil
						}
						// EnforceLimit's own lookup failed for a reason
						// other than "limit reached" (e.g. the
						// subscription/plan read errored). Same fail-open
						// reasoning as the count failure above: a failure to
						// determine the quota is not an exceeded quota.
						if s.log != nil {
							s.log.Error("mcp quota enforcement failed; allowing call through", "error", enforceErr,
								"organization_id", p.OrganizationID, "connector_id", conn.ID, "tool", params.Name)
						}
					}
				}
				// Rate-limit check runs after the permission check (an
				// unpermitted call should never spend budget) and before
				// dispatch — landed here, ahead of any real adapter, so no
				// tool ever ships able to reach a Google API unguarded
				// (docs/07-sheets-adapter-decisions.md step 4). toolCallCost is
				// the dispatch-time floor; a real adapter charges its own
				// per-upstream-request cost mid-execution via
				// Service.ChargeRateLimit instead of relying solely on
				// this floor.
				if s.limiter != nil {
					allowed, retryAfter, limitErr := s.limiter.Take(ctx, conn.ID.String(), toolCallCost)
					if limitErr != nil {
						// An infra failure is not an exhausted bucket: fail
						// closed with a readable IsError rather than admitting
						// an unmetered call or aborting the turn with a
						// protocol error the model can't act on.
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
				// Audited from a defer, after dispatch: row_count and
				// duration_ms don't exist until the handler has run, and both
				// belong in the same row. The defer runs during unwind, ahead
				// of Echo's Recover, so a panicking handler still audits.
				fields := auditableToolFields(params.Arguments)
				start := time.Now()
				// WithoutCancel because this now sits on the far side of a
				// scan that can run for seconds: a client hanging up mid-scan
				// cancels ctx, and Record writes synchronously on it, so the
				// row would never land for exactly the longest calls.
				auditCtx := context.WithoutCancel(ctx)
				defer func() {
					s.auditToolCalled(auditCtx, p, conn, params.Name, time.Since(start), fields, result)
					// Not filtered by result.IsError: the rollup job's drift
					// check compares this table's count against audit_logs
					// for the same window, so the two must record the same
					// set of calls or the drift number means nothing.
					s.recordUsage(auditCtx, p, conn, params.Name)
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

// The audit* helpers all funnel through recordAudit. Every metadata map is
// deliberately tiny and explicit — never the request envelope, never a tool
// argument's value, which could be business data such as a cell or a partner
// name. Column names are recorded; the values filtered on are not.

func (s *Service) auditSessionStarted(ctx context.Context, p *rbac.Principal, conn db.Connector) {
	s.recordAudit(ctx, auditlog.ActionMCPSessionStarted, p, map[string]any{
		"connector_id": conn.ID.String(),
	})
}

// auditToolCalled records mcp.tool.called once a permitted call has finished
// dispatching. row_count is added only when the tool's own StructuredContent
// carries it, so a tool without one omits the field rather than reporting a
// misleading 0.
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

// rowCountFromResult peeks a completed call's StructuredContent for a
// top-level "count" (sheets_query_rows, a match count) or "row_count"
// (sheets_read_range). Only ever pulls out that one integer — never "rows"
// or "cells" — so no cell value can reach an audit row through here.
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

// auditableToolFields extracts the fixed allowlist of argument keys ever
// copied into audit metadata — which resource a call touched, and the column
// names a query filtered on, never a filter's value. An allowlist, not "log
// the whole arguments object": CLAUDE.md's "log individual fields, never a
// whole request struct" applies to tool arguments as much as to HTTP bodies.
// Unknown or malformed arguments yield no fields rather than an error.
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
			// Value is deliberately never decoded here — column names only.
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

// auditRateLimitHit records mcp.ratelimit.hit when a call is refused for an
// exhausted bucket — a quota failure, not an authorization one, hence no
// missing_permission field.
func (s *Service) auditRateLimitHit(ctx context.Context, p *rbac.Principal, conn db.Connector, tool string) {
	s.recordAudit(ctx, auditlog.ActionMCPRateLimitHit, p, map[string]any{
		"connector_id": conn.ID.String(),
		"tool":         tool,
	})
}

// auditQuotaExceeded records mcp.quota.exceeded when a call is refused
// because its organization has used its full max_tool_calls_per_month quota
// for the current billing month — a distinct quota failure from
// mcp.ratelimit.hit; see ActionMCPQuotaExceeded's doc comment for why the
// two are not folded into one action.
func (s *Service) auditQuotaExceeded(ctx context.Context, p *rbac.Principal, conn db.Connector, tool string) {
	s.recordAudit(ctx, auditlog.ActionMCPQuotaExceeded, p, map[string]any{
		"connector_id": conn.ID.String(),
		"tool":         tool,
	})
}

// recordUsage writes one usage_events row for a tools/call the enforce
// middleware let through — the metering counterpart to auditToolCalled,
// called from the same defer so both ledgers cover the same set of calls.
//
// Deliberately NOT best-effort the way recordAudit/auditlog.Service.Record
// is: that asymmetry is why usage_events exists as its own ledger instead
// of being folded into audit_logs — a silently dropped audit row is fine, a
// silently dropped usage row is unrecorded revenue. A failure logs at
// error (one level louder than recordAudit's) and bumps
// usageWriteFailures, but still never returns an error, panics, or touches
// the tools/call result — invariant 3 forbids billing from ever blocking
// the gateway.
//
// mcp_key_id is always left NULL: the authenticated PAT's row id is not
// reachable here today. rbac.Principal's doc comment says it "must never
// grow a credential field", and RequestInfo.KeyName's doc comment says it
// carries the key's display name "never the key id, hash, or token" — both
// pre-existing, deliberate exclusions this step chooses not to fight rather
// than an oversight to route around.
func (s *Service) recordUsage(ctx context.Context, p *rbac.Principal, conn db.Connector, tool string) {
	if s.usage == nil {
		return
	}
	err := s.usage.CreateUsageEvent(ctx, db.CreateUsageEventParams{
		OrganizationID: p.OrganizationID,
		ConnectorID:    pgtype.UUID{Bytes: conn.ID, Valid: true},
		Tool:           tool,
	})
	if err != nil {
		s.usageWriteFailures.Add(1)
		if s.log != nil {
			s.log.Error("failed to record usage event", "error", err,
				"organization_id", p.OrganizationID, "connector_id", conn.ID, "tool", tool)
		}
	}
}

// recordAudit is best-effort: a marshal or write failure is logged and
// swallowed, never propagated. An MCP call must not fail because its own
// audit row couldn't be written. Values stay small scalars or name lists —
// never a whole struct, never a filter's value.
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
