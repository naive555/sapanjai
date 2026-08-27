package auth

import (
	"context"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database/db"
)

// mockTxQuerier stands in for the db.Querier a service method's WithTx body
// receives, so the body can be exercised without a real Postgres
// connection. It embeds db.Querier (left nil) and overrides only the
// statements the transactions under test actually issue: Register's
// (CreateUser, then EnqueueEmail via SendVerificationEmail) and
// ResetPassword's (UpdateUserPassword, MarkUserVerified,
// RevokeAllUserSessions).
//
// Embedding rather than implementing the full generated surface keeps this
// to the methods that matter, and preserves the loud-failure property: any
// other query reached from inside a transaction body hits the nil embedded
// interface and panics, naming the method, instead of silently returning a
// zero value.
//
// Note what these mocks can and cannot show. They establish that the
// statements all run against the same transaction-scoped querier — not that
// the transaction rolls back when one of them fails. That property is
// covered against real Postgres in
// internal/server/register_rollback_integration_test.go.
type mockTxQuerier struct {
	db.Querier

	createUserCalls          []db.CreateUserParams
	enqueueEmailCalls        []db.EnqueueEmailParams
	updateUserPasswordCalls  []db.UpdateUserPasswordParams
	markUserVerifiedCalls    []uuid.UUID
	revokeAllUserSessionsIDs []uuid.UUID

	createUserResult db.User
	createUserErr    error
	enqueueEmailErr  error
}

func (m *mockTxQuerier) CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
	m.createUserCalls = append(m.createUserCalls, arg)
	if m.createUserErr != nil {
		return db.User{}, m.createUserErr
	}
	user := m.createUserResult
	if user.ID == uuid.Nil {
		user = db.User{ID: uuid.New(), Email: arg.Email, PasswordHash: arg.PasswordHash, DisplayName: arg.DisplayName}
	}
	return user, nil
}

func (m *mockTxQuerier) EnqueueEmail(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
	m.enqueueEmailCalls = append(m.enqueueEmailCalls, arg)
	if m.enqueueEmailErr != nil {
		return db.EmailOutbox{}, m.enqueueEmailErr
	}
	return db.EmailOutbox{ID: uuid.New(), ToAddress: arg.ToAddress, Subject: arg.Subject}, nil
}

func (m *mockTxQuerier) UpdateUserPassword(ctx context.Context, arg db.UpdateUserPasswordParams) error {
	m.updateUserPasswordCalls = append(m.updateUserPasswordCalls, arg)
	return nil
}

func (m *mockTxQuerier) MarkUserVerified(ctx context.Context, id uuid.UUID) error {
	m.markUserVerifiedCalls = append(m.markUserVerifiedCalls, id)
	return nil
}

func (m *mockTxQuerier) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error {
	m.revokeAllUserSessionsIDs = append(m.revokeAllUserSessionsIDs, userID)
	return nil
}

// withMockTx returns an authStore.WithTx implementation that runs fn
// against a fresh mockTxQuerier and hands it back through out, so the test
// can assert on what the transaction body did.
func withMockTx(out **mockTxQuerier) func(ctx context.Context, fn func(q db.Querier) error) error {
	return func(ctx context.Context, fn func(q db.Querier) error) error {
		q := &mockTxQuerier{}
		*out = q
		return fn(q)
	}
}
