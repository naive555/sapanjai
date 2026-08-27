package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// ---- RequestPasswordReset ----

func TestService_RequestPasswordReset_CooldownActive(t *testing.T) {
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			t.Fatal("GetUserByEmail must not be called while the per-email cooldown is active")
			return db.User{}, nil
		},
		enqueueEmail: func(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
			t.Fatal("EnqueueEmail must not be called while the per-email cooldown is active")
			return db.EmailOutbox{}, nil
		},
	}
	mail := newMockMail()
	mail.markResetRequested = func(ctx context.Context, email string) (bool, error) {
		return false, nil // cooldown already active
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	if err := svc.RequestPasswordReset(context.Background(), "known@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset: %v, want nil (cooldown must not be observable)", err)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record while the cooldown is active")
	}
}

func TestService_RequestPasswordReset_UnknownEmail(t *testing.T) {
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
		enqueueEmail: func(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
			t.Fatal("EnqueueEmail must not be called for an unknown email")
			return db.EmailOutbox{}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), newMockMail(), newMockRenderer(), testAppURL)

	// The response for an unknown email must be indistinguishable from a
	// known one — nil either way.
	if err := svc.RequestPasswordReset(context.Background(), "nobody@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset: %v, want nil", err)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record for an unknown email")
	}
}

func TestService_RequestPasswordReset_HappyPath(t *testing.T) {
	userID := uuid.New()
	var enqueued []db.EnqueueEmailParams
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{ID: userID, Email: email}, nil
		},
		enqueueEmail: func(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
			enqueued = append(enqueued, arg)
			return db.EmailOutbox{ID: uuid.New()}, nil
		},
	}
	mail := newMockMail()
	var tokenUserID uuid.UUID
	mail.setResetToken = func(ctx context.Context, tokenHash string, uid uuid.UUID) error {
		tokenUserID = uid
		return nil
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	if err := svc.RequestPasswordReset(context.Background(), "known@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if len(enqueued) != 1 || enqueued[0].ToAddress != "known@example.com" {
		t.Fatalf("unexpected enqueued mail: %+v", enqueued)
	}
	if tokenUserID != userID {
		t.Fatalf("SetResetToken userID = %v, want %v", tokenUserID, userID)
	}
	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionUserPasswordResetRequested {
		t.Fatalf("expected one user.password_reset_requested audit call, got %+v", spy.auditCalls)
	}
}

// ---- ResetPassword ----

func TestService_ResetPassword_UnknownToken(t *testing.T) {
	mail := newMockMail() // ConsumeResetToken defaults to "not found".
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			t.Fatal("GetUserByID must not be called when the token isn't found")
			return db.User{}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	err := svc.ResetPassword(context.Background(), "bogus-token", "new-hash")
	if code := appErrorCode(t, err); code != apperror.InvalidResetToken {
		t.Fatalf("code = %q, want %q", code, apperror.InvalidResetToken)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record for an unknown token")
	}
}

func TestService_ResetPassword_ReplayedToken(t *testing.T) {
	// GETDEL-backed consume: the first call finds and deletes the key, the
	// second (a replay of the same raw token) must see nothing.
	userID := uuid.New()
	consumed := false
	mail := newMockMail()
	mail.consumeResetToken = func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
		if consumed {
			return uuid.Nil, false, nil
		}
		consumed = true
		return userID, true, nil
	}
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, Email: "user@example.com"}, nil
		},
		withTx: withMockTx(new(*mockTxQuerier)),
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), mail, newMockRenderer(), testAppURL)

	if err := svc.ResetPassword(context.Background(), "raw-token", "new-hash"); err != nil {
		t.Fatalf("first ResetPassword: %v", err)
	}

	err := svc.ResetPassword(context.Background(), "raw-token", "new-hash")
	if code := appErrorCode(t, err); code != apperror.InvalidResetToken {
		t.Fatalf("replayed token code = %q, want %q", code, apperror.InvalidResetToken)
	}
}

func TestService_ResetPassword_UserGone(t *testing.T) {
	// A token naming a user id that no longer exists must map to the SAME
	// code as an unknown token — never leak the distinction.
	userID := uuid.New()
	mail := newMockMail()
	mail.consumeResetToken = func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
		return userID, true, nil
	}
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), mail, newMockRenderer(), testAppURL)

	err := svc.ResetPassword(context.Background(), "raw-token", "new-hash")
	if code := appErrorCode(t, err); code != apperror.InvalidResetToken {
		t.Fatalf("code = %q, want %q", code, apperror.InvalidResetToken)
	}
}

func TestService_ResetPassword_HappyPath(t *testing.T) {
	userID := uuid.New()
	mail := newMockMail()
	mail.consumeResetToken = func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
		return userID, true, nil
	}
	var tx *mockTxQuerier
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, Email: "user@example.com", IsVerified: false}, nil
		},
		withTx: withMockTx(&tx),
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	if err := svc.ResetPassword(context.Background(), "raw-token", "new-bcrypt-hash"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}

	// Password update, verification, and session revocation must all have
	// gone through the SAME *db.Queries built for this one WithTx call —
	// i.e. the same transaction.
	if len(tx.updateUserPasswordCalls) != 1 {
		t.Fatalf("updateUserPasswordCalls = %d, want 1", len(tx.updateUserPasswordCalls))
	}
	if got := tx.updateUserPasswordCalls[0]; got.ID != userID || got.PasswordHash != "new-bcrypt-hash" {
		t.Fatalf("unexpected UpdateUserPasswordParams: %+v", got)
	}
	if len(tx.markUserVerifiedCalls) != 1 || tx.markUserVerifiedCalls[0] != userID {
		t.Fatalf("markUserVerifiedCalls = %+v, want [%v]", tx.markUserVerifiedCalls, userID)
	}
	if len(tx.revokeAllUserSessionsIDs) != 1 || tx.revokeAllUserSessionsIDs[0] != userID {
		t.Fatalf("revokeAllUserSessionsIDs = %+v, want [%v]", tx.revokeAllUserSessionsIDs, userID)
	}

	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionUserPasswordReset {
		t.Fatalf("expected one user.password_reset audit call, got %+v", spy.auditCalls)
	}
}
