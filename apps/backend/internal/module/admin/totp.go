// Phase 6, Task 6.3: TOTP step-up for the admin console. Three routes,
// none of them login — step-up rather than folded into POST /auth/login
// because login is a documented contract route shared with every tenant
// user (docs/02-api-contract.md), and changing its response shape for a
// handful of staff accounts would be a breaking change to that contract for
// no benefit to anyone but platform staff.
//
// Never log a TOTP secret, a recovery code, or an otpauth:// URI anywhere
// in this file — these are bearer secrets carried in VALUES, which the
// key-based redaction in internal/shared/logger/redact.go cannot catch (the
// same hazard CLAUDE.md documents for rendered email bodies). No method
// here logs one; s.log is used elsewhere in this package but deliberately
// never touches these three flows.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp/totp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// totpIssuer names the platform in the otpauth:// URI, which is what an
// authenticator app displays next to the account — worth getting right
// since staff will be looking at it in a list alongside every other TOTP
// entry they hold.
const totpIssuer = "Sapanjai Admin"

// recoveryCodeCount and recoveryCodeBytes back Task 6.3's "ten 128-bit
// CSPRNG codes" exactly: 16 bytes = 128 bits, hex-encoded for something a
// human can type without a QR scanner.
const (
	recoveryCodeCount = 10
	recoveryCodeBytes = 16
)

// maxTwoFactorAttempts mirrors maxReauthAttempts: same cap, same 15-minute
// window, independent counter (execution plan Task 6.3 — "reusing the
// existing limiter pattern"). Without this, POST /admin/2fa/verify's 6-digit
// code is a 1,000,000-value space an attacker holding a stolen admin access
// token could brute force in seconds.
const maxTwoFactorAttempts = 5

// totpAAD binds a sealed TOTP secret to its owning user, mirroring
// connector's aad(organizationID): a row somehow copied to another user's
// id fails to open rather than decrypting under the wrong identity.
func totpAAD(userID uuid.UUID) []byte {
	return userID[:]
}

// EnrollTOTP generates a fresh TOTP secret for userID and seals it under
// CONNECTOR_MASTER_KEY, returning the otpauth:// URI an authenticator app
// consumes to add the account. Re-callable: a staff member who lost their
// authenticator before confirming (or wants to replace it) can enroll
// again, which wipes any prior confirmation and recovery codes along with
// the old secret (see UpsertUserTOTPSecret's own doc comment) — an
// unconfirmed or superseded secret must never leave stale recovery codes
// tied to a key that no longer exists.
//
// The returned URI is a bearer secret for as long as it's valid — anyone
// who scans it can generate valid codes — which is why it is returned
// exactly once and never persisted anywhere in cleartext, including logs.
func (s *Service) EnrollTOTP(ctx context.Context, userID uuid.UUID, email string) (TOTPEnrollResponse, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: email,
	})
	if err != nil {
		return TOTPEnrollResponse{}, fmt.Errorf("generate totp secret: %w", err)
	}

	sealed, err := s.crypto.Seal(ctx, []byte(key.Secret()), totpAAD(userID))
	if err != nil {
		return TOTPEnrollResponse{}, err
	}

	if err := s.store.UpsertUserTOTPSecret(ctx, db.UpsertUserTOTPSecretParams{
		UserID:          userID,
		SecretEncrypted: sealed,
	}); err != nil {
		return TOTPEnrollResponse{}, err
	}

	return TOTPEnrollResponse{OtpauthURI: key.URL()}, nil
}

// ConfirmTOTP verifies code against the secret from the most recent Enroll
// call and, on success, stamps confirmed_at and mints ten recovery codes —
// the only time recovery codes are (re)generated, since a re-enroll always
// wipes the previous set along with the secret they were issued against.
// The raw codes are returned exactly once; only their SHA-256 hashes are
// persisted (Task 6.3: "not bcrypt" — same reasoning CLAUDE.md records for
// MCP PATs, since these are CSPRNG output, not a low-entropy password).
func (s *Service) ConfirmTOTP(ctx context.Context, userID uuid.UUID, code string) (TOTPConfirmResponse, error) {
	row, err := s.store.GetUserTOTP(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TOTPConfirmResponse{}, apperror.New(apperror.TOTPNotEnrolled)
		}
		return TOTPConfirmResponse{}, err
	}

	secret, err := s.crypto.Open(ctx, row.SecretEncrypted, totpAAD(userID))
	if err != nil {
		return TOTPConfirmResponse{}, err
	}

	if !totp.Validate(code, string(secret)) {
		return TOTPConfirmResponse{}, apperror.New(apperror.InvalidTOTPCode)
	}

	rawCodes, hashedCodes, err := generateRecoveryCodes()
	if err != nil {
		return TOTPConfirmResponse{}, err
	}

	if err := s.store.ConfirmUserTOTP(ctx, db.ConfirmUserTOTPParams{
		UserID:        userID,
		RecoveryCodes: hashedCodes,
	}); err != nil {
		return TOTPConfirmResponse{}, err
	}

	return TOTPConfirmResponse{RecoveryCodes: rawCodes}, nil
}

