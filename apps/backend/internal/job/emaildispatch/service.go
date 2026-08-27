// Package emaildispatch drains the email_outbox table: it claims a batch of
// due rows, hands each to an email.Sender, and records the outcome back onto
// the row.
//
// This is the queue runner Sapanjai does not otherwise have. The API never
// sends mail — POST /auth/register inserts the user and its outbox row in one
// transaction and returns — so everything that actually reaches a mailbox
// goes through here.
package emaildispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/email"
	"github.com/sapanjai/backend/internal/worker"
)

var (
	_ dispatchStore = (*database.Store)(nil)
	_ worker.Job    = (*Job)(nil)
)

const (
	// baseBackoff and maxBackoff bound the retry schedule: the nth failure
	// waits baseBackoff << (n-1), clamped. Long enough that a flapping
	// upstream is not hammered, short enough that a verification mail still
	// arrives while the person is waiting on the signup page.
	baseBackoff = 30 * time.Second
	maxBackoff  = 30 * time.Minute

	// maxBackoffShift bounds the exponent passed to backoffFor's shift so it
	// never has to compute a shift wide enough to overflow (or silently wrap
	// to zero) a time.Duration. baseBackoff<<7 (1920s) is already past
	// maxBackoff, so any exponent beyond this can short-circuit straight to
	// the ceiling without ever performing the shift.
	maxBackoffShift = 6

	// pruneInterval throttles the retention sweep. Every replica keeps this
	// in memory rather than in Redis: the sweep is idempotent, the worker's
	// job lock already serialises runs, and a redundant DELETE costs nothing.
	pruneInterval = time.Hour

	// pruneBatchSize caps one retention sweep so a large backlog cannot hold
	// the job lock for the whole interval; the rest goes next hour.
	pruneBatchSize = 1000

	// maxLastErrorLen caps what is written to email_outbox.last_error. An
	// upstream that returns a whole HTML error page should not bloat the row.
	maxLastErrorLen = 500

	// minLeaseSeconds floors the claim lease. The lease must comfortably
	// outlast one send; on a very short dispatch interval, 2x the interval
	// would not.
	minLeaseSeconds = 60
)

// errMissingBody is recorded against a row whose body has already been
// cleared (MarkEmailSent/MarkEmailFailed both null it out). Seeing one here
// means a double-claim raced a previous run's write, or the row was
// hand-edited; either way it cannot be sent and must be closed out rather
// than retried forever.
var errMissingBody = errors.New("email_outbox row has no body (already sent, already failed, or edited)")

// dispatchStore is the subset of *database.Store this job needs, narrowed so
// unit tests can hand-mock it (same pattern as cleanupStore in
// internal/job/sessioncleanup and authStore in internal/module/auth).
type dispatchStore interface {
	ClaimPendingEmails(ctx context.Context, arg db.ClaimPendingEmailsParams) ([]db.EmailOutbox, error)
	MarkEmailSent(ctx context.Context, id uuid.UUID) error
	MarkEmailFailed(ctx context.Context, arg db.MarkEmailFailedParams) error
	RescheduleEmail(ctx context.Context, arg db.RescheduleEmailParams) error
	PruneEmailOutbox(ctx context.Context, arg db.PruneEmailOutboxParams) (int64, error)
}

// Job drains the outbox once per Interval.
type Job struct {
	store       dispatchStore
	sender      email.Sender
	log         *slog.Logger
	interval    time.Duration
	batchSize   int32
	maxAttempts int32
	retention   time.Duration

	// now is the clock, swapped in tests to exercise the prune throttle
	// without sleeping for an hour.
	now func() time.Time

	// lastPrunedAt is the throttle's only state. Zero means "never pruned",
	// so the first run of a fresh process always sweeps.
	lastPrunedAt time.Time
}

// New builds the dispatch job.
func New(store dispatchStore, sender email.Sender, log *slog.Logger, interval time.Duration, batchSize, maxAttempts int, retention time.Duration) *Job {
	return &Job{
		store:       store,
		sender:      sender,
		log:         log,
		interval:    interval,
		batchSize:   int32(batchSize),
		maxAttempts: int32(maxAttempts),
		retention:   retention,
		now:         time.Now,
	}
}

// Name identifies the job in logs, stats, and its Redis lock key.
func (j *Job) Name() string { return "email-dispatch" }

// Interval is how often the outbox is drained.
func (j *Job) Interval() time.Duration { return j.interval }

// Run claims one batch, attempts each send, and records every outcome.
//
// It returns an error only when it could not do its job at all: a failed
// claim, or a cancelled context (the worker cancels on per-run timeout and on
// shutdown). An individual send failure is NOT an error: it has already been
// recorded on the row with a backoff, and returning an error would make the
// worker release its lock for an immediate retry of a batch that is
// deliberately scheduled into the future.
//
// Rows the run does not reach keep their lease and are picked up by a later
// run once it lapses, so aborting part-way is always safe.
func (j *Job) Run(ctx context.Context) (worker.Result, error) {
	if err := ctx.Err(); err != nil {
		return worker.Result{}, err
	}

	rows, err := j.store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{
		LeaseSeconds: j.leaseSeconds(),
		BatchSize:    j.batchSize,
	})
	if err != nil {
		return worker.Result{}, fmt.Errorf("claim pending emails: %w", err)
	}

	claimed := len(rows)
	sent, failed := 0, 0

	for _, row := range rows {
		// Checked per row, not only before the loop: the worker cancels ctx
		// on a per-run timeout, and a batch that is most of the way through
		// should stop immediately rather than push through to the end of a
		// dead context.
		if err := ctx.Err(); err != nil {
			return worker.Result{
				Affected: int64(sent),
				Attrs:    []any{"claimed", claimed, "sent", sent, "failed", failed},
			}, err
		}

		if j.sendRow(ctx, row) {
			sent++
		} else {
			failed++
		}
	}

	result := worker.Result{
		Affected: int64(sent),
		Attrs:    []any{"claimed", claimed, "sent", sent, "failed", failed},
	}

	if deleted, pruned := j.maybePrune(ctx); pruned {
		result.Attrs = append(result.Attrs, "pruned", deleted)
	}

	return result, nil
}

