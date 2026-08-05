package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- hand-mocked Locker ----

type fakeLocker struct {
	mu         sync.Mutex
	acquireOK  bool
	acquireErr error
	released   int
	lastOwner  string
}

func (f *fakeLocker) Acquire(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquireOK, f.acquireErr
}

func (f *fakeLocker) Release(_ context.Context, _, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	f.lastOwner = owner
	return nil
}

func (f *fakeLocker) releaseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

var _ Locker = (*fakeLocker)(nil)

// ---- hand-mocked Job ----

type fakeJob struct {
	name     string
	interval time.Duration
	runs     chan struct{} // optional; buffered, Run sends on it if non-nil
	fn       func(ctx context.Context) (Result, error)
}

func (f *fakeJob) Name() string            { return f.name }
func (f *fakeJob) Interval() time.Duration { return f.interval }

func (f *fakeJob) Run(ctx context.Context) (Result, error) {
	if f.runs != nil {
		f.runs <- struct{}{}
	}
	if f.fn != nil {
		return f.fn(ctx)
	}
	return Result{}, nil
}

var _ Job = (*fakeJob)(nil)

// ---- helpers ----

func newTestWorker(locker Locker, jobTimeout time.Duration) *Worker {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(locker, log, jobTimeout)
}

func requireReceive(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func requireNoReceive(t *testing.T, ch <-chan struct{}, wait time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected %s", what)
	case <-time.After(wait):
	}
}

func waitForCondition(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- tests ----

func TestWorker_RunsAtStartup(t *testing.T) {
	locker := &fakeLocker{acquireOK: true}
	runs := make(chan struct{}, 10)
	// Long interval: the only run we can observe within the test window is
	// the guaranteed startup run.
	job := &fakeJob{name: "startup", interval: time.Hour, runs: runs}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	requireReceive(t, runs, 2*time.Second, "startup run")
}

func TestWorker_RunsRepeatedly(t *testing.T) {
	locker := &fakeLocker{acquireOK: true}
	runs := make(chan struct{}, 10)
	job := &fakeJob{name: "repeat", interval: 20 * time.Millisecond, runs: runs}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	for range 3 {
		requireReceive(t, runs, 2*time.Second, "a repeated run")
	}
}

func TestWorker_SkipsWhenLockHeldElsewhere(t *testing.T) {
	locker := &fakeLocker{acquireOK: false}
	runs := make(chan struct{}, 10)
	job := &fakeJob{name: "skip", interval: 20 * time.Millisecond, runs: runs}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	requireNoReceive(t, runs, 200*time.Millisecond, "a run while the lock is held elsewhere")

	stats := w.Stats()
	if stats[0].Skipped == 0 {
		t.Fatalf("expected Skipped > 0, got %+v", stats[0])
	}
	if stats[0].Runs != 0 {
		t.Fatalf("expected no runs recorded, got %+v", stats[0])
	}
}

func TestWorker_LockErrorRecordedNoRun(t *testing.T) {
	locker := &fakeLocker{acquireErr: errors.New("redis down")}
	runs := make(chan struct{}, 10)
	job := &fakeJob{name: "lockerr", interval: 20 * time.Millisecond, runs: runs}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	requireNoReceive(t, runs, 200*time.Millisecond, "a run despite a lock error")

	stats := w.Stats()
	if stats[0].Failures == 0 {
		t.Fatalf("expected Failures > 0, got %+v", stats[0])
	}
	if stats[0].LastError == "" {
		t.Fatalf("expected LastError to be set, got %+v", stats[0])
	}
}

func TestWorker_JobErrorReleasesLock(t *testing.T) {
	locker := &fakeLocker{acquireOK: true}
	job := &fakeJob{
		name:     "joberr",
		interval: time.Hour, // only the startup run happens in this window
		fn: func(ctx context.Context) (Result, error) {
			return Result{}, errors.New("boom")
		},
	}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	waitForCondition(t, func() bool { return locker.releaseCount() == 1 }, 2*time.Second, "lock release after job error")

	stats := w.Stats()
	if stats[0].Failures != 1 {
		t.Fatalf("expected exactly 1 failure, got %+v", stats[0])
	}
	if stats[0].LastError == "" {
		t.Fatalf("expected LastError to be set, got %+v", stats[0])
	}
}

func TestWorker_PanicRecovered(t *testing.T) {
	locker := &fakeLocker{acquireOK: true}
	first := make(chan struct{})
	var calls int32

	job := &fakeJob{
		name:     "panicky",
		interval: 100 * time.Millisecond,
		fn: func(ctx context.Context) (Result, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				close(first)
				panic("kaboom")
			}
			return Result{}, nil
		},
	}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	<-first
	waitForCondition(t, func() bool { return w.Stats()[0].Failures > 0 }, 2*time.Second, "panic to be recorded")

	stats := w.Stats()
	if stats[0].Failures == 0 {
		t.Fatalf("expected Failures > 0 after panic, got %+v", stats[0])
	}
	if !strings.Contains(stats[0].LastError, "panic:") {
		t.Fatalf("expected LastError to mention the panic, got %q", stats[0].LastError)
	}

	// The worker must survive the panic and keep scheduling the job.
	waitForCondition(t, func() bool { return atomic.LoadInt32(&calls) >= 2 }, 3*time.Second, "a second run after the panic")
}