// VerifyTOTP is the step-up check itself: on success it sets the
// admin:2fa:<userId> Redis key RequirePlatformRole consults (12h TTL,
// internal/infra/redis/auth.go). code is tried as a live TOTP code first,
// then as an unused recovery code — accepting either at the same endpoint
// keeps the console's step-up prompt to one field regardless of which the
// caller has to hand. A matched recovery code is deleted from the stored
// set immediately (Task 6.3: "deleted on use"), so it verifies exactly
// once even if the same request were somehow replayed.
//
// Rate-limited independently of every other admin limiter
// (admin:2fa:attempts:<userId>, checked before either comparison runs) —
// without it this is a 6-digit brute-force target for anyone holding a
// stolen admin access token.
func (s *Service) VerifyTOTP(ctx context.Context, userID uuid.UUID, code string) error {
	attempts, err := s.redisAuth.GetTwoFactorAttempts(ctx, userID)
	if err != nil {
		return err
	}
	if attempts >= maxTwoFactorAttempts {
		return apperror.New(apperror.TooManyAttempts)
	}

	row, err := s.store.GetUserTOTP(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apperror.New(apperror.TOTPNotEnrolled)
		}
		return err
	}
	if !row.ConfirmedAt.Valid {
		// A secret exists but confirm never landed — treat it the same as
		// "not enrolled" rather than leaking the in-progress state; either
		// way the caller's next step is the same (finish enrolling).
		return apperror.New(apperror.TOTPNotEnrolled)
	}

	secret, err := s.crypto.Open(ctx, row.SecretEncrypted, totpAAD(userID))
	if err != nil {
		return err
	}

	if totp.Validate(code, string(secret)) {
		if err := s.redisAuth.ResetTwoFactorAttempts(ctx, userID); err != nil {
			return err
		}
		return s.redisAuth.SetTwoFactorVerified(ctx, userID)
	}

	if remaining, matched := consumeRecoveryCode(row.RecoveryCodes, code); matched {
		if err := s.store.UpdateUserTOTPRecoveryCodes(ctx, db.UpdateUserTOTPRecoveryCodesParams{
			UserID:        userID,
			RecoveryCodes: remaining,
		}); err != nil {
			return err
		}
		if err := s.redisAuth.ResetTwoFactorAttempts(ctx, userID); err != nil {
			return err
		}
		return s.redisAuth.SetTwoFactorVerified(ctx, userID)
	}

	if _, err := s.redisAuth.IncrementTwoFactorAttempts(ctx, userID); err != nil {
		return err
	}
	return apperror.New(apperror.InvalidTOTPCode)
}

// generateRecoveryCodes returns recoveryCodeCount fresh codes as (raw hex
// strings to show the caller once, SHA-256 hashes to persist). Raw and
// hashed slices are index-aligned.
func generateRecoveryCodes() (raw []string, hashed []string, err error) {
	raw = make([]string, recoveryCodeCount)
	hashed = make([]string, recoveryCodeCount)
	for i := range raw {
		b := make([]byte, recoveryCodeBytes)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, fmt.Errorf("generate recovery code: %w", err)
		}
		code := hex.EncodeToString(b)
		raw[i] = code
		hashed[i] = hashRecoveryCode(code)
	}
	return raw, hashed, nil
}

// hashRecoveryCode SHA-256-hashes a recovery code for storage/comparison —
// not bcrypt, since a recovery code is 128 bits of CSPRNG output rather
// than a low-entropy password an attacker could feasibly dictionary-guess
// (the same reasoning CLAUDE.md records for MCP PAT hashing).
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// consumeRecoveryCode looks for code among storedHashes (each a
// hashRecoveryCode output) using a constant-time comparison per entry —
// this is a secret-bearing comparison against attacker-controlled input,
// same reasoning as any token/password compare elsewhere in this codebase.
// On a match it returns the remaining hashes with that one removed;
// matched is false (remaining == storedHashes, unmodified) for no match,
// so a caller can always safely persist the returned slice only when
// matched is true.
func consumeRecoveryCode(storedHashes []string, code string) (remaining []string, matched bool) {
	target := hashRecoveryCode(code)
	for i, h := range storedHashes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(target)) == 1 {
			out := make([]string, 0, len(storedHashes)-1)
			out = append(out, storedHashes[:i]...)
			out = append(out, storedHashes[i+1:]...)
			return out, true
		}
	}
	return storedHashes, false
}