// sendRow attempts one row's delivery and records the outcome. It reports
// whether the row was sent; every other outcome (missing body, retryable
// failure, exhausted budget) is recorded on the row itself and reported as
// false.
func (j *Job) sendRow(ctx context.Context, row db.EmailOutbox) bool {
	// A row already stripped of its body cannot be sent — the Sender must
	// never see an empty message. This closes the row out instead of
	// retrying it forever.
	if row.BodyHtml == nil || row.BodyText == nil {
		j.markFailed(ctx, row.ID, errMissingBody)
		return false
	}

	msg := email.Message{
		To:      row.ToAddress,
		Subject: row.Subject,
		HTML:    *row.BodyHtml,
		Text:    *row.BodyText,
		// The row id is the only value stable across a crash-and-reclaim, so
		// it is what lets the provider collapse a duplicate send.
		IdempotencyKey: row.ID.String(),
	}

	if err := j.sender.Send(ctx, msg); err != nil {
		// Only recipient, subject, and the upstream's own error text are
		// logged — never HTML/Text, which carry a live single-use token.
		j.log.WarnContext(ctx, "email send failed",
			"to", row.ToAddress, "subject", row.Subject, "attempts", row.Attempts, "error", err)

		if row.Attempts >= j.maxAttempts {
			j.markFailed(ctx, row.ID, err)
		} else {
			j.reschedule(ctx, row.ID, row.Attempts, err)
		}
		return false
	}

	if err := j.store.MarkEmailSent(ctx, row.ID); err != nil {
		// The mail is already gone; a write failure here is not the row's
		// problem to retry. Log it and move on — worst case the row's lease
		// lapses and a later run re-sends, which the IdempotencyKey above
		// makes safe.
		j.log.WarnContext(ctx, "failed to mark email sent", "id", row.ID, "error", err)
	}
	return true
}

// markFailed records a terminal failure: the attempt budget is spent, or the
// row could never have been sent in the first place.
func (j *Job) markFailed(ctx context.Context, id uuid.UUID, sendErr error) {
	msg := truncateError(sendErr.Error())
	if err := j.store.MarkEmailFailed(ctx, db.MarkEmailFailedParams{LastError: &msg, ID: id}); err != nil {
		j.log.WarnContext(ctx, "failed to mark email failed", "id", id, "error", err)
	}
}

// reschedule records a retryable failure and pushes the row's next attempt
// out by the backoff its (post-claim) attempt count has earned.
func (j *Job) reschedule(ctx context.Context, id uuid.UUID, attempts int32, sendErr error) {
	msg := truncateError(sendErr.Error())
	if err := j.store.RescheduleEmail(ctx, db.RescheduleEmailParams{
		LastError:      &msg,
		BackoffSeconds: int32(backoffFor(attempts).Seconds()),
		ID:             id,
	}); err != nil {
		j.log.WarnContext(ctx, "failed to reschedule email", "id", id, "error", err)
	}
}

// maybePrune runs the retention sweep at most once per pruneInterval. It
// reports the number of rows deleted and whether a sweep actually ran, so
// Run can omit the "pruned" attr on the runs that skip it.
func (j *Job) maybePrune(ctx context.Context) (int64, bool) {
	now := j.now()
	if !j.lastPrunedAt.IsZero() && now.Sub(j.lastPrunedAt) < pruneInterval {
		return 0, false
	}

	deleted, err := j.store.PruneEmailOutbox(ctx, db.PruneEmailOutboxParams{
		RetentionSeconds: int32(j.retention.Seconds()),
		BatchSize:        pruneBatchSize,
	})
	if err != nil {
		j.log.WarnContext(ctx, "email outbox prune failed", "error", err)
		return 0, false
	}

	j.lastPrunedAt = now
	return deleted, true
}

// backoffFor returns how long to wait before retrying a row that has already
// been attempted the given number of times. attempts is the post-claim count
// (ClaimPendingEmails increments before the send), so the first failure
// arrives here as 1.
func backoffFor(attempts int32) time.Duration {
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	// Beyond maxBackoffShift, baseBackoff<<shift is already past maxBackoff
	// (and a wide enough shift would overflow or wrap time.Duration's int64
	// to something negative or zero) — short-circuit to the ceiling instead
	// of ever performing that shift.
	if shift > maxBackoffShift {
		return maxBackoff
	}

	d := baseBackoff << uint(shift)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

// truncateError bounds what reaches email_outbox.last_error.
func truncateError(msg string) string {
	if len(msg) <= maxLastErrorLen {
		return msg
	}

	truncated := msg[:maxLastErrorLen]
	// Back off to a full rune boundary rather than splitting a multi-byte
	// UTF-8 sequence in half.
	for len(truncated) > 0 && !utf8.RuneStart(truncated[len(truncated)-1]) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// leaseSeconds is how long a claimed row is hidden from the next run: twice
// the dispatch interval, floored at minLeaseSeconds.
func (j *Job) leaseSeconds() int32 {
	lease := int32(j.interval.Seconds()) * 2
	if lease < minLeaseSeconds {
		return minLeaseSeconds
	}
	return lease
}
