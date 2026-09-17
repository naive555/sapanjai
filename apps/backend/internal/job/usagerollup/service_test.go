package usagerollup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/worker"
)

// ---- hand-mocked rollupStore ----

type mockStore struct {
	mu sync.Mutex

	rollupReturn int64
	rollupErr    error
	rollupCalls  []time.Time

	usageCount int64
	usageErr   error
	auditCount int64
	auditErr   error
	countCalls []time.Time

	// pruneReturns is consumed one value per call, batch-at-a-time, mirroring
	// sessioncleanup's mockStore.
	pruneReturns []int64
	pruneErrs    []error
	pruneCalls   []db.PruneUsageEventsParams
	onPruneCall  func(callIndex int)
}

func (m *mockStore) RollupUsageEvents(_ context.Context, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollupCalls = append(m.rollupCalls, since)
	return m.rollupReturn, m.rollupErr
}

func (m *mockStore) CountUsageEventsSince(_ context.Context, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.countCalls = append(m.countCalls, since)
	return m.usageCount, m.usageErr
}

func (m *mockStore) CountAuditLogsToolCalledSince(_ context.Context, _ time.Time) (int64, error) {
	return m.auditCount, m.auditErr
}

func (m *mockStore) PruneUsageEvents(_ context.Context, arg db.PruneUsageEventsParams) (int64, error) {
	m.mu.Lock()
	idx := len(m.pruneCalls)
	m.pruneCalls = append(m.pruneCalls, arg)
	m.mu.Unlock()

	if m.onPruneCall != nil {
		m.onPruneCall(idx)
	}

	var ret int64
	if idx < len(m.pruneReturns) {
		ret = m.pruneReturns[idx]
	}
	var err error
	if idx < len(m.pruneErrs) {
		err = m.pruneErrs[idx]
	}
	return ret, err
}

var _ rollupStore = (*mockStore)(nil)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func attrsContain(attrs []any, key string, value any) bool {
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key && attrs[i+1] == value {
			return true
		}
	}
	return false
}

// ---- rollupSince (pure function) ----

// defaultRetention is USAGE_EVENTS_RETENTION's shipped default (90 days),
// the value these cases are reasoned against unless they say otherwise.
const defaultRetention = 2160 * time.Hour

func TestRollupSince(t *testing.T) {
	utc := time.UTC
	for _, tc := range []struct {
		name      string
		now       time.Time
		retention time.Duration
		want      time.Time
	}{
		{
			name:      "mid-month",
			now:       time.Date(2026, time.September, 14, 10, 30, 0, 0, utc),
			retention: defaultRetention,
			want:      time.Date(2026, time.July, 1, 0, 0, 0, 0, utc),
		},
		{
			name:      "first instant of month",
			now:       time.Date(2026, time.September, 1, 0, 0, 0, 0, utc),
			retention: defaultRetention,
			want:      time.Date(2026, time.July, 1, 0, 0, 0, 0, utc),
		},
		{
			name:      "crosses a year boundary",
			now:       time.Date(2026, time.January, 15, 0, 0, 0, 0, utc),
			retention: defaultRetention,
			want:      time.Date(2025, time.November, 1, 0, 0, 0, 0, utc),
		},
		{
			// The clamp's reason for existing. Jun+Jul+Aug is 92 days, so
			// the unclamped 3-month lookback would return June 1 — but by
			// Aug 31 the prune has already deleted everything before
			// ~Jun 2, so recomputing June would overwrite a complete
			// rollup with a partial count. The floor must be July 1.
			name:      "month-end: 3 long months overshoot 90d retention",
			now:       time.Date(2026, time.August, 31, 23, 59, 59, 0, utc),
			retention: defaultRetention,
			want:      time.Date(2026, time.July, 1, 0, 0, 0, 0, utc),
		},
		{
			// Same shape at the other month-end the scan flagged.
			name:      "month-end: January 31",
			now:       time.Date(2026, time.January, 31, 23, 59, 59, 0, utc),
			retention: defaultRetention,
			want:      time.Date(2025, time.December, 1, 0, 0, 0, 0, utc),
		},
		{
			// Retention is an operator-tunable env var, so the clamp — not
			// rollupLookbackMonths — has to be what bounds the reach. At
			// 30 days only the current month is wholly inside retention.
			name:      "lowered retention binds tighter than the lookback",
			now:       time.Date(2026, time.September, 14, 10, 30, 0, 0, utc),
			retention: 720 * time.Hour,
			want:      time.Date(2026, time.September, 1, 0, 0, 0, 0, utc),
		},
		{
			// A cutoff landing exactly on a month start leaves that whole
			// month present, so it is safe to recompute — no +1 month.
			name:      "cutoff exactly on a month boundary keeps that month",
			now:       time.Date(2026, time.September, 30, 0, 0, 0, 0, utc),
			retention: 30 * 24 * time.Hour,
			want:      time.Date(2026, time.September, 1, 0, 0, 0, 0, utc),
		},
		{
			name: "non-UTC input is normalised to UTC before truncating",
			// 2026-09-01T02:00 +05:00 is still 2026-08-31T21:00 UTC, so the
			// current UTC month is August, not September. A local-timezone
			// date_trunc would get this wrong, which is exactly what the
			// package doc warns against. Retention is set long here so the
			// clamp cannot bind and this case isolates the timezone
			// handling it is actually about.
			now:       time.Date(2026, time.September, 1, 2, 0, 0, 0, time.FixedZone("test", 5*60*60)),
			retention: 365 * 24 * time.Hour,
			want:      time.Date(2026, time.June, 1, 0, 0, 0, 0, utc),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rollupSince(tc.now, tc.retention)
			if !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Errorf("rollupSince(%v, %v) = %v, want %v (UTC)", tc.now, tc.retention, got, tc.want)
			}
		})
	}
}

