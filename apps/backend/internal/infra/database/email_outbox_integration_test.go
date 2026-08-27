package database_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/migrations"
)

// setupOutbox skips unless DATABASE_URL is set, runs migrations, and hands
// back an empty email_outbox.
//
// It empties the table rather than scoping each assertion to a marker
// address, because ClaimPendingEmails deliberately has no WHERE clause on the
// caller's rows — it claims whatever is due, which is the whole point. Any
// row left behind by another test would be claimed by this one. The same
// DATABASE_URL already has migrations run against it by every other
// integration test in this package, so it is understood to be disposable.
func setupOutbox(t *testing.T) (*database.Store, *pgxpool.Pool, context.Context) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open database/sql: %v", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	pool, err := database.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, "DELETE FROM email_outbox"); err != nil {
		t.Fatalf("clear email_outbox: %v", err)
	}

	return database.NewStore(pool), pool, ctx
}

func strptr(s string) *string { return &s }

func enqueue(t *testing.T, store *database.Store, ctx context.Context, to string) db.EmailOutbox {
	t.Helper()
	row, err := store.EnqueueEmail(ctx, db.EnqueueEmailParams{
		ToAddress: to,
		Subject:   "Verify your email address",
		BodyHtml:  strptr("<p>hi</p><a href=\"https://app/verify-email?token=tok\">go</a>"),
		BodyText:  strptr("hi https://app/verify-email?token=tok"),
	})
	if err != nil {
		t.Fatalf("EnqueueEmail: %v", err)
	}
	return row
}

// backdate pushes a row's next_attempt_at into the past, standing in for an
// elapsed lease (a dispatch run that claimed the row and then died).
func backdate(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"UPDATE email_outbox SET next_attempt_at = now() - INTERVAL '1 hour' WHERE id = $1", id); err != nil {
		t.Fatalf("backdate next_attempt_at: %v", err)
	}
}

func TestEmailOutbox_EnqueueStartsPending(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	row := enqueue(t, store, ctx, "enqueue@example.com")

	if row.ID == uuid.Nil {
		t.Error("EnqueueEmail returned a nil id")
	}
	if row.Status != "pending" {
		t.Errorf("Status = %q, want pending", row.Status)
	}
	if row.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0", row.Attempts)
	}
	if row.SentAt.Valid {
		t.Error("SentAt is set on a freshly enqueued row")
	}
	if row.LastError != nil {
		t.Errorf("LastError = %v, want nil", *row.LastError)
	}
	if row.BodyHtml == nil || row.BodyText == nil {
		t.Fatal("bodies must be stored so the dispatcher has something to send")
	}
}

// A fresh row is due immediately: next_attempt_at defaults to now(), so the
// very next dispatch tick picks it up rather than waiting out a lease.
func TestEmailOutbox_EnqueuedRowIsImmediatelyClaimable(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	enqueue(t, store, ctx, "due-now@example.com")

	claimed, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 60, BatchSize: 10})
	if err != nil {
		t.Fatalf("ClaimPendingEmails: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(claimed))
	}
}

// The claim takes out a lease in the same statement: attempts goes up and the
// row is pushed beyond the current tick.
func TestEmailOutbox_ClaimIncrementsAttemptsAndTakesLease(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	enqueue(t, store, ctx, "lease@example.com")
	before := time.Now().UTC()

	claimed, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 120, BatchSize: 10})
	if err != nil {
		t.Fatalf("ClaimPendingEmails: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(claimed))
	}

	row := claimed[0]
	if row.Attempts != 1 {
		t.Errorf("Attempts = %d after one claim, want 1", row.Attempts)
	}
	if row.Status != "pending" {
		t.Errorf("Status = %q after claim, want it to stay pending", row.Status)
	}
	// Timestamps are "timestamp without time zone" and pgx tags them UTC, so
	// both sides of the comparison must be UTC or the boundary silently
	// shifts by the host's offset (see the RotateSession comment in
	// internal/module/auth/service.go).
	lease := row.NextAttemptAt.Sub(before)
	if lease < 60*time.Second || lease > 180*time.Second {
		t.Errorf("lease pushed next_attempt_at by %v, want roughly 120s", lease)
	}
	// The retry needs the body, so a claim must not strip it.
	if row.BodyHtml == nil || row.BodyText == nil {
		t.Error("claim stripped the bodies; a retry would send an empty mail")
	}
}

