package emaildispatch

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
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/email"
)

// ---- hand-mocked dispatchStore ----

type mockStore struct {
	mu sync.Mutex

	claimBatches [][]db.EmailOutbox // one entry per Run
	claimErr     error
	claimCalls   []db.ClaimPendingEmailsParams

	sentIDs      []uuid.UUID
	failed       []db.MarkEmailFailedParams
	rescheduled  []db.RescheduleEmailParams
	pruneCalls   []db.PruneEmailOutboxParams
	pruneReturns []int64 // one entry per prune call; missing entries return 0

	markSentErr error
}

func (m *mockStore) ClaimPendingEmails(_ context.Context, arg db.ClaimPendingEmailsParams) ([]db.EmailOutbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := len(m.claimCalls)
	m.claimCalls = append(m.claimCalls, arg)
	if m.claimErr != nil {
		return nil, m.claimErr
	}
	if idx < len(m.claimBatches) {
		return m.claimBatches[idx], nil
	}
	return nil, nil
}

func (m *mockStore) MarkEmailSent(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentIDs = append(m.sentIDs, id)
	return m.markSentErr
}

func (m *mockStore) MarkEmailFailed(_ context.Context, arg db.MarkEmailFailedParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, arg)
	return nil
}

func (m *mockStore) RescheduleEmail(_ context.Context, arg db.RescheduleEmailParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rescheduled = append(m.rescheduled, arg)
	return nil
}

func (m *mockStore) PruneEmailOutbox(_ context.Context, arg db.PruneEmailOutboxParams) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := len(m.pruneCalls)
	m.pruneCalls = append(m.pruneCalls, arg)
	if idx < len(m.pruneReturns) {
		return m.pruneReturns[idx], nil
	}
	return 0, nil
}

var _ dispatchStore = (*mockStore)(nil)

// ---- fake Sender ----

type fakeSender struct {
	mu   sync.Mutex
	got  []email.Message
	errs map[string]error // keyed by recipient
}

func (f *fakeSender) Send(_ context.Context, m email.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, m)
	return f.errs[m.To]
}

func (f *fakeSender) messages() []email.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]email.Message(nil), f.got...)
}

var _ email.Sender = (*fakeSender)(nil)

// cancellingSender cancels the run's context from inside Send and then
// fails, reproducing a provider call that is still in flight when the
// worker's per-run timeout fires.
type cancellingSender struct {
	cancel context.CancelFunc
	err    error
}

func (c *cancellingSender) Send(context.Context, email.Message) error {
	c.cancel()
	return c.err
}

var _ email.Sender = (*cancellingSender)(nil)

// ---- helpers ----

func strptr(s string) *string { return &s }

func testRow(to string, attempts int32) db.EmailOutbox {
	return db.EmailOutbox{
		ID:        uuid.New(),
		ToAddress: to,
		Subject:   "Verify your email address",
		BodyHtml:  strptr(`<a href="https://app/verify-email?token=` + secretToken + `">Verify</a>`),
		BodyText:  strptr("Verify: https://app/verify-email?token=" + secretToken),
		Status:    "pending",
		Attempts:  attempts,
	}
}

const secretToken = "8f14e45fceea167a5a36dedd4bea2543b1e3b3c8a2f5d9e0c7b4a1968d2e5f70"

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newJob(store dispatchStore, sender email.Sender) *Job {
	return New(store, sender, discardLogger(), 15*time.Second, 5*time.Minute, 20, 5, 168*time.Hour)
}

func attrValue(attrs []any, key string) (any, bool) {
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key {
			return attrs[i+1], true
		}
	}
	return nil, false
}

// ---- identity ----

func TestJob_NameAndInterval(t *testing.T) {
	j := newJob(&mockStore{}, &fakeSender{})

	if j.Name() != "email-dispatch" {
		t.Errorf("Name() = %q, want email-dispatch", j.Name())
	}
	if j.Interval() != 15*time.Second {
		t.Errorf("Interval() = %v, want 15s", j.Interval())
	}
}