// TestRollupSince_NeverPrecedesPruneBoundary is the property the clamp
// exists to guarantee, asserted directly rather than through hand-picked
// dates: across every day of a leap year and several retention settings,
// the oldest period the rollup recomputes must never begin before the
// prune's cutoff. A regression here means a period gets recomputed from
// partially-deleted events and its rollup overwritten with a smaller
// count — silent billing-history loss, which no other test would catch.
func TestRollupSince_NeverPrecedesPruneBoundary(t *testing.T) {
	retentions := []time.Duration{
		defaultRetention,     // 90d, the shipped default
		60 * 24 * time.Hour,  // a plausible tightening
		30 * 24 * time.Hour,  // aggressive
		7 * 24 * time.Hour,   // shorter than one period
		400 * 24 * time.Hour, // longer than the lookback ceiling
	}
	// 2028 is a leap year, so this covers Feb 29 and every month length.
	start := time.Date(2028, time.January, 1, 23, 59, 59, 0, time.UTC)

	for _, retention := range retentions {
		for d := 0; d < 366; d++ {
			now := start.AddDate(0, 0, d)
			since := rollupSince(now, retention)
			cutoff := now.Add(-retention)
			if since.Before(cutoff) {
				t.Fatalf("retention=%v now=%s: rollupSince=%s precedes prune cutoff %s by %v",
					retention, now.Format(time.RFC3339), since.Format(time.RFC3339),
					cutoff.Format(time.RFC3339), cutoff.Sub(since))
			}
		}
	}
}

// ---- Run: happy path ----