// The lease is what stops a second dispatcher — or the very next tick —
// re-sending a mail that is already in flight.
func TestEmailOutbox_LeaseHidesRowFromSecondClaim(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	enqueue(t, store, ctx, "inflight@example.com")

	first, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 300, BatchSize: 10})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim got %d rows, want 1", len(first))
	}

	second, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 300, BatchSize: 10})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second claim got %d rows while the lease is held, want 0", len(second))
	}
}

// The self-healing property: a dispatcher that claims a row and then dies
// leaves it leased, and it becomes claimable again on its own once the lease
// lapses. No 'sending' status, no reaper.
func TestEmailOutbox_ExpiredLeaseBecomesClaimableAgain(t *testing.T) {
	store, pool, ctx := setupOutbox(t)

	row := enqueue(t, store, ctx, "crashed@example.com")

	if _, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 300, BatchSize: 10}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	backdate(t, pool, ctx, row.ID) // the worker died; the lease lapses

	again, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 300, BatchSize: 10})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("reclaim got %d rows, want 1", len(again))
	}
	if again[0].Attempts != 2 {
		t.Errorf("Attempts = %d after a reclaim, want 2", again[0].Attempts)
	}
}

func TestEmailOutbox_ClaimRespectsBatchSize(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	for i := 0; i < 5; i++ {
		enqueue(t, store, ctx, "batch@example.com")
	}

	claimed, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 60, BatchSize: 2})
	if err != nil {
		t.Fatalf("ClaimPendingEmails: %v", err)
	}
	if len(claimed) != 2 {
		t.Errorf("claimed %d rows with BatchSize 2, want 2", len(claimed))
	}
}

// Oldest-due first, so a row that has been waiting cannot be starved by newer
// arrivals when the backlog is bigger than one batch.
func TestEmailOutbox_ClaimTakesOldestDueFirst(t *testing.T) {
	store, pool, ctx := setupOutbox(t)

	newest := enqueue(t, store, ctx, "newest@example.com")
	oldest := enqueue(t, store, ctx, "oldest@example.com")

	if _, err := pool.Exec(ctx,
		"UPDATE email_outbox SET next_attempt_at = now() - INTERVAL '10 minutes' WHERE id = $1", oldest.ID); err != nil {
		t.Fatalf("age the oldest row: %v", err)
	}

	claimed, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 60, BatchSize: 1})
	if err != nil {
		t.Fatalf("ClaimPendingEmails: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rows, want 1", len(claimed))
	}
	if claimed[0].ID != oldest.ID {
		t.Errorf("claimed the newer row (%s); want the oldest due (%s)", claimed[0].ID, oldest.ID)
	}
	_ = newest
}

// A row scheduled into the future — a backoff after a failed send — must not
// be claimed early.
func TestEmailOutbox_ClaimSkipsRowsNotYetDue(t *testing.T) {
	store, pool, ctx := setupOutbox(t)

	row := enqueue(t, store, ctx, "backoff@example.com")
	if _, err := pool.Exec(ctx,
		"UPDATE email_outbox SET next_attempt_at = now() + INTERVAL '10 minutes' WHERE id = $1", row.ID); err != nil {
		t.Fatalf("schedule into the future: %v", err)
	}

	claimed, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 60, BatchSize: 10})
	if err != nil {
		t.Fatalf("ClaimPendingEmails: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %d rows scheduled into the future, want 0", len(claimed))
	}
}

