package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/email"
)

// tokenRandomBytes is how much crypto/rand output backs a raw
// verification or password-reset token, matching the entropy budget
// mcpkey uses for PATs (docs/07-sheets-adapter-decisions.md §1) —
// comfortably enough that brute-forcing a guess is infeasible.
const tokenRandomBytes = 32

// emailQueue is the narrow slice of db.Querier that SendVerificationEmail
// needs to enqueue the outbox row through. It is satisfied both by a
// transaction-scoped *db.Queries (so Register's call joins the caller's
// transaction) and by authStore itself (so ResendVerificationEmail, which
// runs outside any transaction, can pass s.store) — the same method runs on
// either side of a transaction boundary without the signature depending on
// the full db.Querier surface.
type emailQueue interface {
	EnqueueEmail(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error)
}

// verificationTokens is the subset of *redis.Email the service needs,
// narrowed for the same reason as loginLimiter. It covers both token
// namespaces this package mints (email verification and password reset,
// internal/module/auth/password_reset.go) rather than splitting into two
// near-identical interfaces — both are backed by the same *redis.Email
// value at the single call site (server.go), and the two token kinds share
// nothing but a Redis client, so a second interface would only double the
// mock without narrowing anything real.
type verificationTokens interface {
	SetVerifyToken(ctx context.Context, tokenHash string, userID uuid.UUID) error
	ConsumeVerifyToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error)
	MarkVerifyResent(ctx context.Context, userID uuid.UUID) (bool, error)

	SetResetToken(ctx context.Context, tokenHash string, userID uuid.UUID) error
	ConsumeResetToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error)
	MarkResetRequested(ctx context.Context, email string) (bool, error)
}

// verificationRenderer is the subset of *email.Renderer the service needs
// to build a verification or password-reset mail, narrowed for the same
// reason as the other interfaces in this file.
type verificationRenderer interface {
	VerifyEmail(to string, data email.VerifyEmailData) (email.Message, error)
	PasswordReset(to string, data email.PasswordResetData) (email.Message, error)
}

// SendVerificationEmail generates a fresh verification token, stores its
// hash in Redis, renders the mail, and enqueues it through q — the caller's
// choice of a bare store (ResendVerificationEmail) or a transaction-scoped
// *db.Queries (Register, so the outbox row commits atomically with the user
// row it belongs to).
//
// Ordering note: SetVerifyToken talks to Redis, which is not part of
// whatever Postgres transaction q may belong to. If the caller's
// transaction later rolls back — which this method can itself cause, since
// rendering and the EnqueueEmail insert both run after SetVerifyToken has
// already succeeded — the Redis key would outlive the user it names. That
// is harmless: VerifyEmail's subsequent GetUserByID against a
// no-longer-existent id returns pgx.ErrNoRows, which maps to the same
// INVALID_VERIFICATION_TOKEN code as a bad token, and the key self-expires
// in 24h regardless. Two-store atomicity is not attempted — see the
// email-verification plan §8.
func (s *Service) SendVerificationEmail(ctx context.Context, q emailQueue, userID uuid.UUID, to string, displayName *string) error {
	rawToken, err := generateToken()
	if err != nil {
		return err
	}

	if err := s.mail.SetVerifyToken(ctx, hashToken(rawToken), userID); err != nil {
		return err
	}

	name := ""
	if displayName != nil {
		name = *displayName
	}

	verifyURL := fmt.Sprintf("%s/verify-email?token=%s", s.appURL, rawToken)
	msg, err := s.render.VerifyEmail(to, email.VerifyEmailData{
		DisplayName: name,
		VerifyURL:   verifyURL,
		ExpiresIn:   email.VerifyEmailExpiresIn,
	})
	if err != nil {
		return err
	}

	_, err = q.EnqueueEmail(ctx, db.EnqueueEmailParams{
		ToAddress: msg.To,
		Subject:   msg.Subject,
		BodyHtml:  &msg.HTML,
		BodyText:  &msg.Text,
	})
	return err
}

// VerifyEmail consumes token (single-use, via Redis GETDEL) and marks the
// user it names as verified. An unknown/expired/already-consumed token and
// a token naming a user that no longer exists both return
// INVALID_VERIFICATION_TOKEN — never distinguishing the two, so a caller
// cannot use this to probe for a deleted-account id. Reaching this method
// for an already-verified user is idempotent: the token is gone either way
// (GETDEL already consumed it), so this returns nil without writing a
// second audit row.
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	userID, found, err := s.mail.ConsumeVerifyToken(ctx, hashToken(token))
	if err != nil {
		return err
	}
	if !found {
		return apperror.New(apperror.InvalidVerificationToken)
	}

	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.InvalidVerificationToken)
		}
		return err
	}

	if user.IsVerified {
		return nil
	}

	if err := s.store.MarkUserVerified(ctx, userID); err != nil {
		return err
	}

	s.audit.Record(ctx, auditlog.ActionUserEmailVerified, &userID, nil, nil)

	return nil
}

// ResendVerificationEmail re-sends the verification mail for userID,
// subject to the 5-minute cooldown. USER_NOT_FOUND when the user does not
// exist, ALREADY_VERIFIED when it is already verified, and
// VERIFICATION_RESEND_TOO_SOON when the cooldown is still active — checked
// in that order.
func (s *Service) ResendVerificationEmail(ctx context.Context, userID uuid.UUID) error {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.UserNotFound)
		}
		return err
	}

	if user.IsVerified {
		return apperror.New(apperror.AlreadyVerified)
	}

	sent, err := s.mail.MarkVerifyResent(ctx, userID)
	if err != nil {
		return err
	}
	if !sent {
		return apperror.New(apperror.VerificationResendTooSoon)
	}

	return s.SendVerificationEmail(ctx, s.store, userID, user.Email, user.DisplayName)
}

// generateToken returns a fresh base64url-encoded raw token backed by
// tokenRandomBytes of crypto/rand output. Shared by both token kinds this
// package mints (email verification, password reset) — the entropy budget
// and encoding are the same regardless of what the token is redeemed for.
func generateToken() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken returns the hex-encoded SHA-256 digest of a raw verification or
// password-reset token — the form actually stored as a Redis key, per
// CLAUDE.md's Redis key conventions and the mcp_api_keys precedent
// (docs/07-sheets-adapter-decisions.md §1): not because Redis is
// untrusted, but so a KEYS/MONITOR/RDB dump yields nothing usable.
func hashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}
