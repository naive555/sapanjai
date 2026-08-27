package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// fakeDBTX is a minimal db.DBTX good enough to back a real *db.Queries in a
// unit test, so Register's WithTx body (q.CreateUser then, via
// SendVerificationEmail, q.EnqueueEmail) can be exercised without a real
// Postgres connection — the only way to observe that both calls land on the
// SAME *db.Queries (i.e. would commit in the same transaction) without
// standing up an integration test.
//
// It recognizes exactly the two INSERT statements Register's transaction
// issues, by a substring of their SQL, and fabricates a plausible RETURNING
// row from the bound params. Anything else is a test bug — it errors loudly
// rather than returning a zero value.
type fakeDBTX struct {
	createUserCalls   []db.CreateUserParams
	enqueueEmailCalls []db.EnqueueEmailParams
}

func (f *fakeDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, fmt.Errorf("fakeDBTX: Exec not supported: %s", sql)
}

func (f *fakeDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("fakeDBTX: Query not supported: %s", sql)
}

func (f *fakeDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO users"):
		arg := db.CreateUserParams{
			Email:        args[0].(string),
			PasswordHash: args[1].(string),
			DisplayName:  args[2].(*string),
		}
		f.createUserCalls = append(f.createUserCalls, arg)
		now := time.Now().UTC()
		return fakeRow{values: []any{
			uuid.New(), arg.Email, arg.PasswordHash, arg.DisplayName, false, now, now,
		}}

	case strings.Contains(sql, "INSERT INTO email_outbox"):
		arg := db.EnqueueEmailParams{
			ToAddress: args[0].(string),
			Subject:   args[1].(string),
			BodyHtml:  args[2].(*string),
			BodyText:  args[3].(*string),
		}
		f.enqueueEmailCalls = append(f.enqueueEmailCalls, arg)
		now := time.Now().UTC()
		return fakeRow{values: []any{
			uuid.New(), arg.ToAddress, arg.Subject, arg.BodyHtml, arg.BodyText,
			"pending", int32(0), (*string)(nil), now, pgtype.Timestamp{}, now, now,
		}}

	default:
		return fakeRow{err: fmt.Errorf("fakeDBTX: unsupported query: %s", sql)}
	}
}

// fakeRow is a pgx.Row backed by a fixed slice of already-typed values,
// scanned positionally into dest.
type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("fakeRow: Scan got %d dest, have %d values", len(dest), len(r.values))
	}
	for i, d := range dest {
		if err := scanInto(d, r.values[i]); err != nil {
			return fmt.Errorf("fakeRow: column %d: %w", i, err)
		}
	}
	return nil
}

func scanInto(dest, value any) error {
	switch v := dest.(type) {
	case *uuid.UUID:
		*v = value.(uuid.UUID)
	case *string:
		*v = value.(string)
	case **string:
		*v = value.(*string)
	case *bool:
		*v = value.(bool)
	case *time.Time:
		*v = value.(time.Time)
	case *int32:
		*v = value.(int32)
	case *pgtype.Timestamp:
		*v = value.(pgtype.Timestamp)
	default:
		return fmt.Errorf("unsupported dest type %T", dest)
	}
	return nil
}

// withFakeTx returns an authStore.WithTx implementation that runs fn
// against a *db.Queries backed by a fresh fakeDBTX, and hands the fakeDBTX
// back to the caller (via the out pointer) so the test can assert on what
// was inserted.
func withFakeTx(out **fakeDBTX) func(ctx context.Context, fn func(q *db.Queries) error) error {
	return func(ctx context.Context, fn func(q *db.Queries) error) error {
		dbtx := &fakeDBTX{}
		*out = dbtx
		return fn(db.New(dbtx))
	}
}
