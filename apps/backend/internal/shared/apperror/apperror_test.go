package apperror

import (
	"testing"
)

func TestResolveKnownCodes(t *testing.T) {
	codes := []string{
		EmailTaken, InvalidCredentials, TooManyAttempts, InvalidRefreshToken,
		RefreshTokenReuse, RefreshTokenExpired, SlugTaken, UserNotFound,
		AlreadyMember, MemberNotFound, CannotRemoveOwner, LimitExceeded,
		RoleNotFound, Forbidden, NotFound, ConnectorNameTaken,
		InvalidConnectorType, HealthCheckUnsupported, MCPKeyNotFound,
		MCPKeyNameTaken, AlreadyVerified, InvalidVerificationToken,
		VerificationResendTooSoon, InvalidResetToken, RateLimited,
		AccountSuspended,
		ReauthFailed, CannotTargetSelf, TargetIsPlatformStaff, SuperadminLimit,
		PlanInUse, PlanPriceLastActive, ImpersonationReadOnly,
		CannotImpersonateStaff, OrgConfirmMismatch, TwoFactorRequired,
		TOTPNotEnrolled, InvalidTOTPCode, BillingNotConfigured,
		PlanNotPurchasable, BillingProviderError, WebhookSignatureInvalid,
	}

	// codes is enumerated by hand, so an omission weakens this test silently —
	// which is exactly what happened when it was first written. Every declared
	// code has a Map entry, so the table's size is the authority on the list's.
	if len(codes) != len(Map) {
		t.Fatalf("codes lists %d of the %d codes in Map; add the missing ones", len(codes), len(Map))
	}

	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			status, message := Resolve(code)
			if status < 400 || status > 599 {
				t.Errorf("Resolve(%s) returned unexpected status: %d", code, status)
			}
			if message == "" {
				t.Errorf("Resolve(%s) returned empty message", code)
			}
			if message == "Internal server error" {
				t.Errorf("Resolve(%s) returned fallback message", code)
			}
		})
	}
}

func TestResolveUnknownCode(t *testing.T) {
	status, message := Resolve("NOPE_NOT_A_CODE")
	if status != 500 || message != "Internal server error" {
		t.Errorf("Resolve(\"NOPE_NOT_A_CODE\") returned unexpected status and/or message: %d, %s", status, message)
	}
}

func TestErrorImplementsError(t *testing.T) {
	err := New(EmailTaken)
	if err.Error() != EmailTaken {
		t.Errorf("New(%s).Error() returned unexpected message: %s", EmailTaken, err.Error())
	}
	var _ error = err
}
