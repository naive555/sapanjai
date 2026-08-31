package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pquerna/otp/totp"

	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/shared/apperror"
)

// fakeSealer stands in for *envelope.Encryptor in these unit tests: a real
// Encryptor is exercised end-to-end by internal/module/connector's own
// tests and by the server integration suite, so re-testing AES-GCM here
// would just be redundant. This fake reversibly wraps plaintext as base64
// inside a tiny JSON envelope and asserts aad round-trips unchanged,
// which is the one contract totp.go actually depends on (Open must be
// called with the same aad Seal was).
type fakeSealer struct{}

type fakeSealedEnvelope struct {
	PT  string `json:"pt"`
	AAD string `json:"aad"`
}

func (fakeSealer) Seal(ctx context.Context, plaintext, aad []byte) (json.RawMessage, error) {
	b, err := json.Marshal(fakeSealedEnvelope{
		PT:  base64.StdEncoding.EncodeToString(plaintext),
		AAD: base64.StdEncoding.EncodeToString(aad),
	})
	return json.RawMessage(b), err
}

func (fakeSealer) Open(ctx context.Context, raw json.RawMessage, aad []byte) ([]byte, error) {
	var env fakeSealedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	wantAAD := base64.StdEncoding.EncodeToString(aad)
	if env.AAD != wantAAD {
		return nil, errAADMismatch
	}
	return base64.StdEncoding.DecodeString(env.PT)
}

var errAADMismatch = &fakeAADError{}

type fakeAADError struct{}

func (*fakeAADError) Error() string { return "fakeSealer: aad mismatch" }

// newTOTPTestService wires a Service with a real fakeSealer, an in-memory
// user_totp row keyed by user id (so Enroll -> Confirm -> Verify chain
// correctly across calls, the way the real store does), and a mockAdminAuth
// standing in for Redis. Returns the service and the backing row map so
// tests can assert on persisted state directly.
func newTOTPTestService(t *testing.T) (*Service, map[uuid.UUID]*db.UserTotp, *mockAdminAuth) {
	t.Helper()

	rows := map[uuid.UUID]*db.UserTotp{}
	auth := &mockAdminAuth{}

	store := &mockAdminStore{
		upsertUserTOTPSecret: func(ctx context.Context, arg db.UpsertUserTOTPSecretParams) error {
			rows[arg.UserID] = &db.UserTotp{
				UserID:          arg.UserID,
				SecretEncrypted: arg.SecretEncrypted,
				RecoveryCodes:   []string{},
			}
			return nil
		},
		getUserTOTP: func(ctx context.Context, userID uuid.UUID) (db.UserTotp, error) {
			row, ok := rows[userID]
			if !ok {
				return db.UserTotp{}, pgx.ErrNoRows
			}
			return *row, nil
		},
		confirmUserTOTP: func(ctx context.Context, arg db.ConfirmUserTOTPParams) error {
			row, ok := rows[arg.UserID]
			if !ok {
				return pgx.ErrNoRows
			}
			row.RecoveryCodes = arg.RecoveryCodes
			row.ConfirmedAt = pgtype.Timestamp{Time: time.Now(), Valid: true}
			return nil
		},
		updateUserTOTPRecoveryCodes: func(ctx context.Context, arg db.UpdateUserTOTPRecoveryCodesParams) error {
			row, ok := rows[arg.UserID]
			if !ok {
				return pgx.ErrNoRows
			}
			row.RecoveryCodes = arg.RecoveryCodes
			return nil
		},
	}

	return NewService(store, nil, nil, nil, auth, nil, fakeSealer{}, nil), rows, auth
}

func TestTOTP_EnrollConfirmVerify_HappyPath(t *testing.T) {
	svc, rows, _ := newTOTPTestService(t)
	userID := uuid.New()
	ctx := context.Background()

	enrollResp, err := svc.EnrollTOTP(ctx, userID, "alice@example.com")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	if enrollResp.OtpauthURI == "" {
		t.Fatal("EnrollTOTP: empty OtpauthURI")
	}

	row, ok := rows[userID]
	if !ok {
		t.Fatal("EnrollTOTP did not persist a user_totp row")
	}
	secret := decodeSealedSecret(t, row.SecretEncrypted, userID)

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}

	confirmResp, err := svc.ConfirmTOTP(ctx, userID, code)
	if err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if len(confirmResp.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("ConfirmTOTP returned %d recovery codes, want %d", len(confirmResp.RecoveryCodes), recoveryCodeCount)
	}
	if !rows[userID].ConfirmedAt.Valid {
		t.Fatal("ConfirmTOTP did not set confirmed_at")
	}

	// A fresh code (same 30s window is fine — GenerateCode for "now" again
	// is deterministic within the window) verifies successfully.
	verifyCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	if err := svc.VerifyTOTP(ctx, userID, verifyCode); err != nil {
		t.Fatalf("VerifyTOTP: unexpected error %v", err)
	}
}

func TestTOTP_Confirm_WrongCodeRejected(t *testing.T) {
	svc, _, _ := newTOTPTestService(t)
	userID := uuid.New()
	ctx := context.Background()

	if _, err := svc.EnrollTOTP(ctx, userID, "alice@example.com"); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}

	_, err := svc.ConfirmTOTP(ctx, userID, "000000")
	assertAppErrorCode(t, err, apperror.InvalidTOTPCode)
}

func TestTOTP_Confirm_NotEnrolledRejected(t *testing.T) {
	svc, _, _ := newTOTPTestService(t)

	_, err := svc.ConfirmTOTP(context.Background(), uuid.New(), "123456")
	assertAppErrorCode(t, err, apperror.TOTPNotEnrolled)
}

