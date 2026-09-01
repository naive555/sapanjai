package auth

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/email"
)

// ---- hand-mocked authStore ----

type mockAuthStore struct {
	getUserByEmail   func(ctx context.Context, email string) (db.User, error)
	getUserByID      func(ctx context.Context, id uuid.UUID) (db.User, error)
	createUser       func(ctx context.Context, arg db.CreateUserParams) (db.User, error)
	markUserVerified func(ctx context.Context, id uuid.UUID) error
	enqueueEmail     func(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error)

	getSessionByRefreshToken func(ctx context.Context, refreshToken string) (db.Session, error)
	createSession            func(ctx context.Context, arg db.CreateSessionParams) (db.Session, error)
	revokeSessionByID        func(ctx context.Context, id uuid.UUID) error
	revokeSessionFamily      func(ctx context.Context, family uuid.UUID) error
	revokeAllUserSessions    func(ctx context.Context, userID uuid.UUID) error
	withTx                   func(ctx context.Context, fn func(q db.Querier) error) error
}

func (m *mockAuthStore) GetUserByEmail(ctx context.Context, email string) (db.User, error) {
	return m.getUserByEmail(ctx, email)
}

func (m *mockAuthStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	return m.getUserByID(ctx, id)
}

func (m *mockAuthStore) CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
	return m.createUser(ctx, arg)
}

func (m *mockAuthStore) MarkUserVerified(ctx context.Context, id uuid.UUID) error {
	return m.markUserVerified(ctx, id)
}

func (m *mockAuthStore) EnqueueEmail(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error) {
	return m.enqueueEmail(ctx, arg)
}

func (m *mockAuthStore) GetSessionByRefreshToken(ctx context.Context, refreshToken string) (db.Session, error) {
	return m.getSessionByRefreshToken(ctx, refreshToken)
}

func (m *mockAuthStore) CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.Session, error) {
	return m.createSession(ctx, arg)
}

func (m *mockAuthStore) RevokeSessionByID(ctx context.Context, id uuid.UUID) error {
	return m.revokeSessionByID(ctx, id)
}

func (m *mockAuthStore) RevokeSessionFamily(ctx context.Context, family uuid.UUID) error {
	return m.revokeSessionFamily(ctx, family)
}

func (m *mockAuthStore) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error {
	return m.revokeAllUserSessions(ctx, userID)
}

func (m *mockAuthStore) WithTx(ctx context.Context, fn func(q db.Querier) error) error {
	return m.withTx(ctx, fn)
}

var _ authStore = (*mockAuthStore)(nil)

// ---- hand-mocked loginLimiter ----

type mockLimiter struct {
	attempts    int
	getErr      error
	incremented int
	incErr      error
	resetCalls  int
	resetErr    error
	banCalls    []uuid.UUID
	banErr      error
}

func (m *mockLimiter) GetLoginAttempts(ctx context.Context, email string) (int, error) {
	return m.attempts, m.getErr
}

func (m *mockLimiter) IncrementLoginAttempts(ctx context.Context, email string) (int64, error) {
	m.incremented++
	return int64(m.attempts + m.incremented), m.incErr
}

func (m *mockLimiter) ResetLoginAttempts(ctx context.Context, email string) error {
	m.resetCalls++
	return m.resetErr
}

func (m *mockLimiter) Ban(ctx context.Context, userID uuid.UUID) error {
	m.banCalls = append(m.banCalls, userID)
	return m.banErr
}

var _ loginLimiter = (*mockLimiter)(nil)

// ---- hand-mocked verificationTokens ----

type mockVerificationTokens struct {
	setVerifyToken     func(ctx context.Context, tokenHash string, userID uuid.UUID) error
	consumeVerifyToken func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error)
	markVerifyResent   func(ctx context.Context, userID uuid.UUID) (bool, error)

	setResetToken      func(ctx context.Context, tokenHash string, userID uuid.UUID) error
	consumeResetToken  func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error)
	markResetRequested func(ctx context.Context, email string) (bool, error)
}

func (m *mockVerificationTokens) SetVerifyToken(ctx context.Context, tokenHash string, userID uuid.UUID) error {
	return m.setVerifyToken(ctx, tokenHash, userID)
}

func (m *mockVerificationTokens) ConsumeVerifyToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
	return m.consumeVerifyToken(ctx, tokenHash)
}

func (m *mockVerificationTokens) MarkVerifyResent(ctx context.Context, userID uuid.UUID) (bool, error) {
	return m.markVerifyResent(ctx, userID)
}

func (m *mockVerificationTokens) SetResetToken(ctx context.Context, tokenHash string, userID uuid.UUID) error {
	return m.setResetToken(ctx, tokenHash, userID)
}

func (m *mockVerificationTokens) ConsumeResetToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
	return m.consumeResetToken(ctx, tokenHash)
}

func (m *mockVerificationTokens) MarkResetRequested(ctx context.Context, email string) (bool, error) {
	return m.markResetRequested(ctx, email)
}

var _ verificationTokens = (*mockVerificationTokens)(nil)

