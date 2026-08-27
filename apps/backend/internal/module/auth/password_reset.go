package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/auditlog"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/email"
)

// RequestPasswordReset always returns nil. That is deliberate, not an
// oversight: this route must be enumeration-safe, so its response can never
// reveal whether email belongs to a real account, and it can never reveal
// whether the per-email cooldown (15 min, keyed by email — see
// redis.Email.MarkResetRequested) is active either. A cooldown a caller
// could observe (e.g. by getting back a different status) would itself
// become the enumeration oracle the uniform response exists to close — see
// the email-verification plan §5. Any genuine infrastructure failure
// (Redis or Postgres unreachable) still propagates as a non-nil error, so
// the caller gets a 500 rather than a false "sent" — only the *business*
// outcomes (unknown email, cooldown active) are folded into nil.
//
// Order: MarkResetRequested first, so the cooldown is set (and its own
// enumeration-safety property holds) before any lookup that could depend on
// whether the address exists; then load the user; then generate, store, and
// mail the token; then audit.
func (s *Service) RequestPasswordReset(ctx context.Context, to string) error {
	sent, err := s.mail.MarkResetRequested(ctx, to)
	if err != nil {
		return err
	}
	if !sent {
		// Cooldown already active for this address. Send nothing, and
		// return the same nil every other branch returns — the caller
		// must not be able to tell this apart from "unknown email" or
		// "mail queued".
		return nil
	}

	user, err := s.store.GetUserByEmail(ctx, to)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}

	rawToken, err := generateToken()
	if err != nil {
		return err
	}

	if err := s.mail.SetResetToken(ctx, hashToken(rawToken), user.ID); err != nil {
		return err
	}

	name := ""
	if user.DisplayName != nil {
		name = *user.DisplayName
	}

	resetURL := fmt.Sprintf("%s/reset-password?token=%s", s.appURL, rawToken)
	msg, err := s.render.PasswordReset(user.Email, email.PasswordResetData{
		DisplayName: name,
		ResetURL:    resetURL,
		ExpiresIn:   email.PasswordResetExpiresIn,
	})
	if err != nil {
		return err
	}

	if _, err := s.store.EnqueueEmail(ctx, db.EnqueueEmailParams{
		ToAddress: msg.To,
		Subject:   msg.Subject,
		BodyHtml:  &msg.HTML,
		BodyText:  &msg.Text,
	}); err != nil {
		return err
	}

	s.audit.Record(ctx, auditlog.ActionUserPasswordResetRequested, &user.ID, nil, nil)

	return nil
}

// ResetPassword consumes token (single-use, via Redis GETDEL) and, if
// valid, sets the owning user's password to newPasswordHash. An
// unknown/expired/already-consumed token and a token naming a user that no
// longer exists both return INVALID_RESET_TOKEN — never distinguishing the
// two, matching VerifyEmail's precedent.
//
// The password update, marking the user verified, and revoking every one
// of their sessions all happen in one transaction (store.WithTx): a
// password reset is exactly the kind of event after which a stale session
// must not survive it, and a successful reset — reaching a link only the
// mailbox owner could have received — proves control of the mailbox, which
// is the only thing verification asserts (same call the plan makes for
// verification itself; see the email-verification plan §1). Already-issued
// access tokens are unaffected and remain valid until their own (short)
// expiry — only refresh sessions die immediately.
//
// newPasswordHash is already bcrypt-hashed by the caller (the handler),
// mirroring Register's split so the 72-byte truncation step lives in
// exactly one place.
func (s *Service) ResetPassword(ctx context.Context, token, newPasswordHash string) error {
	userID, found, err := s.mail.ConsumeResetToken(ctx, hashToken(token))
	if err != nil {
		return err
	}
	if !found {
		return apperror.New(apperror.InvalidResetToken)
	}

	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.InvalidResetToken)
		}
		return err
	}

	err = s.store.WithTx(ctx, func(q *db.Queries) error {
		if err := q.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{
			ID:           user.ID,
			PasswordHash: newPasswordHash,
		}); err != nil {
			return err
		}
		if err := q.MarkUserVerified(ctx, user.ID); err != nil {
			return err
		}
		return q.RevokeAllUserSessions(ctx, user.ID)
	})
	if err != nil {
		return err
	}

	s.audit.Record(ctx, auditlog.ActionUserPasswordReset, &user.ID, nil, nil)

	return nil
}
