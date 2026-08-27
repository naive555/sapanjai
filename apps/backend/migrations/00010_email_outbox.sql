-- +goose Up
-- The transactional outbox behind email verification and password reset.
--
-- Sapanjai has no job queue, so this table plus the email-dispatch job in
-- internal/job/emaildispatch is the queue: POST /auth/register inserts the
-- user row and its outbox row in ONE transaction, which is what guarantees a
-- user cannot exist without its verification mail being queued. A goroutine
-- firing off a send would lose the mail on any pod restart between the two.
--
-- body_html/body_text are NULLABLE on purpose. A rendered verification body
-- contains a live, single-use bearer token, so MarkEmailSent nulls both
-- columns in the same statement that flips the status: the row survives as an
-- audit trail (recipient, subject, when, how many attempts) with the secret
-- stripped out. PruneEmailOutbox then deletes the row entirely once it is
-- past the retention window.
--
-- Claiming is lease-based rather than a 'sending' status. A 'sending' state
-- needs a reaper for rows whose worker died mid-flight; instead the claim
-- query pushes next_attempt_at forward by a lease window in the same UPDATE,
-- so a crashed run's rows simply become claimable again when the lease
-- lapses. Self-healing, and no second job to own.
CREATE TABLE "email_outbox" (
	"id" uuid PRIMARY KEY DEFAULT gen_random_uuid() NOT NULL,
	"to_address" text NOT NULL,
	"subject" text NOT NULL,
	"body_html" text,
	"body_text" text,
	"status" text DEFAULT 'pending' NOT NULL,
	"attempts" integer DEFAULT 0 NOT NULL,
	"last_error" text,
	"next_attempt_at" timestamp DEFAULT now() NOT NULL,
	"sent_at" timestamp,
	"created_at" timestamp DEFAULT now() NOT NULL,
	"updated_at" timestamp DEFAULT now() NOT NULL,
	CONSTRAINT "email_outbox_status_check" CHECK ("status" IN ('pending', 'sent', 'failed'))
);

-- The claim query is WHERE status = 'pending' AND next_attempt_at <= now()
-- ORDER BY next_attempt_at LIMIT n. Partial on 'pending' (mirroring
-- idx_sessions_revoked_created_at in 00006) because sent/failed rows are the
-- overwhelming majority of the table in steady state and are never claimed.
CREATE INDEX IF NOT EXISTS "idx_email_outbox_claim" ON "email_outbox" ("next_attempt_at") WHERE "status" = 'pending';

-- Supports the prune sweep: WHERE status IN ('sent','failed') AND updated_at < …
CREATE INDEX IF NOT EXISTS "idx_email_outbox_prune" ON "email_outbox" ("status", "updated_at");

-- +goose Down
DROP TABLE "email_outbox";
