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

// ---- VerifyEmail ----

func TestService_VerifyEmail_UnknownToken(t *testing.T) {
	mail := newMockMail() // ConsumeVerifyToken defaults to "not found".
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			t.Fatal("GetUserByID must not be called when the token isn't found")
			return db.User{}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	err := svc.VerifyEmail(context.Background(), "bogus-token")
	if code := appErrorCode(t, err); code != apperror.InvalidVerificationToken {
		t.Fatalf("code = %q, want %q", code, apperror.InvalidVerificationToken)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record for an unknown token")
	}
}

func TestService_VerifyEmail_ReplayedToken(t *testing.T) {
	// A GETDEL-backed consume: the first call finds and deletes the key,
	// the second (a replay of the same raw token) must see nothing.
	userID := uuid.New()
	consumed := false
	mail := newMockMail()
	mail.consumeVerifyToken = func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
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
		markUserVerified: func(ctx context.Context, id uuid.UUID) error { return nil },
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), mail, newMockRenderer(), testAppURL)

	if err := svc.VerifyEmail(context.Background(), "raw-token"); err != nil {
		t.Fatalf("first VerifyEmail: %v", err)
	}

	err := svc.VerifyEmail(context.Background(), "raw-token")
	if code := appErrorCode(t, err); code != apperror.InvalidVerificationToken {
		t.Fatalf("replayed token code = %q, want %q", code, apperror.InvalidVerificationToken)
	}
}

func TestService_VerifyEmail_UserGone(t *testing.T) {
	// A token that names a user id which no longer exists (e.g. the
	// registration transaction it was minted alongside got rolled back)
	// must map to the SAME code as an unknown token — never leak the
	// distinction.
	userID := uuid.New()
	mail := newMockMail()
	mail.consumeVerifyToken = func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
		return userID, true, nil
	}
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), mail, newMockRenderer(), testAppURL)

	err := svc.VerifyEmail(context.Background(), "raw-token")
	if code := appErrorCode(t, err); code != apperror.InvalidVerificationToken {
		t.Fatalf("code = %q, want %q", code, apperror.InvalidVerificationToken)
	}
}

func TestService_VerifyEmail_HappyPath(t *testing.T) {
	userID := uuid.New()
	mail := newMockMail()
	mail.consumeVerifyToken = func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
		return userID, true, nil
	}
	var markedID uuid.UUID
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, Email: "user@example.com", IsVerified: false}, nil
		},
		markUserVerified: func(ctx context.Context, id uuid.UUID) error {
			markedID = id
			return nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	if err := svc.VerifyEmail(context.Background(), "raw-token"); err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if markedID != userID {
		t.Fatalf("MarkUserVerified called with %v, want %v", markedID, userID)
	}
	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionUserEmailVerified {
		t.Fatalf("expected one user.email_verified audit call, got %+v", spy.auditCalls)
	}
}

func TestService_VerifyEmail_AlreadyVerified_NoSecondAudit(t *testing.T) {
	// A live (unexpired, unconsumed) token for a user who is already
	// verified — e.g. a second click on an old email after a fresh one was
	// already used — is a no-op: nil, no MarkUserVerified call, no audit
	// row.
	userID := uuid.New()
	mail := newMockMail()
	mail.consumeVerifyToken = func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
		return userID, true, nil
	}
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, Email: "user@example.com", IsVerified: true}, nil
		},
		markUserVerified: func(ctx context.Context, id uuid.UUID) error {
			t.Fatal("MarkUserVerified must not be called for an already-verified user")
			return nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	if err := svc.VerifyEmail(context.Background(), "raw-token"); err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatalf("expected no audit record for an already-verified user, got %+v", spy.auditCalls)
	}
}

// ---- ResendVerificationEmail ----

func TestService_ResendVerification_UserNotFound(t *testing.T) {
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), newMockMail(), newMockRenderer(), testAppURL)

	err := svc.ResendVerificationEmail(context.Background(), uuid.New())
	if code := appErrorCode(t, err); code != apperror.UserNotFound {
		t.Fatalf("code = %q, want %q", code, apperror.UserNotFound)
	}
}

func TestService_ResendVerification_AlreadyVerified(t *testing.T) {
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, IsVerified: true}, nil
		},
	}
	mail := newMockMail()
	mail.markVerifyResent = func(ctx context.Context, userID uuid.UUID) (bool, error) {
		t.Fatal("MarkVerifyResent must not be called for an already-verified user")
		return false, nil
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), mail, newMockRenderer(), testAppURL)

	err := svc.ResendVerificationEmail(context.Background(), uuid.New())
	if code := appErrorCode(t, err); code != apperror.AlreadyVerified {
		t.Fatalf("code = %q, want %q", code, apperror.AlreadyVerified)
	}
}

func TestService_ResendVerification_CooldownActive(t *testing.T) {
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, Email: "user@example.com", IsVerified: false}, nil
		},
		enqueueEmail: func(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
			t.Fatal("EnqueueEmail must not be called while the cooldown is active")
			return db.EmailOutbox{}, nil
		},
	}
	mail := newMockMail()
	mail.markVerifyResent = func(ctx context.Context, userID uuid.UUID) (bool, error) {
		return false, nil // cooldown already active
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), mail, newMockRenderer(), testAppURL)

	err := svc.ResendVerificationEmail(context.Background(), uuid.New())
	if code := appErrorCode(t, err); code != apperror.VerificationResendTooSoon {
		t.Fatalf("code = %q, want %q", code, apperror.VerificationResendTooSoon)
	}
}

func TestService_ResendVerification_HappyPath(t *testing.T) {
	userID := uuid.New()
	var enqueued []db.EnqueueEmailParams
	store := &mockAuthStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, Email: "user@example.com", IsVerified: false}, nil
		},
		enqueueEmail: func(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
			enqueued = append(enqueued, arg)
			return db.EmailOutbox{ID: uuid.New()}, nil
		},
	}
	mail := newMockMail()
	var tokenUserID uuid.UUID
	mail.setVerifyToken = func(ctx context.Context, tokenHash string, uid uuid.UUID) error {
		tokenUserID = uid
		return nil
	}
	svc := NewService(store, &mockLimiter{}, newTestAudit(&spyQuerier{}), mail, newMockRenderer(), testAppURL)

	if err := svc.ResendVerificationEmail(context.Background(), userID); err != nil {
		t.Fatalf("ResendVerificationEmail: %v", err)
	}
	if len(enqueued) != 1 || enqueued[0].ToAddress != "user@example.com" {
		t.Fatalf("unexpected enqueued mail: %+v", enqueued)
	}
	if tokenUserID != userID {
		t.Fatalf("SetVerifyToken userID = %v, want %v", tokenUserID, userID)
	}
}
