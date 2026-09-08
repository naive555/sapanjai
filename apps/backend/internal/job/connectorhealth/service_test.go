package connectorhealth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/email"
	"github.com/sapanjai/backend/internal/worker"
)

const testAppURL = "http://localhost:4000"

// ---- hand-mocked healthStore ----

type mockStore struct {
	mu sync.Mutex

	listRows []db.ListConnectorsForHealthCheckRow
	listErr  error

	owners   map[uuid.UUID]db.GetOrganizationOwnerRow
	ownerErr error // returned instead of a map lookup when set

	enqueued   []db.EnqueueEmailParams
	enqueueErr error
}

func (m *mockStore) ListConnectorsForHealthCheck(_ context.Context, _ int32) ([]db.ListConnectorsForHealthCheckRow, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.listRows, nil
}

func (m *mockStore) GetOrganizationOwner(_ context.Context, organizationID uuid.UUID) (db.GetOrganizationOwnerRow, error) {
	if m.ownerErr != nil {
		return db.GetOrganizationOwnerRow{}, m.ownerErr
	}
	owner, ok := m.owners[organizationID]
	if !ok {
		return db.GetOrganizationOwnerRow{}, fmt.Errorf("mockStore: no owner configured for org %s", organizationID)
	}
	return owner, nil
}

func (m *mockStore) EnqueueEmail(_ context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enqueueErr != nil {
		return db.EmailOutbox{}, m.enqueueErr
	}
	m.enqueued = append(m.enqueued, arg)
	return db.EmailOutbox{ID: uuid.New(), ToAddress: arg.ToAddress, Subject: arg.Subject, BodyHtml: arg.BodyHtml, BodyText: arg.BodyText}, nil
}

var _ healthStore = (*mockStore)(nil)

// ---- hand-mocked healthChecker (stands in for *connector.Service) ----

type checkResult struct {
	row db.Connector
	err error
}

type mockChecker struct {
	mu      sync.Mutex
	calls   []uuid.UUID // connector ids, in call order
	results map[uuid.UUID]checkResult
}

func (m *mockChecker) CheckHealth(_ context.Context, _, connectorID uuid.UUID) (db.Connector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, connectorID)
	res, ok := m.results[connectorID]
	if !ok {
		return db.Connector{}, fmt.Errorf("mockChecker: no result configured for connector %s", connectorID)
	}
	return res.row, res.err
}

var _ healthChecker = (*mockChecker)(nil)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// realRenderer parses the actual embedded templates. Using the real
// email.Renderer, not a mock, is deliberate for this package: the property
// under test in several cases below is what the ACTUAL rendered body
// contains, and a mock that just echoes its input back would not catch a
// regression where the job started passing more than a name and a URL into
// the template data.
func realRenderer(t *testing.T) *email.Renderer {
	t.Helper()
	r, err := email.NewRenderer()
	if err != nil {
		t.Fatalf("email.NewRenderer: %v", err)
	}
	return r
}

func attrsContain(attrs []any, key string, value any) bool {
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key && attrs[i+1] == value {
			return true
		}
	}
	return false
}

// ---- tests ----

func TestJob_NameAndInterval(t *testing.T) {
	j := New(&mockStore{}, &mockChecker{}, realRenderer(t), newTestLogger(), 42*time.Minute, 50, testAppURL)

	if j.Name() != "connector-health" {
		t.Errorf("Name() = %q, want %q", j.Name(), "connector-health")
	}
	if j.Interval() != 42*time.Minute {
		t.Errorf("Interval() = %v, want 42m", j.Interval())
	}
	var _ worker.Job = j
}

