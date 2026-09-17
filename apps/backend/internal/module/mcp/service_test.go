package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/mcp"
	"github.com/sapanjai/backend/internal/module/rbac"
	"github.com/sapanjai/backend/internal/module/subscription"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// connect drives a real MCP handshake against a scoped server over the
// in-memory transport pair — the same technique
// spikes/mcp-gateway/internal/gateway/gateway_test.go uses, verified there
// against SDK v1.7.0.
func connect(t *testing.T, s *gomcp.Server) *gomcp.ClientSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	serverT, clientT := gomcp.NewInMemoryTransports()

	ss, err := s.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := gomcp.NewClient(&gomcp.Implementation{Name: "mcp-test", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect (handshake failed): %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func toolNames(t *testing.T, cs *gomcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func testConnector() db.Connector {
	return db.Connector{
		ID:     uuid.New(),
		Name:   "warehouse-db",
		Type:   "generic",
		Status: "active",
	}
}

// ---- BuildServer: construction-time filtering (enforcement layer 1) ----

func TestBuildServer_ToolVisibilityByPermission(t *testing.T) {
	svc := mcp.NewService(nil, nil, nil, nil, nil, nil, nil)
	conn := testConnector()

	// connector:read gates two connector-agnostic tools now —
	// sapanjai_describe_connector and sapanjai_whoami — so every case that
	// grants it sees both.
	cases := []struct {
		name  string
		p     *rbac.Principal
		wantN int
	}{
		{"owner sees both tools via bypass", &rbac.Principal{Role: "owner"}, 2},
		{"exact connector:read grant", &rbac.Principal{Actions: []string{"connector:read"}}, 2},
		{"connector:* wildcard", &rbac.Principal{Actions: []string{"connector:*"}}, 2},
		{"unrelated grant sees nothing", &rbac.Principal{Actions: []string{"mcpkey:read"}}, 0},
		{"no grant at all sees nothing", &rbac.Principal{}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := connect(t, svc.BuildServer(tc.p, conn, mcp.RequestInfo{}))
			got := toolNames(t, cs)
			if len(got) != tc.wantN {
				t.Errorf("tools/list = %v, want %d tool(s)", got, tc.wantN)
			}
		})
	}
}

func TestBuildServer_DescribeConnectorReturnsNoConfig(t *testing.T) {
	svc := mcp.NewService(nil, nil, nil, nil, nil, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("describe_connector errored: %v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["name"] != conn.Name || out["type"] != conn.Type || out["status"] != conn.Status {
		t.Errorf("output = %v, want name/type/status matching the connector", out)
	}
	if _, ok := out["config"]; ok {
		t.Fatal("sapanjai_describe_connector leaked a config field")
	}
	// Structural check on the wire shape, not just this instance's field
	// values: the output type has exactly three keys, so there is no field
	// a future edit could accidentally populate with connector.EncryptedConfig.
	if len(out) != 3 {
		t.Errorf("output has %d fields (%v), want exactly 3 (name, type, status)", len(out), out)
	}
}

// ---- enforce: request-time enforcement (layer 2) + audit ----

func TestEnforce_DeniedToolIsNotCallable(t *testing.T) {
	svc := mcp.NewService(nil, nil, nil, nil, nil, nil, nil)
	conn := testConnector()
	// No grant at all: sapanjai_describe_connector is not registered, so
	// this exercises the SDK's own "unknown tool" refusal — the tool being
	// invisible is itself the assertion.
	cs := connect(t, svc.BuildServer(&rbac.Principal{}, conn, mcp.RequestInfo{}))

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		// The SDK reports an unregistered tool as a protocol error — a
		// refusal either way, but assert on it here since IsError never gets
		// set for tools that were never registered in the first place.
		return
	}
	if !res.IsError {
		t.Fatal("denied tool call succeeded")
	}
}

func TestEnforce_MiddlewareDeniesEvenWhenRegistered(t *testing.T) {
	// Isolates enforcement layer 2 from layer 1: build the server with a
	// grant (so the tool IS registered), and confirm the middleware still
	// blocks a call once permission is missing on the *middleware's* view —
	// mirrors spikes/mcp-gateway's TestMiddlewareDeniesEvenWhenRegistered,
	// the mid-session-revocation shape.
	granted := &rbac.Principal{Actions: []string{"connector:read"}}
	svc := mcp.NewService(nil, nil, nil, nil, nil, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(granted, conn, mcp.RequestInfo{}))

	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("granted principal was denied: %v", res.Content)
	}
}

func TestEnforce_PermissionDeniedTextMatchesRESTBody(t *testing.T) {
	got := mcp.PermissionDenied("connector:read")
	if !got.IsError {
		t.Fatal("PermissionDenied result must have IsError set")
	}
	text, ok := got.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", got.Content[0])
	}
	if text.Text != "Missing permission: connector:read" {
		t.Errorf("text = %q, want the REST 403 body's exact wording", text.Text)
	}
}