func TestJob_HappyPath_RollsUpThenChecksDriftThenPrunes(t *testing.T) {
	store := &mockStore{
		rollupReturn: 42,
		usageCount:   100,
		auditCount:   100,
		pruneReturns: []int64{5},
	}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, 1000)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Affected != 42 {
		t.Errorf("Affected = %d, want 42 (the rollup's rows-affected)", res.Affected)
	}
	if !attrsContain(res.Attrs, "rolled_up", int64(42)) {
		t.Errorf("Attrs missing rolled_up=42: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "drift", int64(0)) {
		t.Errorf("Attrs missing drift=0: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "pruned", int64(5)) {
		t.Errorf("Attrs missing pruned=5: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "prune_drained", true) {
		t.Errorf("Attrs missing prune_drained=true: %v", res.Attrs)
	}

	if len(store.rollupCalls) != 1 {
		t.Fatalf("expected 1 RollupUsageEvents call, got %d", len(store.rollupCalls))
	}
	if len(store.pruneCalls) != 1 {
		t.Fatalf("expected 1 PruneUsageEvents call, got %d", len(store.pruneCalls))
	}
}

// The order matters: a run that fails between the two steps must not have
// pruned before it rolled up.
func TestJob_RollsUpBeforePruning(t *testing.T) {
	var order []string
	store := &mockStore{rollupReturn: 1, pruneReturns: []int64{0}}
	// Wrap PruneUsageEvents indirectly via onPruneCall/rollupCalls ordering:
	// record order using timestamps is fragile, so instead assert via a
	// rollup error short-circuiting prune (see next test) plus this direct
	// call-order check using a small shim.
	shim := &orderedStore{mockStore: store, order: &order}
	j := New(shim, discardLogger(), 15*time.Minute, 2160*time.Hour, 1000)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(order) < 2 || order[0] != "rollup" || order[len(order)-1] != "prune" {
		t.Fatalf("expected rollup before prune, got order=%v", order)
	}
}

type orderedStore struct {
	*mockStore
	order *[]string
}

func (o *orderedStore) RollupUsageEvents(ctx context.Context, since time.Time) (int64, error) {
	*o.order = append(*o.order, "rollup")
	return o.mockStore.RollupUsageEvents(ctx, since)
}

func (o *orderedStore) PruneUsageEvents(ctx context.Context, arg db.PruneUsageEventsParams) (int64, error) {
	*o.order = append(*o.order, "prune")
	return o.mockStore.PruneUsageEvents(ctx, arg)
}

// A rollup failure must abort before any prune is attempted: pruning without
// a successful, up-to-date rollup risks the very overwrite-with-a-smaller-
// count trap the package doc warns about, on a *later* run that recomputes
// this period again.
func TestJob_RollupFailure_NeverPrunes(t *testing.T) {
	store := &mockStore{rollupErr: errors.New("db down"), pruneReturns: []int64{0}}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, 1000)

	_, err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "rollup usage events") {
		t.Errorf("error does not wrap rollup usage events: %v", err)
	}
	if len(store.pruneCalls) != 0 {
		t.Errorf("expected 0 prune calls after a rollup failure, got %d", len(store.pruneCalls))
	}
}

// A prune failure must not erase the fact that the rollup already committed
// real work; Affected still reports it even though Run returns an error.
func TestJob_PruneFailure_StillReportsRollupAffected(t *testing.T) {
	store := &mockStore{
		rollupReturn: 7,
		pruneErrs:    []error{errors.New("db down")},
	}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, 1000)

	res, err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "prune usage events") {
		t.Errorf("error does not wrap prune usage events: %v", err)
	}
	if res.Affected != 7 {
		t.Errorf("Affected = %d, want 7 (the rollup already committed)", res.Affected)
	}
}

// ---- drift cross-check ----

func TestJob_Drift_LogsWarnWhenNonZero(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := &mockStore{
		rollupReturn: 1,
		usageCount:   90,
		auditCount:   100, // 10 audit rows with no matching usage_events row
		pruneReturns: []int64{0},
	}
	j := New(store, log, 15*time.Minute, 2160*time.Hour, 1000)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !attrsContain(res.Attrs, "drift", int64(10)) {
		t.Errorf("Attrs missing drift=10: %v", res.Attrs)
	}

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("expected a WARN log line for nonzero drift, got:\n%s", logged)
	}
	if !strings.Contains(logged, "drift") {
		t.Errorf("expected the drift log line to mention drift, got:\n%s", logged)
	}
}

func TestJob_Drift_LogsInfoWhenZero(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := &mockStore{
		rollupReturn: 1,
		usageCount:   50,
		auditCount:   50,
		pruneReturns: []int64{0},
	}
	j := New(store, log, 15*time.Minute, 2160*time.Hour, 1000)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logged := buf.String()
	if strings.Contains(logged, "level=WARN") {
		t.Errorf("expected no WARN log line for zero drift, got:\n%s", logged)
	}
	if !strings.Contains(logged, "level=INFO") {
		t.Errorf("expected an INFO log line for zero drift, got:\n%s", logged)
	}
}