// newMockMail returns a mockVerificationTokens with harmless defaults —
// SetVerifyToken/SetResetToken succeed, ConsumeVerifyToken/ConsumeResetToken
// report "not found", and MarkVerifyResent/MarkResetRequested report
// "cooldown not active" — so tests that don't care about verification or
// password reset at all (Login, RotateSession) never need to wire one up by
// hand.
func newMockMail() *mockVerificationTokens {
	return &mockVerificationTokens{
		setVerifyToken: func(ctx context.Context, tokenHash string, userID uuid.UUID) error {
			return nil
		},
		consumeVerifyToken: func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
			return uuid.Nil, false, nil
		},
		markVerifyResent: func(ctx context.Context, userID uuid.UUID) (bool, error) {
			return true, nil
		},
		setResetToken: func(ctx context.Context, tokenHash string, userID uuid.UUID) error {
			return nil
		},
		consumeResetToken: func(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
			return uuid.Nil, false, nil
		},
		markResetRequested: func(ctx context.Context, email string) (bool, error) {
			return true, nil
		},
	}
}

// ---- hand-mocked verificationRenderer ----

type mockRenderer struct {
	verifyEmail   func(to string, data email.VerifyEmailData) (email.Message, error)
	passwordReset func(to string, data email.PasswordResetData) (email.Message, error)
}

func (m *mockRenderer) VerifyEmail(to string, data email.VerifyEmailData) (email.Message, error) {
	return m.verifyEmail(to, data)
}

func (m *mockRenderer) PasswordReset(to string, data email.PasswordResetData) (email.Message, error) {
	return m.passwordReset(to, data)
}

var _ verificationRenderer = (*mockRenderer)(nil)

// newMockRenderer returns a mockRenderer that renders a fixed, harmless
// message for either mail kind — enough for tests that only care that *an*
// email was enqueued, not its exact contents.
func newMockRenderer() *mockRenderer {
	return &mockRenderer{
		verifyEmail: func(to string, data email.VerifyEmailData) (email.Message, error) {
			return email.Message{To: to, Subject: email.SubjectVerifyEmail, HTML: "<html>" + data.VerifyURL + "</html>", Text: data.VerifyURL}, nil
		},
		passwordReset: func(to string, data email.PasswordResetData) (email.Message, error) {
			return email.Message{To: to, Subject: email.SubjectPasswordReset, HTML: "<html>" + data.ResetURL + "</html>", Text: data.ResetURL}, nil
		},
	}
}

const testAppURL = "http://localhost:4000"

// spyQuerier records CreateAuditLog calls for assertion; it embeds the
// db.Querier interface unset so any other method panics if accidentally
// exercised — none of these tests should ever reach one.
type spyQuerier struct {
	db.Querier
	auditCalls []db.CreateAuditLogParams
}

func (s *spyQuerier) CreateAuditLog(ctx context.Context, arg db.CreateAuditLogParams) error {
	s.auditCalls = append(s.auditCalls, arg)
	return nil
}

func newTestAudit(spy *spyQuerier) *auditlog.Service {
	return auditlog.NewService(spy, slog.New(slog.NewTextHandler(os.Stdout, nil)))
}

func appErrorCode(t *testing.T, err error) string {
	t.Helper()
	var appErr *apperror.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperror.Error, got %T: %v", err, err)
	}
	return appErr.Code
}

// ---- Register ----

func TestService_Register_EmailTaken(t *testing.T) {
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{ID: uuid.New(), Email: email}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), newMockMail(), newMockRenderer(), testAppURL)

	_, err := svc.Register(context.Background(), "taken@example.com", "hash", nil)
	if code := appErrorCode(t, err); code != apperror.EmailTaken {
		t.Fatalf("code = %q, want %q", code, apperror.EmailTaken)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record when registration fails")
	}
}

func TestService_Register_HappyPath(t *testing.T) {
	var tx *mockTxQuerier
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
		withTx: withMockTx(&tx),
	}
	mail := newMockMail()
	var setTokenUserID uuid.UUID
	mail.setVerifyToken = func(ctx context.Context, tokenHash string, userID uuid.UUID) error {
		setTokenUserID = userID
		return nil
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy), mail, newMockRenderer(), testAppURL)

	displayName := "Ann"
	user, err := svc.Register(context.Background(), "new@example.com", "hashed-pw", &displayName)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user.Email != "new@example.com" || user.PasswordHash != "hashed-pw" {
		t.Fatalf("unexpected user: %+v", user)
	}

	// CreateUser and EnqueueEmail must both have gone through the SAME
	// *db.Queries built for this one WithTx call — i.e. the same
	// transaction — not two independent statements.
	if len(tx.createUserCalls) != 1 {
		t.Fatalf("createUserCalls = %d, want 1", len(tx.createUserCalls))
	}
	created := tx.createUserCalls[0]
	if created.Email != "new@example.com" || created.PasswordHash != "hashed-pw" || created.DisplayName != &displayName {
		t.Fatalf("unexpected CreateUserParams: %+v", created)
	}
	if len(tx.enqueueEmailCalls) != 1 {
		t.Fatalf("enqueueEmailCalls = %d, want 1 (exactly one outbox row, in the same tx as the user insert)", len(tx.enqueueEmailCalls))
	}
	if enqueued := tx.enqueueEmailCalls[0]; enqueued.ToAddress != "new@example.com" {
		t.Fatalf("unexpected EnqueueEmailParams: %+v", enqueued)
	}
	if setTokenUserID != user.ID {
		t.Fatalf("SetVerifyToken userID = %v, want %v", setTokenUserID, user.ID)
	}

	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionUserRegister {
		t.Fatalf("expected one user.register audit call, got %+v", spy.auditCalls)
	}
}