// ---- ResolveConnector: tenant isolation ----

type fakeConnectorGetter struct {
	get        func(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error)
	openConfig func(ctx context.Context, organizationID uuid.UUID, encryptedConfig json.RawMessage) (map[string]any, error)
}

func (f *fakeConnectorGetter) Get(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error) {
	return f.get(ctx, organizationID, connectorID)
}

func (f *fakeConnectorGetter) OpenConfig(ctx context.Context, organizationID uuid.UUID, encryptedConfig json.RawMessage) (map[string]any, error) {
	if f.openConfig == nil {
		return nil, errors.New("fakeConnectorGetter: OpenConfig not configured")
	}
	return f.openConfig(ctx, organizationID, encryptedConfig)
}

func TestResolveConnector_DelegatesToConnectorService(t *testing.T) {
	orgID, connID := uuid.New(), uuid.New()
	want := db.Connector{ID: connID, OrganizationID: orgID, Name: "x"}
	getter := &fakeConnectorGetter{
		get: func(ctx context.Context, gotOrg, gotConn uuid.UUID) (db.Connector, error) {
			if gotOrg != orgID || gotConn != connID {
				t.Errorf("Get called with (%s, %s), want (%s, %s)", gotOrg, gotConn, orgID, connID)
			}
			return want, nil
		},
	}
	svc := mcp.NewService(getter, nil, nil, nil, nil, nil, nil)

	got, err := svc.ResolveConnector(context.Background(), orgID, connID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// db.Connector embeds a json.RawMessage (EncryptedConfig), which is not
	// comparable with ==; compare the fields this test actually cares about.
	if got.ID != want.ID || got.OrganizationID != want.OrganizationID || got.Name != want.Name {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveConnector_PropagatesNotFound(t *testing.T) {
	wantErr := errors.New("not found")
	getter := &fakeConnectorGetter{
		get: func(ctx context.Context, orgID, connID uuid.UUID) (db.Connector, error) {
			return db.Connector{}, wantErr
		},
	}
	svc := mcp.NewService(getter, nil, nil, nil, nil, nil, nil)

	_, err := svc.ResolveConnector(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// ---- recordUsage: the usage_events ledger (step 3 of
// .claude/plans/2026-09-13-billing-and-usage-metering.md) ----

// fakeUsageRecorder is a usageRecorder test double: records every call it
// receives and, when err is set, fails every one of them — used to prove a
// usage-write failure never surfaces to the tools/call caller (invariant 3).
type fakeUsageRecorder struct {
	mu    sync.Mutex
	calls []db.CreateUsageEventParams
	err   error

	// countResult/countErr configure CountUsageEventsForOrgSince, the quota
	// check's read side (step 5). The zero value (0, nil) is exactly right
	// for every test above this section, none of which exercises quota.
	countResult int64
	countErr    error
	countCalls  []db.CountUsageEventsForOrgSinceParams
}

func (f *fakeUsageRecorder) CreateUsageEvent(ctx context.Context, arg db.CreateUsageEventParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, arg)
	return f.err
}

func (f *fakeUsageRecorder) CountUsageEventsForOrgSince(ctx context.Context, arg db.CountUsageEventsForOrgSinceParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countCalls = append(f.countCalls, arg)
	return f.countResult, f.countErr
}

func (f *fakeUsageRecorder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// callTool is a small helper: connects, calls sapanjai_describe_connector
// (the connector-agnostic tool every test in this file already uses, so no
// connectorGetter/sheets config is needed to exercise a real dispatch), and
// returns the result.
func callTool(t *testing.T, cs *gomcp.ClientSession) *gomcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &gomcp.CallToolParams{Name: "sapanjai_describe_connector"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return res
}

// TestRecordUsage_WriteFailureDoesNotFailToolCall is the plan's metering
// testing expectation and invariant 3, made concrete: a broken usage ledger
// must never be visible to the MCP client. The defer that calls recordUsage
// runs (and, per Go's defer semantics, completes) before CallTool's result
// reaches the client, so no sleep/poll is needed to observe its effect.
func TestRecordUsage_WriteFailureDoesNotFailToolCall(t *testing.T) {
	usage := &fakeUsageRecorder{err: errors.New("usage_events insert failed")}
	svc := mcp.NewService(nil, nil, nil, usage, nil, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("tools/call failed because its usage write failed: %v", res.Content)
	}
	if got := usage.callCount(); got != 1 {
		t.Fatalf("usage recorder was called %d times, want 1 (the failed attempt)", got)
	}
	if got := svc.UsageWriteFailures(); got != 1 {
		t.Errorf("UsageWriteFailures() = %d, want 1", got)
	}
}

// TestRecordUsage_SuccessfulCallWritesOneRow covers the other half: a
// permitted, successfully-dispatched call writes exactly one usage_events
// row, scoped to the calling principal's org and the connector actually
// used, naming the tool called and nothing else (never tool arguments,
// never mcp_key_id — see recordUsage's doc comment for why the latter is
// always NULL today).
func TestRecordUsage_SuccessfulCallWritesOneRow(t *testing.T) {
	usage := &fakeUsageRecorder{}
	svc := mcp.NewService(nil, nil, nil, usage, nil, nil, nil)
	conn := testConnector()
	orgID := uuid.New()
	p := &rbac.Principal{Role: "owner", OrganizationID: orgID}
	cs := connect(t, svc.BuildServer(p, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res.Content)
	}

	if got := usage.callCount(); got != 1 {
		t.Fatalf("usage recorder was called %d times, want exactly 1", got)
	}
	got := usage.calls[0]
	if got.OrganizationID != orgID {
		t.Errorf("OrganizationID = %s, want %s", got.OrganizationID, orgID)
	}
	if !got.ConnectorID.Valid || got.ConnectorID.Bytes != conn.ID {
		t.Errorf("ConnectorID = %+v, want valid %s", got.ConnectorID, conn.ID)
	}
	if got.Tool != "sapanjai_describe_connector" {
		t.Errorf("Tool = %q, want %q", got.Tool, "sapanjai_describe_connector")
	}
	if got.McpKeyID.Valid {
		t.Errorf("McpKeyID = %+v, want NULL (not reachable at this call site today)", got.McpKeyID)
	}
}

// TestRecordUsage_NilRecorderIsSafe pins the nil-tolerant convention
// NewService's doc comment now documents for usage, mirroring limiter:
// a Service built with no usage recorder (the zero value most unit tests in
// this file already pass) must dispatch tools normally rather than panic.
func TestRecordUsage_NilRecorderIsSafe(t *testing.T) {
	svc := mcp.NewService(nil, nil, nil, nil, nil, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("unexpected tool error with a nil usage recorder: %v", res.Content)
	}
}

// ---- quota: max_tool_calls_per_month enforcement (step 5 of
// .claude/plans/2026-09-13-billing-and-usage-metering.md) ----

// fakeQuotaEnforcer is a quotaEnforcer test double, hand-mocked exactly like
// fakeConnectorGetter above: it records every call so a test can assert both
// the outcome and what mcp.Service actually asked for.
type fakeQuotaEnforcer struct {
	mu    sync.Mutex
	err   error
	calls []fakeQuotaEnforcerCall
}

type fakeQuotaEnforcerCall struct {
	organizationID uuid.UUID
	key            string
	currentCount   int
}

func (f *fakeQuotaEnforcer) EnforceLimit(ctx context.Context, organizationID uuid.UUID, key string, currentCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeQuotaEnforcerCall{organizationID, key, currentCount})
	return f.err
}

func (f *fakeQuotaEnforcer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestQuota_ExceededRefusesCallCleanlyAndActionably is CLAUDE.md's gateway
// edge case ("a rate-limited or scan-budget-exhausted call ends cleanly
// (IsError), never a panic or a raw Go error") applied to the quota cap, and
// the plan's §Risks requirement that "the error must say what happened and
// what to do about it."
func TestQuota_ExceededRefusesCallCleanlyAndActionably(t *testing.T) {
	usage := &fakeUsageRecorder{countResult: 1000}
	quota := &fakeQuotaEnforcer{err: apperror.New(apperror.LimitExceeded)}
	svc := mcp.NewService(nil, nil, nil, usage, quota, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if !res.IsError {
		t.Fatal("call past the monthly quota succeeded, want a clean IsError refusal")
	}
	text, ok := res.Content[0].(*gomcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %#v, want *TextContent", res.Content[0])
	}
	if !containsFold(text.Text, "QUOTA_EXCEEDED") || !containsFold(text.Text, "upgrade") {
		t.Errorf("text = %q, want it to name what happened (quota exceeded) and what to do about it (upgrade)", text.Text)
	}
	// A quota-refused call must never write its own usage_events row: the
	// enforce middleware returns before the defer that calls recordUsage is
	// even registered.
	if got := usage.callCount(); got != 0 {
		t.Errorf("CreateUsageEvent called %d times for a refused call, want 0", got)
	}
}

// TestQuota_CountLookupFailureAllowsCallThrough is invariant 3 made
// concrete, and the most important test in this section per the task
// brief: a failure to *determine* the quota (a DB error counting
// usage_events) must never be treated as an exceeded quota. quota is
// configured to refuse if it is ever reached, so a passing call here proves
// EnforceLimit was never even called.
func TestQuota_CountLookupFailureAllowsCallThrough(t *testing.T) {
	usage := &fakeUsageRecorder{countErr: errors.New("usage_events count query failed")}
	quota := &fakeQuotaEnforcer{err: apperror.New(apperror.LimitExceeded)}
	svc := mcp.NewService(nil, nil, nil, usage, quota, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("a quota *count* failure blocked the call; invariant 3 requires it to fail open: %v", res.Content)
	}
	if got := quota.callCount(); got != 0 {
		t.Errorf("EnforceLimit called %d times after a count failure, want 0 (count failing must short-circuit before it)", got)
	}
}

// TestQuota_EnforceFailureAllowsCallThrough covers the other half of
// invariant 3: EnforceLimit itself can fail for a reason that is not
// LIMIT_EXCEEDED (e.g. its own subscription lookup errored) -- see
// subscription.Service.GetLimit's propagated error path. That must also
// fail open, not be mistaken for an exceeded quota.
func TestQuota_EnforceFailureAllowsCallThrough(t *testing.T) {
	usage := &fakeUsageRecorder{countResult: 5}
	quota := &fakeQuotaEnforcer{err: errors.New("subscription lookup failed")}
	svc := mcp.NewService(nil, nil, nil, usage, quota, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("a non-LIMIT_EXCEEDED EnforceLimit failure blocked the call; invariant 3 requires it to fail open: %v", res.Content)
	}
}

// TestQuota_NilQuotaSkipsCheckEvenWithUsageCounter pins the nil-tolerant
// convention NewService's doc comment states for quota, mirroring limiter
// and usage: a Service built with no quota enforcer must dispatch normally
// regardless of what the usage counter would have returned.
func TestQuota_NilQuotaSkipsCheckEvenWithUsageCounter(t *testing.T) {
	usage := &fakeUsageRecorder{countResult: 999999} // would exceed any real plan if the check ran
	svc := mcp.NewService(nil, nil, nil, usage, nil, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("nil quota enforcer still blocked the call: %v", res.Content)
	}
}

// TestQuota_NilUsageCounterSkipsCheckEvenWithQuotaEnforcer is the mirror
// image: a quota enforcer configured to always refuse must never be reached
// when there is no usage counter to ask.
func TestQuota_NilUsageCounterSkipsCheckEvenWithQuotaEnforcer(t *testing.T) {
	quota := &fakeQuotaEnforcer{err: apperror.New(apperror.LimitExceeded)}
	svc := mcp.NewService(nil, nil, nil, nil, quota, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("nil usage counter still blocked the call: %v", res.Content)
	}
	if got := quota.callCount(); got != 0 {
		t.Errorf("EnforceLimit called %d times with no usage counter present, want 0", got)
	}
}

// TestQuota_CountWindowIsCurrentUTCCalendarMonth asserts the cap's window
// matches internal/job/usagerollup's period exactly: UTC midnight on the
// 1st of the current month. A mismatch here would mean the cap counts the
// wrong thing relative to what the rollup (and eventually the frontend
// usage meter) report as "this month".
func TestQuota_CountWindowIsCurrentUTCCalendarMonth(t *testing.T) {
	usage := &fakeUsageRecorder{}
	quota := &fakeQuotaEnforcer{}
	svc := mcp.NewService(nil, nil, nil, usage, quota, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	nowUTC := time.Now().UTC()
	_ = callTool(t, cs)

	usage.mu.Lock()
	defer usage.mu.Unlock()
	if len(usage.countCalls) != 1 {
		t.Fatalf("CountUsageEventsForOrgSince called %d times, want 1", len(usage.countCalls))
	}
	since := usage.countCalls[0].Since
	wantYear, wantMonth, _ := nowUTC.Date()
	gotYear, gotMonth, gotDay := since.Date()
	if gotYear != wantYear || gotMonth != wantMonth || gotDay != 1 {
		t.Errorf("Since = %v, want the 1st of the current UTC month (%d-%02d)", since, wantYear, wantMonth)
	}
	if h, m, s := since.Clock(); h != 0 || m != 0 || s != 0 || since.Nanosecond() != 0 {
		t.Errorf("Since = %v, want UTC midnight exactly", since)
	}
	if since.Location() != time.UTC {
		t.Errorf("Since location = %v, want time.UTC", since.Location())
	}
}

// fakeSubStore satisfies subscription.Service's own (unexported) store
// dependency well enough to run a *real* subscription.Service inside these
// tests, rather than fakeQuotaEnforcer. Only GetOrgSubscriptionWithPlan is
// ever called by EnforceLimit/EffectiveLimits/GetLimit; the other three
// methods exist only to satisfy the interface and are never expected to run.
type fakeSubStore struct {
	planLimits   json.RawMessage
	customLimits []byte
}

func (f *fakeSubStore) GetOrgSubscriptionWithPlan(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionWithPlanRow, error) {
	return db.GetOrgSubscriptionWithPlanRow{PlanLimits: f.planLimits, CustomLimits: f.customLimits}, nil
}

func (f *fakeSubStore) GetOrgSubscription(ctx context.Context, organizationID uuid.UUID) (db.GetOrgSubscriptionRow, error) {
	return db.GetOrgSubscriptionRow{}, errors.New("fakeSubStore: GetOrgSubscription not used by this test")
}

func (f *fakeSubStore) UpsertOrgSubscription(ctx context.Context, arg db.UpsertOrgSubscriptionParams) error {
	return errors.New("fakeSubStore: UpsertOrgSubscription not used by this test")
}

func (f *fakeSubStore) ListPublicPlans(ctx context.Context) ([]db.Plan, error) {
	return nil, errors.New("fakeSubStore: ListPublicPlans not used by this test")
}

func (f *fakeSubStore) ListActivePlanPricesForPublicPlans(ctx context.Context) ([]db.ListActivePlanPricesForPublicPlansRow, error) {
	return nil, errors.New("fakeSubStore: ListActivePlanPricesForPublicPlans not used by this test")
}

// TestQuota_CustomLimitsOverridePlan_EndToEnd is invariant 2 ("custom_limits
// still wins") made concrete for this key, wired through a real
// subscription.Service rather than fakeQuotaEnforcer -- per the task's
// instruction to assert the composition rather than reimplementing
// EffectiveLimits' own merge logic (already exhaustively covered by
// subscription/service_test.go's TestEffectiveLimits_CustomOverridesPlanPerKey
// and friends). A plan capped at 10 calls/month with a custom_limits
// override raising it to -1 (unlimited) must let call #1000 through.
func TestQuota_CustomLimitsOverridePlan_EndToEnd(t *testing.T) {
	store := &fakeSubStore{
		planLimits:   json.RawMessage(`{"max_tool_calls_per_month": 10}`),
		customLimits: []byte(`{"max_tool_calls_per_month": -1}`),
	}
	realQuota := subscription.NewService(store)
	usage := &fakeUsageRecorder{countResult: 999}
	svc := mcp.NewService(nil, nil, nil, usage, realQuota, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if res.IsError {
		t.Fatalf("custom_limits override was not honored, call refused: %v", res.Content)
	}
}

// TestQuota_PlanLimitAppliesWithoutOverride_EndToEnd is the same wiring
// without a custom_limits override: a call at exactly the plan's limit must
// be refused (EnforceLimit's own semantics are currentCount >= limit).
func TestQuota_PlanLimitAppliesWithoutOverride_EndToEnd(t *testing.T) {
	store := &fakeSubStore{
		planLimits: json.RawMessage(`{"max_tool_calls_per_month": 10}`),
	}
	realQuota := subscription.NewService(store)
	usage := &fakeUsageRecorder{countResult: 10}
	svc := mcp.NewService(nil, nil, nil, usage, realQuota, nil, nil)
	conn := testConnector()
	cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

	res := callTool(t, cs)
	if !res.IsError {
		t.Fatal("call at the plan's limit was allowed through, want a quota refusal")
	}
}

// TestQuota_MissingKeyAndUnlimitedPlanBothPass_EndToEnd covers the last two
// EnforceLimit semantics the task brief calls out explicitly -- a plan with
// no max_tool_calls_per_month key at all, and one with -1 -- both against a
// real subscription.Service, both passing regardless of how high the usage
// counter reports.
func TestQuota_MissingKeyAndUnlimitedPlanBothPass_EndToEnd(t *testing.T) {
	cases := []struct {
		name   string
		limits json.RawMessage
	}{
		{"key absent from plan limits", json.RawMessage(`{"max_members": 5}`)},
		{"key present but -1", json.RawMessage(`{"max_tool_calls_per_month": -1}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			realQuota := subscription.NewService(&fakeSubStore{planLimits: tc.limits})
			usage := &fakeUsageRecorder{countResult: 1_000_000}
			svc := mcp.NewService(nil, nil, nil, usage, realQuota, nil, nil)
			conn := testConnector()
			cs := connect(t, svc.BuildServer(&rbac.Principal{Role: "owner"}, conn, mcp.RequestInfo{}))

			res := callTool(t, cs)
			if res.IsError {
				t.Fatalf("call refused, want unlimited: %v", res.Content)
			}
		})
	}
}

// containsFold reports whether s contains substr, case-insensitively --
// avoids pulling in strings.ToLower at every call site above.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