// A failure to even compute the drift counts must not fail the run: it is a
// diagnostic, not a correctness dependency.
func TestJob_Drift_CountFailureIsNonFatal(t *testing.T) {
	store := &mockStore{
		rollupReturn: 1,
		usageErr:     errors.New("count query failed"),
		pruneReturns: []int64{0},
	}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, 1000)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !attrsContain(res.Attrs, "drift_error", true) {
		t.Errorf("Attrs missing drift_error=true: %v", res.Attrs)
	}
	if len(store.pruneCalls) != 1 {
		t.Errorf("expected prune to still run despite the drift-check failure, got %d calls", len(store.pruneCalls))
	}
}

// ---- prune batching ----

func TestJob_Prune_MultiBatchDrain(t *testing.T) {
	store := &mockStore{
		rollupReturn: 0,
		pruneReturns: []int64{1000, 1000, 42},
	}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, 1000)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.pruneCalls) != 3 {
		t.Fatalf("expected 3 prune calls, got %d", len(store.pruneCalls))
	}
	if !attrsContain(res.Attrs, "pruned", int64(2042)) {
		t.Errorf("Attrs missing pruned=2042: %v", res.Attrs)
	}
	if !attrsContain(res.Attrs, "prune_drained", true) {
		t.Errorf("Attrs missing prune_drained=true: %v", res.Attrs)
	}
}

func TestJob_Prune_BatchCap(t *testing.T) {
	const batchSize = 10
	returns := make([]int64, maxPruneBatches)
	for i := range returns {
		returns[i] = batchSize
	}
	store := &mockStore{rollupReturn: 0, pruneReturns: returns}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, batchSize)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.pruneCalls) != maxPruneBatches {
		t.Fatalf("expected exactly %d prune calls (batch cap), got %d", maxPruneBatches, len(store.pruneCalls))
	}
	if !attrsContain(res.Attrs, "prune_drained", false) {
		t.Errorf("expected prune_drained=false (cap hit before draining): %v", res.Attrs)
	}
}

func TestJob_Prune_ParamsCorrect(t *testing.T) {
	store := &mockStore{rollupReturn: 0, pruneReturns: []int64{0}}
	j := New(store, discardLogger(), 15*time.Minute, 720*time.Hour, 500)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.pruneCalls) != 1 {
		t.Fatalf("expected 1 prune call, got %d", len(store.pruneCalls))
	}
	got := store.pruneCalls[0]
	if got.RetentionSeconds != 2_592_000 {
		t.Errorf("RetentionSeconds = %d, want 2592000 (30d)", got.RetentionSeconds)
	}
	if got.BatchSize != 500 {
		t.Errorf("BatchSize = %d, want 500", got.BatchSize)
	}
}

// ---- cancellation ----

func TestJob_ContextCancelledBeforeRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &mockStore{rollupReturn: 1, pruneReturns: []int64{0}}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, 1000)

	_, err := j.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(store.rollupCalls) != 0 {
		t.Errorf("expected no rollup call once ctx is already cancelled, got %d", len(store.rollupCalls))
	}
}

func TestJob_ContextCancelledMidPrune(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &mockStore{rollupReturn: 1, pruneReturns: []int64{500, 0}}
	store.onPruneCall = func(idx int) {
		if idx == 0 {
			cancel()
		}
	}
	j := New(store, discardLogger(), 15*time.Minute, 2160*time.Hour, 500)

	res, err := j.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !attrsContain(res.Attrs, "pruned", int64(500)) {
		t.Errorf("expected the already-committed batch to still be reported: %v", res.Attrs)
	}
	if len(store.pruneCalls) != 1 {
		t.Errorf("expected exactly 1 prune call before cancellation was observed, got %d", len(store.pruneCalls))
	}
}

// ---- identity ----

func TestJob_NameAndInterval(t *testing.T) {
	j := New(&mockStore{}, discardLogger(), 42*time.Minute, time.Hour, 10)

	if j.Name() != "usage-rollup" {
		t.Errorf("Name() = %q, want %q", j.Name(), "usage-rollup")
	}
	if j.Interval() != 42*time.Minute {
		t.Errorf("Interval() = %v, want 42m", j.Interval())
	}
	var _ worker.Job = j
}