// ---- claiming ----

func TestJob_EmptyOutboxIsANoOp(t *testing.T) {
	store := &mockStore{}
	sender := &fakeSender{}
	j := newJob(store, sender)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Affected != 0 {
		t.Errorf("Affected = %d, want 0", res.Affected)
	}
	if len(sender.messages()) != 0 {
		t.Errorf("sent %d messages from an empty outbox", len(sender.messages()))
	}
}

func TestJob_ClaimsWithBatchSize(t *testing.T) {
	store := &mockStore{}
	j := newJob(store, &fakeSender{})

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.claimCalls) != 1 {
		t.Fatalf("claimed %d times, want 1", len(store.claimCalls))
	}
	if got := store.claimCalls[0].BatchSize; got != 20 {
		t.Errorf("BatchSize = %d, want 20", got)
	}
}

// The lease has to outlast the longest possible RUN, not the interval.
//
// Sizing it off the interval is what the first version did, and it is wrong:
// the worker holds a job's lock for only interval-interval/10 (13.5s at the
// 15s default), so on a multi-replica fleet the next run starts long before
// this one has worked through a batch of 20 sends that may take up to a
// minute each. Once the old 60s lease lapsed mid-flight, the second run
// re-claimed rows the first was still sending — double delivery attempts, and
// a double attempts increment that silently eats the retry budget.
//
// The worker cancels any run at jobTimeout, so no run can outlive it: that
// plus one interval of margin is the correct lease.
func TestJob_LeaseOutlastsAFullRun(t *testing.T) {
	store := &mockStore{}
	j := New(store, &fakeSender{}, discardLogger(), 15*time.Second, 5*time.Minute, 20, 5, time.Hour)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := store.claimCalls[0].LeaseSeconds
	want := int32((5*time.Minute + 15*time.Second).Seconds())
	if got != want {
		t.Errorf("LeaseSeconds = %d, want %d (jobTimeout + one interval)", got, want)
	}
	if got <= int32((15 * time.Second).Seconds()) {
		t.Errorf("lease %ds does not outlast even one dispatch interval", got)
	}
}

func TestJob_LeaseHasAFloor(t *testing.T) {
	store := &mockStore{}
	j := New(store, &fakeSender{}, discardLogger(), time.Second, time.Second, 1, 1, time.Hour)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.claimCalls[0].LeaseSeconds; got != minLeaseSeconds {
		t.Errorf("LeaseSeconds = %d for sub-second config, want the %d floor", got, minLeaseSeconds)
	}
}

// A claim failure means the job genuinely could not run, so it propagates and
// the worker releases its lock for an earlier retry.
func TestJob_ClaimErrorIsReturned(t *testing.T) {
	store := &mockStore{claimErr: errors.New("connection refused")}
	j := newJob(store, &fakeSender{})

	if _, err := j.Run(context.Background()); err == nil {
		t.Fatal("Run returned nil when the claim failed")
	}
}

// ---- sending ----

func TestJob_SendsClaimedRowsAndMarksThemSent(t *testing.T) {
	a, b := testRow("a@example.com", 1), testRow("b@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{a, b}}}
	sender := &fakeSender{}
	j := newJob(store, sender)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Affected != 2 {
		t.Errorf("Affected = %d, want 2", res.Affected)
	}
	if len(sender.messages()) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sender.messages()))
	}
	if len(store.sentIDs) != 2 {
		t.Errorf("marked %d rows sent, want 2", len(store.sentIDs))
	}
	if len(store.failed) != 0 || len(store.rescheduled) != 0 {
		t.Error("a successful batch should neither fail nor reschedule anything")
	}
}

