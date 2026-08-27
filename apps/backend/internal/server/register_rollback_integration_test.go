package server_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestIntegration_RegisterRollsBackWhenEnqueueFails covers the one property
// Register's transaction exists for, and the one its unit-test mocks
// structurally cannot show: that a failure enqueuing the verification email
// takes the user row down with it. The mocks in
// internal/module/auth/tx_test.go establish that both statements run
// against the same transaction-scoped querier; only real Postgres can show
// that the transaction actually rolls back.
//
// The failure is injected with a temporary BEFORE INSERT trigger on
// email_outbox that raises for one sentinel address, so the insert fails
// exactly the way a constraint violation or a dead connection would —
// after CreateUser has already succeeded inside the same transaction.
func TestIntegration_RegisterRollsBackWhenEnqueueFails(t *testing.T) {
	srv, _, store := setupTestServer(t)
	ctx := context.Background()

	email := fmt.Sprintf("rollback-%s@example.com", uuid.NewString())

	if _, err := store.Pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION reject_sentinel_outbox() RETURNS trigger AS $$
		BEGIN
			IF NEW.to_address = `+quoteLiteral(email)+` THEN
				RAISE EXCEPTION 'injected outbox failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		CREATE TRIGGER reject_sentinel_outbox_trigger
		BEFORE INSERT ON email_outbox
		FOR EACH ROW EXECUTE FUNCTION reject_sentinel_outbox();
	`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS reject_sentinel_outbox_trigger ON email_outbox;
			DROP FUNCTION IF EXISTS reject_sentinel_outbox();
		`)
	})

	resp, _ := doJSON(t, srv.Client(), srv.URL, http.MethodPost, "/auth/register", map[string]any{
		"email":    email,
		"password": "password123",
	}, nil)

	// The enqueue blew up inside the transaction, so registration fails.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	// The point of the test: the user row must not have survived the
	// rolled-back transaction.
	var users int
	if err := store.Pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE email = $1`, email).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 0 {
		t.Fatalf("users with email %s = %d, want 0 (the transaction must have rolled the user insert back)", email, users)
	}

	// ...and neither did a half-written outbox row.
	var outbox int
	if err := store.Pool.QueryRow(ctx,
		`SELECT count(*) FROM email_outbox WHERE to_address = $1`, email).Scan(&outbox); err != nil {
		t.Fatalf("count email_outbox: %v", err)
	}
	if outbox != 0 {
		t.Fatalf("email_outbox rows for %s = %d, want 0", email, outbox)
	}

	// The address must still be registrable once the injected failure is
	// gone — a rolled-back attempt must not leave the email "taken".
	if _, err := store.Pool.Exec(ctx, `DROP TRIGGER reject_sentinel_outbox_trigger ON email_outbox`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	retry, _ := doJSON(t, srv.Client(), srv.URL, http.MethodPost, "/auth/register", map[string]any{
		"email":    email,
		"password": "password123",
	}, nil)
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", retry.StatusCode)
	}
}

// quoteLiteral renders s as a single-quoted SQL literal for embedding in a
// DDL body, where bind parameters are not available. Test-only, and only
// ever called with a uuid-derived address this file generates itself.
func quoteLiteral(s string) string {
	return "'" + s + "'"
}