// ---- Login ----

func newLoginTestUser(t *testing.T, password string) db.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	return db.User{ID: uuid.New(), Email: "user@example.com", PasswordHash: string(hash)}
}

func TestService_Login_RateLimited(t *testing.T) {
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			t.Fatal("GetUserByEmail must not be called once the rate limit is hit")
			return db.User{}, nil
		},
	}
	limiter := &mockLimiter{attempts: maxLoginAttempts}
	svc := NewService(store, limiter, newTestAudit(&spyQuerier{}), newMockMail(), newMockRenderer(), testAppURL)

	_, err := svc.Login(context.Background(), "user@example.com", "irrelevant")
	if code := appErrorCode(t, err); code != apperror.TooManyAttempts {
		t.Fatalf("code = %q, want %q", code, apperror.TooManyAttempts)
	}
}

func TestService_Login_WrongPassword(t *testing.T) {
	user := newLoginTestUser(t, "correct-password")
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return user, nil
		},
	}
	limiter := &mockLimiter{attempts: 1}
	svc := NewService(store, limiter, newTestAudit(&spyQuerier{}), newMockMail(), newMockRenderer(), testAppURL)

	_, err := svc.Login(context.Background(), user.Email, "wrong-password")
	if code := appErrorCode(t, err); code != apperror.InvalidCredentials {
		t.Fatalf("code = %q, want %q", code, apperror.InvalidCredentials)
	}
	if limiter.incremented != 1 {
		t.Fatalf("incremented = %d, want 1", limiter.incremented)
	}
	if limiter.resetCalls != 0 {
		t.Fatal("expected no reset on a failed login")
	}
}

func TestService_Login_UnknownUser(t *testing.T) {
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
	}
	limiter := &mockLimiter{}
	svc := NewService(store, limiter, newTestAudit(&spyQuerier{}), newMockMail(), newMockRenderer(), testAppURL)

	_, err := svc.Login(context.Background(), "nobody@example.com", "whatever")
	if code := appErrorCode(t, err); code != apperror.InvalidCredentials {
		t.Fatalf("code = %q, want %q", code, apperror.InvalidCredentials)
	}
	if limiter.incremented != 1 {
		t.Fatalf("incremented = %d, want 1", limiter.incremented)
	}
}

func TestService_Login_Success(t *testing.T) {
	password := "correct-password"
	user := newLoginTestUser(t, password)
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return user, nil
		},
	}
	limiter := &mockLimiter{attempts: 3}
	spy := &spyQuerier{}
	svc := NewService(store, limiter, newTestAudit(spy), newMockMail(), newMockRenderer(), testAppURL)

	got, err := svc.Login(context.Background(), user.Email, password)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.ID != user.ID {
		t.Fatalf("unexpected user: %+v", got)
	}
	if limiter.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want 1", limiter.resetCalls)
	}
	if limiter.incremented != 0 {
		t.Fatalf("incremented = %d, want 0 on success", limiter.incremented)
	}
	if len(spy.auditCalls) != 1 || spy.auditCalls[0].Action != auditlog.ActionUserLogin {
		t.Fatalf("expected one user.login audit call, got %+v", spy.auditCalls)
	}
}

func TestService_Login_BannedUser(t *testing.T) {
	password := "correct-password"
	user := newLoginTestUser(t, password)
	user.BannedAt = pgtype.Timestamp{Time: time.Now().Add(-time.Hour), Valid: true}
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return user, nil
		},
	}
	limiter := &mockLimiter{attempts: 1}
	spy := &spyQuerier{}
	svc := NewService(store, limiter, newTestAudit(spy), newMockMail(), newMockRenderer(), testAppURL)

	_, err := svc.Login(context.Background(), user.Email, password)
	if code := appErrorCode(t, err); code != apperror.AccountSuspended {
		t.Fatalf("code = %q, want %q", code, apperror.AccountSuspended)
	}
	// Correct credentials for a banned user still reset the failed-attempt
	// counter (the password check itself succeeded) but must never emit a
	// user.login audit record — the login is refused, not granted.
	if limiter.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want 1", limiter.resetCalls)
	}
	if len(limiter.banCalls) != 1 || limiter.banCalls[0] != user.ID {
		t.Fatalf("banCalls = %v, want exactly [%v] (re-priming the Redis ban cache is what makes a flush self-heal)", limiter.banCalls, user.ID)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatalf("expected no user.login audit call for a banned user, got %+v", spy.auditCalls)
	}
}