// The row is the only source of the message; every field has to survive.
func TestJob_MessageCarriesTheRowVerbatim(t *testing.T) {
	row := testRow("person@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{row}}}
	sender := &fakeSender{}
	j := newJob(store, sender)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1", len(msgs))
	}
	got := msgs[0]
	if got.To != row.ToAddress {
		t.Errorf("To = %q, want %q", got.To, row.ToAddress)
	}
	if got.Subject != row.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, row.Subject)
	}
	if got.HTML != *row.BodyHtml {
		t.Errorf("HTML body did not survive")
	}
	if got.Text != *row.BodyText {
		t.Errorf("text body did not survive")
	}
}

// This is what makes at-least-once claiming safe. A run that sends and then
// dies before recording it re-claims the row when the lease lapses; the
// provider collapses the duplicate on this key. The row id is the only value
// that is stable across those two attempts.
func TestJob_MessageCarriesRowIDAsIdempotencyKey(t *testing.T) {
	row := testRow("person@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{row}}}
	sender := &fakeSender{}
	j := newJob(store, sender)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sender.messages()[0].IdempotencyKey; got != row.ID.String() {
		t.Errorf("IdempotencyKey = %q, want the row id %q", got, row.ID)
	}
}

// ---- failure handling ----

func TestJob_RetryableFailureReschedulesWithBackoff(t *testing.T) {
	row := testRow("boom@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{row}}}
	sender := &fakeSender{errs: map[string]error{"boom@example.com": errors.New("upstream 500")}}
	j := newJob(store, sender)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned an error for an individual send failure: %v", err)
	}
	if res.Affected != 0 {
		t.Errorf("Affected = %d, want 0", res.Affected)
	}
	if len(store.rescheduled) != 1 {
		t.Fatalf("rescheduled %d rows, want 1", len(store.rescheduled))
	}
	if len(store.failed) != 0 {
		t.Error("a row with attempts below the budget must not be marked failed")
	}
	got := store.rescheduled[0]
	if got.ID != row.ID {
		t.Errorf("rescheduled the wrong row")
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "upstream 500") {
		t.Errorf("LastError = %v, want it to carry the upstream reason", got.LastError)
	}
	if got.BackoffSeconds != int32(baseBackoff.Seconds()) {
		t.Errorf("BackoffSeconds = %d after one failure, want %d", got.BackoffSeconds, int32(baseBackoff.Seconds()))
	}
}

func TestJob_ExhaustedBudgetMarksFailed(t *testing.T) {
	row := testRow("dead@example.com", 5) // maxAttempts is 5
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{row}}}
	sender := &fakeSender{errs: map[string]error{"dead@example.com": errors.New("domain not verified")}}
	j := newJob(store, sender)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.failed) != 1 {
		t.Fatalf("marked %d rows failed, want 1", len(store.failed))
	}
	if len(store.rescheduled) != 0 {
		t.Error("a row at the attempt budget must not be rescheduled")
	}
	if le := store.failed[0].LastError; le == nil || !strings.Contains(*le, "domain not verified") {
		t.Errorf("LastError = %v, want the upstream reason", le)
	}
}

// One bad recipient must not strand the rest of the batch.
func TestJob_OneFailureDoesNotAbortTheBatch(t *testing.T) {
	good1, bad, good2 := testRow("g1@example.com", 1), testRow("bad@example.com", 1), testRow("g2@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{good1, bad, good2}}}
	sender := &fakeSender{errs: map[string]error{"bad@example.com": errors.New("nope")}}
	j := newJob(store, sender)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Affected != 2 {
		t.Errorf("Affected = %d, want 2 (the two good rows)", res.Affected)
	}
	if len(store.sentIDs) != 2 {
		t.Errorf("marked %d rows sent, want 2", len(store.sentIDs))
	}
	if len(store.rescheduled) != 1 {
		t.Errorf("rescheduled %d rows, want 1", len(store.rescheduled))
	}
}

// A row whose body was already stripped (a double-claim racing MarkEmailSent,
// or a hand-edited row) cannot be sent. It must be closed out rather than
// delivered as an empty email, and the Sender must never see it.
func TestJob_RowWithMissingBodyIsFailedNotSent(t *testing.T) {
	row := testRow("nobody@example.com", 1)
	row.BodyHtml, row.BodyText = nil, nil
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{row}}}
	sender := &fakeSender{}
	j := newJob(store, sender)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sender.messages()) != 0 {
		t.Error("a row with no body was handed to the Sender")
	}
	if len(store.failed) != 1 {
		t.Errorf("marked %d rows failed, want 1", len(store.failed))
	}
}

