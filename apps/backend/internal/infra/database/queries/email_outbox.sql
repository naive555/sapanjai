-- name: EnqueueEmail :one
INSERT INTO email_outbox (to_address, subject, body_html, body_text)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetEmailByID :one
SELECT * FROM email_outbox WHERE id = $1;

-- name: ClaimPendingEmails :many
-- Claims a batch by taking out a lease: attempts is incremented and
-- next_attempt_at is pushed forward in the same statement, so a run that dies
-- mid-send leaves rows that become claimable again on their own when the
-- lease lapses -- no 'sending' status and no reaper job. FOR UPDATE SKIP
-- LOCKED keeps two dispatchers off the same row even though the worker's
-- Redis lock already serialises runs.
UPDATE email_outbox SET
    attempts = attempts + 1,
    next_attempt_at = now() + (sqlc.arg(lease_seconds)::int * INTERVAL '1 second'),
    updated_at = now()
WHERE id IN (
    SELECT id FROM email_outbox
    WHERE status = 'pending' AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkEmailSent :exec
-- Nulls both bodies in the same statement that flips the status: a delivered
-- verification body holds a live single-use token, and the row is kept for its
-- audit value (recipient, subject, attempts), not for its content.
UPDATE email_outbox SET
    status = 'sent',
    sent_at = now(),
    updated_at = now(),
    body_html = NULL,
    body_text = NULL,
    last_error = NULL
WHERE id = $1;

-- name: RescheduleEmail :exec
-- A retryable failure: record why and push the next attempt out by the
-- caller's backoff. Status stays 'pending' so the row is claimed again.
UPDATE email_outbox SET
    last_error = sqlc.arg(last_error),
    next_attempt_at = now() + (sqlc.arg(backoff_seconds)::int * INTERVAL '1 second'),
    updated_at = now()
WHERE id = sqlc.arg(id);

-- name: MarkEmailFailed :exec
-- Terminal: the attempt budget is spent. Bodies are dropped for the same
-- reason as MarkEmailSent -- an undelivered token is no less live.
UPDATE email_outbox SET
    status = 'failed',
    last_error = sqlc.arg(last_error),
    updated_at = now(),
    body_html = NULL,
    body_text = NULL
WHERE id = sqlc.arg(id);

-- name: PruneEmailOutbox :execrows
DELETE FROM email_outbox
WHERE id IN (
    SELECT id FROM email_outbox
    WHERE status IN ('sent', 'failed')
      AND updated_at < now() - (sqlc.arg(retention_seconds)::int * INTERVAL '1 second')
    LIMIT sqlc.arg(batch_size)
);
