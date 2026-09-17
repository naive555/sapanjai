-- name: ClaimStripeEvent :one
-- Webhook idempotency (step 7 of
-- .claude/plans/2026-09-13-billing-and-usage-metering.md). Stripe retries,
-- so the same event id can arrive many times; the id is the primary key of
-- stripe_events (migration 00013), so the second delivery conflicts.
--
-- ON CONFLICT DO NOTHING ... RETURNING makes "already processed" a
-- pgx.ErrNoRows rather than a driver-level unique-violation the caller
-- would have to sniff a SQLSTATE for -- and, crucially, it does NOT abort
-- the surrounding transaction the way a raised 23505 would.
--
-- The claim is issued INSIDE the same transaction as the state change it
-- guards (billing.Service.reconcile). That ordering is the whole point: a
-- failure after the claim rolls the claim back too, so Stripe's retry
-- reprocesses the event instead of finding it marked handled and silently
-- doing nothing. Claim-then-process in separate transactions loses the
-- state change forever with no error anywhere.
INSERT INTO stripe_events (id, type)
VALUES (@id::text, @type::text)
ON CONFLICT (id) DO NOTHING
RETURNING id;