// The regression that matters most: an active connector that goes error
// during this run must produce exactly one outbox row.
func TestJob_ActiveToError_EnqueuesOneEmail(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()

	store := &mockStore{
		listRows: []db.ListConnectorsForHealthCheckRow{
			{ID: connID, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusActive)},
		},
		owners: map[uuid.UUID]db.GetOrganizationOwnerRow{
			orgID: {UserID: uuid.New(), Email: "owner@example.com", DisplayName: nil},
		},
	}
	checker := &mockChecker{results: map[uuid.UUID]checkResult{
		connID: {row: db.Connector{ID: connID, OrganizationID: orgID, Name: "Prod Sheets", Type: "google_sheets", Status: string(connector.StatusError)}},
	}}

	j := New(store, checker, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.enqueued) != 1 {
		t.Fatalf("expected exactly 1 enqueued email, got %d", len(store.enqueued))
	}
	got := store.enqueued[0]
	if got.ToAddress != "owner@example.com" {
		t.Errorf("ToAddress = %q, want the org owner's address", got.ToAddress)
	}
	if got.BodyHtml == nil || !strings.Contains(*got.BodyHtml, "Prod Sheets") {
		t.Errorf("HTML body does not name the connector: %v", got.BodyHtml)
	}
	if got.BodyText == nil || !strings.Contains(*got.BodyText, "/connectors/"+connID.String()) {
		t.Errorf("text body does not link to /connectors/%s: %v", connID, got.BodyText)
	}

	if res.Affected != 1 {
		t.Errorf("Affected = %d, want 1", res.Affected)
	}
	if !attrsContain(res.Attrs, "checked", 1) {
		t.Errorf("Attrs missing checked=1: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "notified", 1) {
		t.Errorf("Attrs missing notified=1: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "failed", 0) {
		t.Errorf("Attrs missing failed=0: %v", res.Attrs)
	}
}

// The regression that matters most, the other direction: a connector that
// was ALREADY broken must not be emailed about again just because it is
// still broken. One email per outage, not one per interval.
func TestJob_ErrorToError_EnqueuesNone(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()

	store := &mockStore{
		listRows: []db.ListConnectorsForHealthCheckRow{
			{ID: connID, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusError)},
		},
		owners: map[uuid.UUID]db.GetOrganizationOwnerRow{
			orgID: {UserID: uuid.New(), Email: "owner@example.com"},
		},
	}
	checker := &mockChecker{results: map[uuid.UUID]checkResult{
		connID: {row: db.Connector{ID: connID, OrganizationID: orgID, Name: "Prod Sheets", Status: string(connector.StatusError)}},
	}}

	j := New(store, checker, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.enqueued) != 0 {
		t.Fatalf("expected 0 enqueued emails for an already-broken connector, got %d", len(store.enqueued))
	}
	if !attrsContain(res.Attrs, "notified", 0) {
		t.Errorf("Attrs missing notified=0: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "checked", 1) {
		t.Errorf("Attrs missing checked=1: %v", res.Attrs)
	}
}

// A connector whose type has no registered Checker (the "generic"
// placeholder, or any future type before its adapter lands) is not a
// failure: CheckHealth returns apperror.HealthCheckUnsupported, and it must
// be skipped quietly rather than counted as checked or failed.
func TestJob_UnsupportedType_SkippedNotCounted(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()

	store := &mockStore{
		listRows: []db.ListConnectorsForHealthCheckRow{
			{ID: connID, OrganizationID: orgID, Type: "generic", Status: string(connector.StatusInactive)},
		},
	}
	checker := &mockChecker{results: map[uuid.UUID]checkResult{
		connID: {err: apperror.New(apperror.HealthCheckUnsupported)},
	}}

	j := New(store, checker, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.enqueued) != 0 {
		t.Fatalf("expected 0 enqueued emails, got %d", len(store.enqueued))
	}
	if res.Affected != 0 {
		t.Errorf("Affected = %d, want 0", res.Affected)
	}
	for _, want := range []struct {
		key string
		val int
	}{{"checked", 0}, {"failed", 0}, {"notified", 0}} {
		if !attrsContain(res.Attrs, want.key, want.val) {
			t.Errorf("Attrs missing %s=%d: %v", want.key, want.val, res.Attrs)
		}
	}
}

// A single connector's CheckHealth call erroring out (a real infra failure
// -- e.g. the row vanished, or the sealed config would not open) must not
// stop the sweep from reaching the rest of the batch.
func TestJob_OneFailureDoesNotAbortSweep(t *testing.T) {
	orgID := uuid.New()
	brokenID := uuid.New()
	healthyID := uuid.New()

	store := &mockStore{
		listRows: []db.ListConnectorsForHealthCheckRow{
			{ID: brokenID, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusActive)},
			{ID: healthyID, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusActive)},
		},
		owners: map[uuid.UUID]db.GetOrganizationOwnerRow{
			orgID: {UserID: uuid.New(), Email: "owner@example.com"},
		},
	}
	checker := &mockChecker{results: map[uuid.UUID]checkResult{
		brokenID:  {err: errors.New("connector row vanished mid-sweep")},
		healthyID: {row: db.Connector{ID: healthyID, OrganizationID: orgID, Name: "Second Sheet", Status: string(connector.StatusError)}},
	}}

	j := New(store, checker, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(checker.calls) != 2 {
		t.Fatalf("expected both connectors to be attempted, got %d calls", len(checker.calls))
	}
	if !attrsContain(res.Attrs, "failed", 1) {
		t.Errorf("Attrs missing failed=1: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "checked", 1) {
		t.Errorf("Attrs missing checked=1: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "notified", 1) {
		t.Errorf("Attrs missing notified=1: %v", res.Attrs)
	}
	if len(store.enqueued) != 1 {
		t.Fatalf("expected the healthy-then-broken connector's email to still be enqueued, got %d", len(store.enqueued))
	}
}

// The notification must never carry the connector's config or the upstream
// Checker error string (CLAUDE.md). Architecturally CheckHealth does not
// even return the upstream error on the path that produces a notification
// (a Check() failure is swallowed into a status flip, not surfaced as a Go
// error) -- this test guards the other half: that a config value present on
// the returned row never reaches the rendered body, catching a regression
// where the job started serializing more of the row than its Name.
func TestJob_NotificationBody_ExcludesConfigValue(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()
	const secretConfigValue = "sk_super_secret_credential_12345"

	store := &mockStore{
		listRows: []db.ListConnectorsForHealthCheckRow{
			{ID: connID, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusActive)},
		},
		owners: map[uuid.UUID]db.GetOrganizationOwnerRow{
			orgID: {UserID: uuid.New(), Email: "owner@example.com"},
		},
	}
	checker := &mockChecker{results: map[uuid.UUID]checkResult{
		connID: {row: db.Connector{
			ID:              connID,
			OrganizationID:  orgID,
			Name:            "Prod Sheets",
			Status:          string(connector.StatusError),
			EncryptedConfig: []byte(`{"api_key":"` + secretConfigValue + `"}`),
		}},
	}}

	j := New(store, checker, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.enqueued) != 1 {
		t.Fatalf("expected exactly 1 enqueued email, got %d", len(store.enqueued))
	}
	got := store.enqueued[0]
	for _, part := range []struct {
		name string
		body *string
	}{{"html", got.BodyHtml}, {"text", got.BodyText}} {
		if part.body == nil {
			t.Fatalf("%s body is nil", part.name)
		}
		if strings.Contains(*part.body, secretConfigValue) {
			t.Errorf("%s body leaked the connector's config value:\n%s", part.name, *part.body)
		}
		if strings.Contains(*part.body, "api_key") {
			t.Errorf("%s body leaked a config field name:\n%s", part.name, *part.body)
		}
	}
}

// A failed owner lookup or a failed outbox enqueue must not fail the run --
// best-effort, same discipline as every other audit/email write in this
// codebase.
func TestJob_NotifyFailure_IsBestEffort(t *testing.T) {
	orgID := uuid.New()
	connID := uuid.New()

	store := &mockStore{
		listRows: []db.ListConnectorsForHealthCheckRow{
			{ID: connID, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusActive)},
		},
		ownerErr: errors.New("db is down"),
	}
	checker := &mockChecker{results: map[uuid.UUID]checkResult{
		connID: {row: db.Connector{ID: connID, OrganizationID: orgID, Name: "Prod Sheets", Status: string(connector.StatusError)}},
	}}

	j := New(store, checker, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.enqueued) != 0 {
		t.Fatalf("expected 0 enqueued emails when the owner lookup fails, got %d", len(store.enqueued))
	}
	if !attrsContain(res.Attrs, "notified", 0) {
		t.Errorf("Attrs missing notified=0: %v", res.Attrs)
	}
	// The connector itself was still successfully checked -- only the
	// best-effort notification failed.
	if !attrsContain(res.Attrs, "checked", 1) {
		t.Errorf("Attrs missing checked=1: %v", res.Attrs)
	}
}

