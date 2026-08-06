package auth

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// ---- hand-mocked authStore ----

type mockAuthStore struct {
	getUserByEmail func(ctx context.Context, email string) (db.User, error)
	createUser     func(ctx context.Context, arg db.CreateUserParams) (db.User, error)

	getSessionByRefreshToken func(ctx context.Context, refreshToken string) (db.Session, error)
	createSession            func(ctx context.Context, arg db.CreateSessionParams) (db.Session, error)
	revokeSessionByID        func(ctx context.Context, id uuid.UUID) error
	revokeSessionFamily      func(ctx context.Context, family uuid.UUID) error
	revokeAllUserSessions    func(ctx context.Context, userID uuid.UUID) error
	withTx                   func(ctx context.Context, fn func(q *db.Queries) error) error
}

func (m *mockAuthStore) GetUserByEmail(ctx context.Context, email string) (db.User, error) {
	return m.getUserByEmail(ctx, email)
}

func (m *mockAuthStore) CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
	return m.createUser(ctx, arg)
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

func (m *mockAuthStore) WithTx(ctx context.Context, fn func(q *db.Queries) error) error {
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

var _ loginLimiter = (*mockLimiter)(nil)

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
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy))

	_, err := svc.Register(context.Background(), "taken@example.com", "hash", nil)
	if code := appErrorCode(t, err); code != apperror.EmailTaken {
		t.Fatalf("code = %q, want %q", code, apperror.EmailTaken)
	}
	if len(spy.auditCalls) != 0 {
		t.Fatal("expected no audit record when registration fails")
	}
}

func TestService_Register_HappyPath(t *testing.T) {
	var createdArg db.CreateUserParams
	newUserID := uuid.New()
	store := &mockAuthStore{
		getUserByEmail: func(ctx context.Context, email string) (db.User, error) {
			return db.User{}, pgx.ErrNoRows
		},
		createUser: func(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
			createdArg = arg
			return db.User{ID: newUserID, Email: arg.Email, PasswordHash: arg.PasswordHash, DisplayName: arg.DisplayName}, nil
		},
	}
	spy := &spyQuerier{}
	svc := NewService(store, &mockLimiter{}, newTestAudit(spy))

	displayName := "Ann"
	user, err := svc.Register(context.Background(), "new@example.com", "hashed-pw", &displayName)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user.ID != newUserID || user.Email != "new@example.com" {
		t.Fatalf("unexpected user: %+v", user)
	}
	if createdArg.Email != "new@example.com" || createdArg.PasswordHash != "hashed-pw" || createdArg.DisplayName != &displayName {
		t.Fatalf("unexpected CreateUserParams: %+v", createdArg)
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
	svc := NewService(store, limiter, newTestAudit(&spyQuerier{}))

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
	svc := NewService(store, limiter, newTestAudit(&spyQuerier{}))

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
	svc := NewService(store, limiter, newTestAudit(&spyQuerier{}))

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
	svc := NewService(store, limiter, newTestAudit(spy))

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