// TestTOTP_Verify_RejectsStaleWindow is Task 6.4's explicit requirement:
// a code from well outside the current TOTP window must be rejected. This
// service uses totp.Validate's default (zero skew), so a code from five
// periods (150s) ago is unambiguously stale — no clock-skew tolerance could
// plausibly excuse it.
func TestTOTP_Verify_RejectsStaleWindow(t *testing.T) {
	svc, rows, auth := newTOTPTestService(t)
	userID := uuid.New()
	ctx := context.Background()

	if _, err := svc.EnrollTOTP(ctx, userID, "alice@example.com"); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := decodeSealedSecret(t, rows[userID].SecretEncrypted, userID)
	// ConfirmTOTP with a valid current-window code so the row is confirmed
	// (VerifyTOTP refuses an unconfirmed enrollment with a different code,
	// which would make this test pass for the wrong reason).
	confirmCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	if _, err := svc.ConfirmTOTP(ctx, userID, confirmCode); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	staleCode, err := totp.GenerateCode(secret, time.Now().Add(-150*time.Second))
	if err != nil {
		t.Fatalf("totp.GenerateCode (stale): %v", err)
	}

	err = svc.VerifyTOTP(ctx, userID, staleCode)
	assertAppErrorCode(t, err, apperror.InvalidTOTPCode)
	if auth.incrementTwoFactorCalls != 1 {
		t.Errorf("IncrementTwoFactorAttempts called %d times, want 1", auth.incrementTwoFactorCalls)
	}
	if len(auth.setTwoFactorVerifiedCalls) != 0 {
		t.Errorf("SetTwoFactorVerified must not be called for a rejected code, got %d calls", len(auth.setTwoFactorVerifiedCalls))
	}
}

// TestTOTP_Verify_RecoveryCodeAcceptedExactlyOnce is Task 6.4's other
// explicit requirement.
func TestTOTP_Verify_RecoveryCodeAcceptedExactlyOnce(t *testing.T) {
	svc, rows, _ := newTOTPTestService(t)
	userID := uuid.New()
	ctx := context.Background()

	if _, err := svc.EnrollTOTP(ctx, userID, "alice@example.com"); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := decodeSealedSecret(t, rows[userID].SecretEncrypted, userID)
	confirmCode, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	confirmResp, err := svc.ConfirmTOTP(ctx, userID, confirmCode)
	if err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	recoveryCode := confirmResp.RecoveryCodes[0]

	if err := svc.VerifyTOTP(ctx, userID, recoveryCode); err != nil {
		t.Fatalf("VerifyTOTP with a fresh recovery code: unexpected error %v", err)
	}
	if len(rows[userID].RecoveryCodes) != recoveryCodeCount-1 {
		t.Fatalf("recovery codes remaining = %d, want %d", len(rows[userID].RecoveryCodes), recoveryCodeCount-1)
	}

	err = svc.VerifyTOTP(ctx, userID, recoveryCode)
	assertAppErrorCode(t, err, apperror.InvalidTOTPCode)
}

func TestTOTP_Verify_RateLimited(t *testing.T) {
	svc, _, auth := newTOTPTestService(t)
	userID := uuid.New()
	auth.getTwoFactorAttempts = func(ctx context.Context, gotUserID uuid.UUID) (int, error) {
		return maxTwoFactorAttempts, nil
	}

	err := svc.VerifyTOTP(context.Background(), userID, "123456")
	assertAppErrorCode(t, err, apperror.TooManyAttempts)
}

func TestTOTP_Verify_NotEnrolledRejected(t *testing.T) {
	svc, _, _ := newTOTPTestService(t)

	err := svc.VerifyTOTP(context.Background(), uuid.New(), "123456")
	assertAppErrorCode(t, err, apperror.TOTPNotEnrolled)
}

// TestTOTP_Verify_UnconfirmedRejected: an enrolled-but-never-confirmed
// secret must not be usable at the step-up check.
func TestTOTP_Verify_UnconfirmedRejected(t *testing.T) {
	svc, rows, _ := newTOTPTestService(t)
	userID := uuid.New()
	ctx := context.Background()

	if _, err := svc.EnrollTOTP(ctx, userID, "alice@example.com"); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := decodeSealedSecret(t, rows[userID].SecretEncrypted, userID)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}

	err = svc.VerifyTOTP(ctx, userID, code)
	assertAppErrorCode(t, err, apperror.TOTPNotEnrolled)
}

// decodeSealedSecret opens row's fakeSealer-sealed secret under the same
// aad totp.go itself uses (totpAAD(userID)), for tests that need a real
// TOTP code to hand to Confirm/Verify.
func decodeSealedSecret(t *testing.T, sealed json.RawMessage, userID uuid.UUID) string {
	t.Helper()
	plaintext, err := (fakeSealer{}).Open(context.Background(), sealed, totpAAD(userID))
	if err != nil {
		t.Fatalf("decode sealed secret: %v", err)
	}
	return string(plaintext)
}

// assertAppErrorCode asserts err is an *apperror.Error carrying wantCode.
// apperror.Error.Error() returns its Code verbatim (apperror.go), so this
// is a plain string comparison rather than a type-switch dance.
func assertAppErrorCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an apperror %q, got nil", wantCode)
	}
	ae, ok := err.(*apperror.Error)
	if !ok {
		t.Fatalf("expected *apperror.Error, got %T (%v)", err, err)
	}
	if ae.Code != wantCode {
		t.Fatalf("error code = %q, want %q", ae.Code, wantCode)
	}
}