func TestWorker_SuccessDoesNotReleaseLock(t *testing.T) {
	locker := &fakeLocker{acquireOK: true}
	runs := make(chan struct{}, 10)
	job := &fakeJob{name: "success", interval: time.Hour, runs: runs}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	requireReceive(t, runs, 2*time.Second, "startup run")
	// Give tryRun a moment to finish recording stats before we assert.
	time.Sleep(50 * time.Millisecond)

	if got := locker.releaseCount(); got != 0 {
		t.Fatalf("expected no lock release after a successful run, got %d", got)
	}
}

func TestWorker_PerRunTimeout(t *testing.T) {
	locker := &fakeLocker{acquireOK: true}
	job := &fakeJob{
		name:     "slow",
		interval: time.Hour, // only the startup run happens in this window
		fn: func(ctx context.Context) (Result, error) {
			<-ctx.Done()
			return Result{}, ctx.Err()
		},
	}

	w := newTestWorker(locker, 20*time.Millisecond)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)

	waitForCondition(t, func() bool { return w.Stats()[0].Failures > 0 }, 2*time.Second, "the run to time out")
}

func TestWorker_CancelStopsRun(t *testing.T) {
	locker := &fakeLocker{acquireOK: true}
	job := &fakeJob{name: "cancel", interval: 10 * time.Millisecond}

	w := newTestWorker(locker, time.Second)
	w.Register(job)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let it tick a couple of times
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestWorker_RegisterDuplicatePanics(t *testing.T) {
	w := newTestWorker(&fakeLocker{acquireOK: true}, time.Second)
	w.Register(&fakeJob{name: "dup", interval: time.Hour})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected Register to panic on a duplicate job name")
		}
	}()
	w.Register(&fakeJob{name: "dup", interval: time.Hour})
}

func TestLockTTL(t *testing.T) {
	if got, want := lockTTL(time.Hour), 54*time.Minute; got != want {
		t.Errorf("lockTTL(1h) = %v, want %v", got, want)
	}
	if got, want := lockTTL(20*time.Millisecond), 18*time.Millisecond; got != want {
		t.Errorf("lockTTL(20ms) = %v, want %v", got, want)
	}

	// For any positive interval, the lock must never come back <= 0 —
	// that would make Acquire's TTL argument reject the lock immediately.
	for _, interval := range []time.Duration{time.Nanosecond, time.Microsecond, time.Millisecond, 7 * time.Millisecond} {
		if got := lockTTL(interval); got <= 0 {
			t.Errorf("lockTTL(%v) = %v, want > 0", interval, got)
		}
	}
}