func TestEmailOutbox_ClaimIgnoresSentAndFailedRows(t *testing.T) {
	store, pool, ctx := setupOutbox(t)

	sent := enqueue(t, store, ctx, "already-sent@example.com")
	failed := enqueue(t, store, ctx, "already-failed@example.com")

	if err := store.MarkEmailSent(ctx, sent.ID); err != nil {
		t.Fatalf("MarkEmailSent: %v", err)
	}
	if err := store.MarkEmailFailed(ctx, db.MarkEmailFailedParams{ID: failed.ID, LastError: strptr("gave up")}); err != nil {
		t.Fatalf("MarkEmailFailed: %v", err)
	}
	backdate(t, pool, ctx, sent.ID)
	backdate(t, pool, ctx, failed.ID)

	claimed, err := store.ClaimPendingEmails(ctx, db.ClaimPendingEmailsParams{LeaseSeconds: 60, BatchSize: 10})
	if err != nil {
		t.Fatalf("ClaimPendingEmails: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %d terminal rows, want 0", len(claimed))
	}
}

// The security-relevant one: a delivered body still holds a live single-use
// token, so the same statement that records success must drop it.
func TestEmailOutbox_MarkSentNullsBodies(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	row := enqueue(t, store, ctx, "sent@example.com")
	if err := store.MarkEmailSent(ctx, row.ID); err != nil {
		t.Fatalf("MarkEmailSent: %v", err)
	}

	got, err := store.GetEmailByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("GetEmailByID: %v", err)
	}
	if got.Status != "sent" {
		t.Errorf("Status = %q, want sent", got.Status)
	}
	if !got.SentAt.Valid {
		t.Error("SentAt was not recorded")
	}
	if got.BodyHtml != nil {
		t.Errorf("body_html survived the send: %q", *got.BodyHtml)
	}
	if got.BodyText != nil {
		t.Errorf("body_text survived the send: %q", *got.BodyText)
	}
	// The audit trail has to remain useful with the secret stripped.
	if got.ToAddress != "sent@example.com" || got.Subject == "" {
		t.Error("recipient/subject should survive as the audit trail")
	}
}

// An undelivered token is no less live than a delivered one.
func TestEmailOutbox_MarkFailedNullsBodiesAndRecordsError(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	row := enqueue(t, store, ctx, "failed@example.com")
	if err := store.MarkEmailFailed(ctx, db.MarkEmailFailedParams{
		ID: row.ID, LastError: strptr("domain not verified"),
	}); err != nil {
		t.Fatalf("MarkEmailFailed: %v", err)
	}

	got, err := store.GetEmailByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("GetEmailByID: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.LastError == nil || *got.LastError != "domain not verified" {
		t.Errorf("LastError = %v, want the recorded reason", got.LastError)
	}
	if got.BodyHtml != nil || got.BodyText != nil {
		t.Error("bodies survived a terminal failure")
	}
	if got.SentAt.Valid {
		t.Error("SentAt is set on a row that never sent")
	}
}

// A retryable failure is the opposite of the two above: the row must keep its
// body, or the retry delivers an empty email.
func TestEmailOutbox_RescheduleKeepsBodyAndStaysPending(t *testing.T) {
	store, _, ctx := setupOutbox(t)

	row := enqueue(t, store, ctx, "retry@example.com")
	before := time.Now().UTC()

	if err := store.RescheduleEmail(ctx, db.RescheduleEmailParams{
		ID: row.ID, LastError: strptr("upstream 500"), BackoffSeconds: 90,
	}); err != nil {
		t.Fatalf("RescheduleEmail: %v", err)
	}

	got, err := store.GetEmailByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("GetEmailByID: %v", err)
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want it to stay pending so the row is retried", got.Status)
	}
	if got.BodyHtml == nil || got.BodyText == nil {
		t.Fatal("reschedule stripped the body; the retry would send an empty mail")
	}
	if got.LastError == nil || *got.LastError != "upstream 500" {
		t.Errorf("LastError = %v, want the recorded reason", got.LastError)
	}
	backoff := got.NextAttemptAt.Sub(before)
	if backoff < 45*time.Second || backoff > 135*time.Second {
		t.Errorf("backoff pushed next_attempt_at by %v, want roughly 90s", backoff)
	}
}

