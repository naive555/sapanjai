package sessioncleanup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junctera/backend/internal/infra/database/db"
	"github.com/junctera/backend/internal/worker"
)

// ---- hand-mocked cleanupStore ----

type mockStore struct {
	mu      sync.Mutex
	returns []int64
	errs    []error
	calls   []db.DeleteExpiredSessionsParams
	onCall  func(callIndex int) // optional hook, invoked after recording each call
}

func (m *mockStore) DeleteExpiredSessions(_ context.Context, arg db.DeleteExpiredSessionsParams) (int64, error) {
	m.mu.Lock()
	idx := len(m.calls)
	m.calls = append(m.calls, arg)
	m.mu.Unlock()

	if m.onCall != nil {
		m.onCall(idx)
	}

	var ret int64
	if idx < len(m.returns) {
		ret = m.returns[idx]
	}
	var err error
	if idx < len(m.errs) {
		err = m.errs[idx]
	}
	return ret, err
}

var _ cleanupStore = (*mockStore)(nil)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

func TestJob_SingleShortBatch(t *testing.T) {
	store := &mockStore{returns: []int64{7}}
	j := New(store, newTestLogger(), time.Hour, 720*time.Hour, 1000)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(store.calls))
	}
	if res.Affected != 7 {
		t.Fatalf("expected Affected=7, got %d", res.Affected)
	}
	if !attrsContain(res.Attrs, "drained", true) {
		t.Fatalf("expected drained=true in Attrs, got %v", res.Attrs)
	}
}

func TestJob_MultiBatchDrain(t *testing.T) {
	store := &mockStore{returns: []int64{1000, 1000, 42}}
	j := New(store, newTestLogger(), time.Hour, 720*time.Hour, 1000)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(store.calls))
	}
	if res.Affected != 2042 {
		t.Fatalf("expected Affected=2042, got %d", res.Affected)
	}
	if !attrsContain(res.Attrs, "drained", true) {
		t.Fatalf("expected drained=true in Attrs, got %v", res.Attrs)
	}
}

func TestJob_ParamsCorrect(t *testing.T) {
	store := &mockStore{returns: []int64{0}}
	j := New(store, newTestLogger(), 720*time.Hour, 720*time.Hour, 500)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(store.calls))
	}
	got := store.calls[0]
	if got.RetentionSeconds != 2_592_000 {
		t.Errorf("expected RetentionSeconds=2592000 (30d), got %d", got.RetentionSeconds)
	}
	if got.BatchSize != 500 {
		t.Errorf("expected BatchSize=500, got %d", got.BatchSize)
	}
}

func TestJob_StoreErrorPropagates(t *testing.T) {
	store := &mockStore{
		returns: []int64{500, 0},
		errs:    []error{nil, errors.New("db down")},
	}
	j := New(store, newTestLogger(), time.Hour, 720*time.Hour, 500)

	res, err := j.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "delete expired sessions") {
		t.Errorf("expected error to wrap \"delete expired sessions\", got %v", err)
	}
	if res.Affected != 500 {
		t.Errorf("expected partial Affected=500 from the first batch, got %d", res.Affected)
	}
	if len(store.calls) != 2 {
		t.Errorf("expected 2 calls (1 ok, 1 erroring), got %d", len(store.calls))
	}
}

func TestJob_BatchCap(t *testing.T) {
	const batchSize = 10
	returns := make([]int64, maxBatches)
	for i := range returns {
		returns[i] = batchSize
	}
	store := &mockStore{returns: returns}
	j := New(store, newTestLogger(), time.Hour, 720*time.Hour, batchSize)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.calls) != maxBatches {
		t.Fatalf("expected exactly %d calls (batch cap), got %d", maxBatches, len(store.calls))
	}
	if res.Affected != int64(maxBatches*batchSize) {
		t.Errorf("expected Affected=%d, got %d", maxBatches*batchSize, res.Affected)
	}
	if !attrsContain(res.Attrs, "drained", false) {
		t.Errorf("expected drained=false in Attrs (cap hit before draining), got %v", res.Attrs)
	}
}

func TestJob_ContextCancelledMidLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &mockStore{returns: []int64{500, 0}}
	store.onCall = func(idx int) {
		if idx == 0 {
			// Cancel after the first batch commits but before the loop
			// attempts a second one.
			cancel()
		}
	}
	j := New(store, newTestLogger(), time.Hour, 720*time.Hour, 500)

	res, err := j.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if res.Affected != 500 {
		t.Fatalf("expected partial Affected=500 from the committed batch, got %d", res.Affected)
	}
	if len(store.calls) != 1 {
		t.Fatalf("expected exactly 1 call before the cancellation was observed, got %d", len(store.calls))
	}
}

func TestJob_NameAndInterval(t *testing.T) {
	j := New(&mockStore{}, newTestLogger(), 42*time.Minute, time.Hour, 10)

	if j.Name() != "session-cleanup" {
		t.Errorf("expected Name()=%q, got %q", "session-cleanup", j.Name())
	}
	if j.Interval() != 42*time.Minute {
		t.Errorf("expected Interval()=42m, got %v", j.Interval())
	}
	var _ worker.Job = j
}
