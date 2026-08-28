package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/infra/redis"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/email"
)

// Compile-time checks that the concrete infra types satisfy the narrow
// interfaces this service depends on.
var (
	_ authStore            = (*database.Store)(nil)
	_ loginLimiter         = (*redis.Auth)(nil)
	_ verificationTokens   = (*redis.Email)(nil)
	_ verificationRenderer = (*email.Renderer)(nil)
)

// maxLoginAttempts mirrors MAX_LOGIN_ATTEMPTS in the source app's
// src/modules/auth/service.ts.
const maxLoginAttempts = 5

// bcryptCost mirrors SALT_ROUNDS in the source app's
// src/modules/auth/service.ts (bcryptjs cost 12).
const bcryptCost = 12

// authStore is the subset of *database.Store the auth service depends on,
// narrowed so unit tests can hand-mock it without implementing the full
// db.Querier surface. *database.Store satisfies this (it embeds *db.Queries
// and provides WithTx).
type authStore interface {
	GetUserByEmail(ctx context.Context, email string) (db.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)
	CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error)
	MarkUserVerified(ctx context.Context, id uuid.UUID) error
	EnqueueEmail(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error)
	GetSessionByRefreshToken(ctx context.Context, refreshToken string) (db.Session, error)
	CreateSession(ctx context.Context, arg db.CreateSessionParams) (db.Session, error)
	RevokeSessionByID(ctx context.Context, id uuid.UUID) error
	RevokeSessionFamily(ctx context.Context, family uuid.UUID) error
	RevokeAllUserSessions(ctx context.Context, userID uuid.UUID) error
	WithTx(ctx context.Context, fn func(q db.Querier) error) error
}

// loginLimiter is the subset of *redis.Auth the service needs for login
// rate limiting plus re-priming the ban cache on a banned credential's
// login attempt, narrowed for the same reason as authStore.
type loginLimiter interface {
	GetLoginAttempts(ctx context.Context, email string) (int, error)
	IncrementLoginAttempts(ctx context.Context, email string) (int64, error)
	ResetLoginAttempts(ctx context.Context, email string) error
	Ban(ctx context.Context, userID uuid.UUID) error
}

// Service implements register/login/session-rotation, mirroring AuthService
// in the source app's src/modules/auth/service.ts.
type Service struct {
	store   authStore
	limiter loginLimiter
	audit   *auditlog.Service

	// mail, render, and appURL back email verification (verification.go):
	// mail is the Redis token/cooldown helper, render turns template data
	// into a ready-to-send email.Message, and appURL is the browser-facing
	// frontend origin (config.Config.AppPublicURL) verification links point
	// at.
	mail   verificationTokens
	render verificationRenderer
	appURL string
}

// NewService builds an auth Service.
func NewService(store authStore, limiter loginLimiter, audit *auditlog.Service, mail verificationTokens, render verificationRenderer, appURL string) *Service {
	return &Service{store: store, limiter: limiter, audit: audit, mail: mail, render: render, appURL: appURL}
}

// Register creates a new user and enqueues its verification email.
// passwordHash is the already-bcrypt-hashed password (hashing happens in
// the handler alongside the 72-byte truncation shared with Login). Returns
// apperror.EmailTaken if the email is already registered.
//
// CreateUser and the verification email's outbox insert run inside one
// transaction (store.WithTx) so a user row can never exist without its
// verification email queued, and vice versa — the whole point of the
// outbox pattern (see the email-verification plan §2). The GetUserByEmail
// pre-check and the audit write stay outside the transaction: the
// pre-check is read-only and has nothing to roll back, and audit writes
// are best-effort and must never roll back a registration that otherwise
// succeeded.
func (s *Service) Register(ctx context.Context, email, passwordHash string, displayName *string) (db.User, error) {
	_, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		return db.User{}, apperror.New(apperror.EmailTaken)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, err
	}

	var user db.User
	err = s.store.WithTx(ctx, func(q db.Querier) error {
		created, err := q.CreateUser(ctx, db.CreateUserParams{
			Email:        email,
			PasswordHash: passwordHash,
			DisplayName:  displayName,
		})
		if err != nil {
			return err
		}
		user = created

		return s.SendVerificationEmail(ctx, q, user.ID, user.Email, user.DisplayName)
	})
	if err != nil {
		return db.User{}, err
	}

	s.audit.Record(ctx, auditlog.ActionUserRegister, &user.ID, nil, nil)

	return user, nil
}