// A store write failing mid-batch is not a reason to strand the remaining
// rows: the row keeps its lease and is retried once the lease lapses.
func TestJob_MarkSentErrorDoesNotAbortTheBatch(t *testing.T) {
	a, b := testRow("a@example.com", 1), testRow("b@example.com", 1)
	store := &mockStore{
		claimBatches: [][]db.EmailOutbox{{a, b}},
		markSentErr:  errors.New("write conflict"),
	}
	sender := &fakeSender{}
	j := newJob(store, sender)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sender.messages()) != 2 {
		t.Errorf("sent %d messages, want both attempted despite the write failure", len(sender.messages()))
	}
}

// last_error is a database column, not a log sink.
func TestJob_TruncatesLongErrors(t *testing.T) {
	row := testRow("verbose@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{row}}}
	sender := &fakeSender{errs: map[string]error{"verbose@example.com": errors.New(strings.Repeat("x", 5000))}}
	j := newJob(store, sender)

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.rescheduled) != 1 {
		t.Fatalf("rescheduled %d rows, want 1", len(store.rescheduled))
	}
	if le := store.rescheduled[0].LastError; le == nil || len(*le) > maxLastErrorLen {
		got := 0
		if le != nil {
			got = len(*le)
		}
		t.Errorf("last_error length = %d, want <= %d", got, maxLastErrorLen)
	}
}

// ---- backoff math ----

func TestBackoffFor(t *testing.T) {
	for _, tc := range []struct {
		attempts int32
		want     time.Duration
	}{
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 480 * time.Second},
		{6, 960 * time.Second},
		{7, maxBackoff},  // 1920s would exceed the ceiling
		{20, maxBackoff}, // must clamp, not overflow
		{64, maxBackoff}, // a shift this wide would wrap without a guard
	} {
		if got := backoffFor(tc.attempts); got != tc.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

func TestBackoffFor_NeverNegativeOrZero(t *testing.T) {
	for attempts := int32(0); attempts < 100; attempts++ {
		if got := backoffFor(attempts); got <= 0 || got > maxBackoff {
			t.Fatalf("backoffFor(%d) = %v, outside (0, %v]", attempts, got, maxBackoff)
		}
	}
}

// ---- prune throttling ----

func TestJob_PrunesOnFirstRun(t *testing.T) {
	store := &mockStore{pruneReturns: []int64{3}}
	j := newJob(store, &fakeSender{})

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.pruneCalls) != 1 {
		t.Fatalf("pruned %d times on a fresh job, want 1", len(store.pruneCalls))
	}
	if got := store.pruneCalls[0].RetentionSeconds; got != int32((168 * time.Hour).Seconds()) {
		t.Errorf("RetentionSeconds = %d, want the configured retention", got)
	}
	if v, ok := attrValue(res.Attrs, "pruned"); !ok || v != int64(3) {
		t.Errorf("Attrs pruned = %v (present=%v), want 3", v, ok)
	}
}