// A failed listing call is the one thing this job cannot recover from -- it
// has nothing to check -- and must surface as an error so the worker's
// scheduler logs and retries it.
func TestJob_ListError_Propagates(t *testing.T) {
	store := &mockStore{listErr: errors.New("db down")}
	j := New(store, &mockChecker{}, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	_, err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "list connectors for health check") {
		t.Errorf("error = %v, want it to wrap \"list connectors for health check\"", err)
	}
}

// The context-cancellation guard is checked per-connector, mirroring
// emaildispatch: a batch most of the way through a run should stop rather
// than push through to the end of a dead context.
func TestJob_ContextCancelledMidLoop(t *testing.T) {
	orgID := uuid.New()
	first := uuid.New()
	second := uuid.New()

	ctx, cancel := context.WithCancel(context.Background())

	store := &mockStore{
		listRows: []db.ListConnectorsForHealthCheckRow{
			{ID: first, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusActive)},
			{ID: second, OrganizationID: orgID, Type: "google_sheets", Status: string(connector.StatusActive)},
		},
		owners: map[uuid.UUID]db.GetOrganizationOwnerRow{
			orgID: {UserID: uuid.New(), Email: "owner@example.com"},
		},
	}
	checker := &mockChecker{results: map[uuid.UUID]checkResult{
		first:  {row: db.Connector{ID: first, OrganizationID: orgID, Name: "First", Status: string(connector.StatusActive)}},
		second: {row: db.Connector{ID: second, OrganizationID: orgID, Name: "Second", Status: string(connector.StatusError)}},
	}}

	j := New(store, checker, realRenderer(t), newTestLogger(), time.Hour, 50, testAppURL)

	// Cancel immediately: the loop's first ctx.Err() check should fire
	// before any connector is checked.
	cancel()

	_, err := j.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(checker.calls) != 0 {
		t.Errorf("expected 0 connectors checked after cancellation, got %d", len(checker.calls))
	}
}