// Login validates credentials against the rate limiter and stored hash.
// The rate-limit check happens BEFORE credential validation, matching
// source. A failed attempt (unknown email or bad password) increments the
// limiter and returns apperror.InvalidCredentials; success resets it.
func (s *Service) Login(ctx context.Context, email, password string) (db.User, error) {
	attempts, err := s.limiter.GetLoginAttempts(ctx, email)
	if err != nil {
		return db.User{}, err
	}
	if attempts >= maxLoginAttempts {
		return db.User{}, apperror.New(apperror.TooManyAttempts)
	}

	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return db.User{}, err
		}
		if _, incErr := s.limiter.IncrementLoginAttempts(ctx, email); incErr != nil {
			return db.User{}, incErr
		}
		return db.User{}, apperror.New(apperror.InvalidCredentials)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), truncatePassword(password)); err != nil {
		if _, incErr := s.limiter.IncrementLoginAttempts(ctx, email); incErr != nil {
			return db.User{}, incErr
		}
		return db.User{}, apperror.New(apperror.InvalidCredentials)
	}

	if err := s.limiter.ResetLoginAttempts(ctx, email); err != nil {
		return db.User{}, err
	}

	// Credentials check out, but a banned account never gets a session.
	// Re-priming the Redis ban cache here — rather than only trusting
	// whatever is already there — is what makes a Redis flush self-heal:
	// the next login attempt from a banned user re-establishes the cache
	// entry even if it was lost. See docs/11-admin-panel.md §4.
	if user.BannedAt.Valid {
		if err := s.limiter.Ban(ctx, user.ID); err != nil {
			return db.User{}, err
		}
		return db.User{}, apperror.New(apperror.AccountSuspended)
	}

	s.audit.Record(ctx, auditlog.ActionUserLogin, &user.ID, nil, nil)

	return user, nil
}

// RotateSession validates oldRefreshToken, detects reuse of an already-
// rotated (revoked) token by revoking its entire family, and — if valid —
// atomically revokes it and inserts newRefreshToken in its place. Mirrors
// AuthService.rotateSession exactly, including check order: not-found,
// then reuse, then expiry.
//
// Only the revoke-old+create-new pair runs inside a transaction. The
// not-found/reuse/expiry checks return a business apperror on failure, and
// Store.WithTx rolls back the transaction whenever its callback returns a
// non-nil error — so a RevokeSessionFamily call made inside the same
// transaction as a "return apperror.RefreshTokenReuse" would be silently
// undone by that rollback. Keeping the reuse-family revocation as its own
// statement (already atomic on its own) outside any transaction avoids that.
func (s *Service) RotateSession(ctx context.Context, oldRefreshToken, newRefreshToken string, expiresAt time.Time) (uuid.UUID, error) {
	session, err := s.store.GetSessionByRefreshToken(ctx, oldRefreshToken)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, apperror.New(apperror.InvalidRefreshToken)
		}
		return uuid.Nil, err
	}

	if session.IsRevoked {
		if err := s.store.RevokeSessionFamily(ctx, session.Family); err != nil {
			return uuid.Nil, err
		}
		return uuid.Nil, apperror.New(apperror.RefreshTokenReuse)
	}

	// sessions.expires_at is "timestamp without time zone": pgx scans it as
	// a UTC-tagged time.Time regardless of what zone was originally
	// written. Comparing against a local time.Now() would silently shift
	// the boundary by the server's UTC offset (verified live: on a UTC+7
	// host, an intentionally-expired fixture read back as ~7h in the
	// future and this check incorrectly passed). Both sides must be UTC.
	if session.ExpiresAt.Before(time.Now().UTC()) {
		return uuid.Nil, apperror.New(apperror.RefreshTokenExpired)
	}

	err = s.store.WithTx(ctx, func(q db.Querier) error {
		if err := q.RevokeSessionByID(ctx, session.ID); err != nil {
			return err
		}

		_, err := q.CreateSession(ctx, db.CreateSessionParams{
			UserID:       session.UserID,
			RefreshToken: newRefreshToken,
			Family:       session.Family,
			ExpiresAt:    expiresAt,
		})
		return err
	})
	if err != nil {
		return uuid.Nil, err
	}

	return session.UserID, nil
}

// CreateSession inserts a new session row, used by register/login to
// establish the initial refresh-token family.
func (s *Service) CreateSession(ctx context.Context, userID uuid.UUID, refreshToken string, family uuid.UUID, expiresAt time.Time) error {
	_, err := s.store.CreateSession(ctx, db.CreateSessionParams{
		UserID:       userID,
		RefreshToken: refreshToken,
		Family:       family,
		ExpiresAt:    expiresAt,
	})
	return err
}

// RevokeAllSessions revokes every active session for a user, used by
// logout.
func (s *Service) RevokeAllSessions(ctx context.Context, userID uuid.UUID) error {
	return s.store.RevokeAllUserSessions(ctx, userID)
}

// truncatePassword reproduces bcryptjs's silent truncation of inputs over
// 72 bytes (Go's bcrypt errors instead of truncating), so long passwords
// hash/compare identically to the source rather than turning into a 500.
func truncatePassword(password string) []byte {
	b := []byte(password)
	if len(b) > 72 {
		return b[:72]
	}
	return b
}