func TestEmailOutbox_PruneDeletesOnlyOldTerminalRows(t *testing.T) {
	store, pool, ctx := setupOutbox(t)

	oldSent := enqueue(t, store, ctx, "old-sent@example.com")
	oldFailed := enqueue(t, store, ctx, "old-failed@example.com")
	freshSent := enqueue(t, store, ctx, "fresh-sent@example.com")
	stillPending := enqueue(t, store, ctx, "still-pending@example.com")

	if err := store.MarkEmailSent(ctx, oldSent.ID); err != nil {
		t.Fatalf("MarkEmailSent: %v", err)
	}
	if err := store.MarkEmailFailed(ctx, db.MarkEmailFailedParams{ID: oldFailed.ID, LastError: strptr("x")}); err != nil {
		t.Fatalf("MarkEmailFailed: %v", err)
	}
	if err := store.MarkEmailSent(ctx, freshSent.ID); err != nil {
		t.Fatalf("MarkEmailSent: %v", err)
	}

	// Age the two "old" rows past the retention window.
	if _, err := pool.Exec(ctx,
		"UPDATE email_outbox SET updated_at = now() - INTERVAL '30 days' WHERE id = ANY($1)",
		[]uuid.UUID{oldSent.ID, oldFailed.ID}); err != nil {
		t.Fatalf("age terminal rows: %v", err)
	}
	// A pending row that is somehow ancient must still never be pruned.
	if _, err := pool.Exec(ctx,
		"UPDATE email_outbox SET updated_at = now() - INTERVAL '30 days' WHERE id = $1", stillPending.ID); err != nil {
		t.Fatalf("age the pending row: %v", err)
	}

	deleted, err := store.PruneEmailOutbox(ctx, db.PruneEmailOutboxParams{
		RetentionSeconds: int32((168 * time.Hour).Seconds()),
		BatchSize:        100,
	})
	if err != nil {
		t.Fatalf("PruneEmailOutbox: %v", err)
	}
	if deleted != 2 {
		t.Errorf("pruned %d rows, want 2 (the aged sent + failed)", deleted)
	}

	for _, keep := range []struct {
		name string
		id   uuid.UUID
	}{{"fresh sent", freshSent.ID}, {"pending", stillPending.ID}} {
		if _, err := store.GetEmailByID(ctx, keep.id); err != nil {
			t.Errorf("prune deleted the %s row: %v", keep.name, err)
		}
	}
}

func TestEmailOutbox_PruneRespectsBatchSize(t *testing.T) {
	store, pool, ctx := setupOutbox(t)

	for i := 0; i < 5; i++ {
		row := enqueue(t, store, ctx, "prunable@example.com")
		if err := store.MarkEmailSent(ctx, row.ID); err != nil {
			t.Fatalf("MarkEmailSent: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE email_outbox SET updated_at = now() - INTERVAL '30 days'"); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	deleted, err := store.PruneEmailOutbox(ctx, db.PruneEmailOutboxParams{
		RetentionSeconds: int32((168 * time.Hour).Seconds()),
		BatchSize:        2,
	})
	if err != nil {
		t.Fatalf("PruneEmailOutbox: %v", err)
	}
	if deleted != 2 {
		t.Errorf("pruned %d rows with BatchSize 2, want 2", deleted)
	}
}

// The CHECK constraint is the last line of defence against a typo'd status
// reaching the table and making a row permanently unclaimable and unprunable.
func TestEmailOutbox_RejectsUnknownStatus(t *testing.T) {
	store, pool, ctx := setupOutbox(t)

	row := enqueue(t, store, ctx, "bad-status@example.com")

	if _, err := pool.Exec(ctx, "UPDATE email_outbox SET status = 'sending' WHERE id = $1", row.ID); err == nil {
		t.Error("the table accepted status='sending'; the CHECK constraint is not doing its job")
	}
}