func TestJob_DoesNotPruneAgainWithinTheHour(t *testing.T) {
	store := &mockStore{}
	j := newJob(store, &fakeSender{})

	base := time.Now()
	j.now = func() time.Time { return base }

	for i := 0; i < 3; i++ {
		if _, err := j.Run(context.Background()); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if len(store.pruneCalls) != 1 {
		t.Errorf("pruned %d times across three runs in the same minute, want 1", len(store.pruneCalls))
	}
}

func TestJob_PrunesAgainAfterTheInterval(t *testing.T) {
	store := &mockStore{}
	j := newJob(store, &fakeSender{})

	base := time.Now()
	j.now = func() time.Time { return base }
	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	j.now = func() time.Time { return base.Add(pruneInterval + time.Minute) }
	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if len(store.pruneCalls) != 2 {
		t.Errorf("pruned %d times across an hour boundary, want 2", len(store.pruneCalls))
	}
}

// ---- result and logging ----

func TestJob_ResultCountsClaimedSentAndFailed(t *testing.T) {
	good, bad := testRow("g@example.com", 1), testRow("b@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{good, bad}}}
	sender := &fakeSender{errs: map[string]error{"b@example.com": errors.New("nope")}}
	j := newJob(store, sender)

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, tc := range []struct {
		key  string
		want any
	}{{"claimed", 2}, {"sent", 1}, {"failed", 1}} {
		got, ok := attrValue(res.Attrs, tc.key)
		if !ok {
			t.Errorf("Attrs has no %q", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("Attrs %s = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// The job handles rendered bodies containing live single-use tokens. Its logs
// go to production; the token must not.
func TestJob_NeverLogsTheBody(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	good, bad := testRow("g@example.com", 1), testRow("b@example.com", 5)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{good, bad}}}
	sender := &fakeSender{errs: map[string]error{"b@example.com": errors.New("nope")}}

	j := New(store, sender, log, 15*time.Second, 5*time.Minute, 20, 5, time.Hour)
	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("the job logged a live token.\nlog:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "<a href=") {
		t.Errorf("the job logged an HTML body.\nlog:\n%s", buf.String())
	}
}

// ---- cancellation ----

// The worker cancels ctx on its per-run timeout and on shutdown. The job must
// stop promptly rather than working through a full batch against a dead
// context; rows it has not reached keep their lease and are retried.
func TestJob_StopsOnContextCancellation(t *testing.T) {
	rows := make([]db.EmailOutbox, 0, 10)
	for i := 0; i < 10; i++ {
		rows = append(rows, testRow("bulk@example.com", 1))
	}
	store := &mockStore{claimBatches: [][]db.EmailOutbox{rows}}
	sender := &fakeSender{}
	j := newJob(store, sender)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := j.Run(ctx); err == nil {
		t.Fatal("Run returned nil for an already-cancelled context")
	}
	if n := len(sender.messages()); n > 1 {
		t.Errorf("sent %d messages on a cancelled context, want it to stop immediately", n)
	}
}

// ---- regression: last_error must be storable ----

// truncateError cuts a byte slice, which can land in the middle of a
// multi-byte rune. The first version stripped trailing continuation bytes but
// left a dangling LEAD byte, producing invalid UTF-8 — and Postgres rejects
// that with SQLSTATE 22021, so the write is lost. On the terminal path that
// is unbounded: a row at its attempt budget whose upstream error is long and
// non-ASCII can never be recorded as failed, stays pending, and is re-claimed
// and re-sent to the provider forever.
//
// The original test here only used ASCII, so it never caught this.
func TestTruncateError_AlwaysProducesValidUTF8(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"euro at the cut", strings.Repeat("a", maxLastErrorLen-1) + "€" + strings.Repeat("b", 100)},
		{"thai at the cut", strings.Repeat("a", maxLastErrorLen-1) + "ก" + strings.Repeat("b", 100)},
		{"emoji at the cut", strings.Repeat("a", maxLastErrorLen-2) + "🔥" + strings.Repeat("b", 100)},
		{"continuation at the cut", strings.Repeat("a", maxLastErrorLen-3) + "€€" + strings.Repeat("b", 100)},
		{"all multibyte", strings.Repeat("€", 400)},
		{"emoji run", strings.Repeat("🔥", 300)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateError(tc.in)
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8 (len %d, last byte 0x%X) — Postgres will reject this", len(got), got[len(got)-1])
			}
			if len(got) > maxLastErrorLen {
				t.Errorf("length %d exceeds the %d cap", len(got), maxLastErrorLen)
			}
		})
	}
}

// Sweep every cut position through a multi-byte string: whatever the offset,
// the result must be both storable and within the cap.
func TestTruncateError_ValidAtEveryCutOffset(t *testing.T) {
	for pad := 0; pad < 8; pad++ {
		in := strings.Repeat("a", maxLastErrorLen-pad) + strings.Repeat("🔥€ก", 50)
		got := truncateError(in)
		if !utf8.ValidString(got) {
			t.Fatalf("pad=%d produced invalid UTF-8", pad)
		}
		if len(got) > maxLastErrorLen {
			t.Fatalf("pad=%d produced %d bytes, over the %d cap", pad, len(got), maxLastErrorLen)
		}
	}
}

// A legitimate U+FFFD in the message is a real rune, not a broken tail, and
// must survive rather than being eaten by the trim.
func TestTruncateError_KeepsGenuineReplacementChar(t *testing.T) {
	in := strings.Repeat("a", 100) + "\uFFFD"
	if got := truncateError(in); got != in {
		t.Errorf("a short message was altered: %q", got[len(got)-10:])
	}
}

// ---- regression: outcome writes must survive cancellation ----

// If ctx is cancelled while the provider call is in flight, the follow-up
// write recording that failure must still land. Using the dead ctx loses
// last_error silently and leaves the row looking untouched.
func TestJob_RecordsOutcomeWhenCancelledDuringSend(t *testing.T) {
	row := testRow("cancelled@example.com", 1)
	store := &mockStore{claimBatches: [][]db.EmailOutbox{{row}}}

	ctx, cancel := context.WithCancel(context.Background())
	sender := &cancellingSender{cancel: cancel, err: errors.New("upstream timeout")}

	j := newJob(store, sender)
	_, _ = j.Run(ctx)

	if len(store.rescheduled) != 1 {
		t.Fatalf("rescheduled %d rows, want 1 — the outcome write was lost to the cancelled context", len(store.rescheduled))
	}
	if le := store.rescheduled[0].LastError; le == nil || !strings.Contains(*le, "upstream timeout") {
		t.Errorf("LastError = %v, want the upstream reason recorded", le)
	}
}

// ---- regression: prune must keep up with dispatch ----

// One sweep is capped at pruneBatchSize, but the sweep only runs once an
// hour. Capping the HOUR at pruneBatchSize rows put the ceiling at 1000
// deletions/hour against a dispatch ceiling of 4800/hour, so email_outbox
// grew without bound at any sustained volume. The sweep must drain.
func TestJob_PruneDrainsTheBacklog(t *testing.T) {
	store := &mockStore{pruneReturns: []int64{pruneBatchSize, pruneBatchSize, 7}}
	j := newJob(store, &fakeSender{})

	res, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.pruneCalls) != 3 {
		t.Fatalf("pruned %d times, want 3 (two full batches then a short one)", len(store.pruneCalls))
	}
	if v, _ := attrValue(res.Attrs, "pruned"); v != int64(2*pruneBatchSize+7) {
		t.Errorf("Attrs pruned = %v, want %d", v, 2*pruneBatchSize+7)
	}
}

// Draining must still be bounded, or one sweep can hold the job lock for the
// whole interval against a pathological backlog.
func TestJob_PruneStopsAtTheBatchCap(t *testing.T) {
	full := make([]int64, maxPruneBatches+10)
	for i := range full {
		full[i] = pruneBatchSize
	}
	store := &mockStore{pruneReturns: full}
	j := newJob(store, &fakeSender{})

	if _, err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.pruneCalls) > maxPruneBatches {
		t.Errorf("pruned %d times, want at most %d", len(store.pruneCalls), maxPruneBatches)
	}
}
