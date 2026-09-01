// Package apperror defines the service-level error codes shared across
// modules and their mapping to HTTP status codes and messages, mirroring
// the ERROR_MAP in the source Node implementation (src/shared/errors).
package apperror

// Error is a typed, code-only error returned by services. Handlers never
// inspect Error() text for control flow — they compare Code, or simply
// let it propagate to the server's HTTPErrorHandler, which calls Resolve.
type Error struct {
	Code string
}

func (e *Error) Error() string {
	return e.Code
}

// New constructs an *Error carrying the given code.
func New(code string) *Error {
	return &Error{Code: code}
}

// mapping describes the HTTP status and message a given error code resolves to.
type mapping struct {
	Status  int
	Message string
}

// Known service error codes (mirrors docs/02-api-contract.md).
const (
	EmailTaken          = "EMAIL_TAKEN"
	InvalidCredentials  = "INVALID_CREDENTIALS"
	TooManyAttempts     = "TOO_MANY_ATTEMPTS"
	InvalidRefreshToken = "INVALID_REFRESH_TOKEN"
	RefreshTokenReuse   = "REFRESH_TOKEN_REUSE"
	RefreshTokenExpired = "REFRESH_TOKEN_EXPIRED"
	SlugTaken           = "SLUG_TAKEN"
	UserNotFound        = "USER_NOT_FOUND"
	AlreadyMember       = "ALREADY_MEMBER"
	MemberNotFound      = "MEMBER_NOT_FOUND"
	CannotRemoveOwner   = "CANNOT_REMOVE_OWNER"
	LimitExceeded       = "LIMIT_EXCEEDED"
	RoleNotFound        = "ROLE_NOT_FOUND"
	Forbidden           = "FORBIDDEN"
	NotFound            = "NOT_FOUND"

	ConnectorNameTaken     = "CONNECTOR_NAME_TAKEN"
	InvalidConnectorType   = "INVALID_CONNECTOR_TYPE"
	HealthCheckUnsupported = "HEALTH_CHECK_UNSUPPORTED"

	MCPKeyNotFound  = "MCP_KEY_NOT_FOUND"
	MCPKeyNameTaken = "MCP_KEY_NAME_TAKEN"

	AlreadyVerified           = "ALREADY_VERIFIED"
	InvalidVerificationToken  = "INVALID_VERIFICATION_TOKEN"
	VerificationResendTooSoon = "VERIFICATION_RESEND_TOO_SOON"

	// InvalidResetToken is deliberately the only password-reset error
	// code: there is no RESET_TOO_SOON, because the cooldown on
	// POST /auth/forgot-password must not be observable — the route
	// always returns 200 regardless. See RequestPasswordReset's doc
	// comment.
	InvalidResetToken = "INVALID_RESET_TOKEN"

	// RateLimited is the rate-limit exhaustion code. The MCP file-download
	// route returns it; a tools/call denial does not, since mcp.RateLimited
	// builds a CallToolResult carrying the concrete retry-after instead of
	// this generic message.
	RateLimited = "RATE_LIMITED"

	// AccountSuspended is POST /auth/login's response to a banned
	// credential: 403, since the login attempt never issues a token.
	// Contrast the 401 "Account suspended" that internal/middleware.Guards
	// .verify returns for an already-issued, now-banned token — that
	// asymmetry is intentional (docs/11-admin-panel.md §4) and is not
	// unified with this code.
	AccountSuspended = "ACCOUNT_SUSPENDED"

	// ReauthFailed is returned when a destructive admin operation's
	// password-confirmation step (docs/11-admin-panel.md D4) fails.
	ReauthFailed = "REAUTH_FAILED"

	// CannotTargetSelf guards admin mutations a staff member must not be
	// able to perform on their own account (ban, platform-role change).
	CannotTargetSelf = "CANNOT_TARGET_SELF"

	// TargetIsPlatformStaff guards against banning or deleting a platform
	// staff account directly — it must be demoted first.
	TargetIsPlatformStaff = "TARGET_IS_PLATFORM_STAFF"

	// SuperadminLimit caps how many accounts may simultaneously hold
	// platform_role = 'superadmin' (admin.superadminCap, currently 10) —
	// a scripting-mistake guard on PATCH /admin/users/:userId/platform-role,
	// not a floor on the last superadmin.
	SuperadminLimit = "SUPERADMIN_LIMIT"

	// PlanInUse guards against deleting a plan with active subscriptions.
	PlanInUse = "PLAN_IN_USE"

	// ImpersonationReadOnly is returned when an impersonated session (see
	// docs/11-admin-panel.md §5) attempts a non-GET/HEAD/OPTIONS request.
	ImpersonationReadOnly = "IMPERSONATION_READ_ONLY"

	// CannotImpersonateStaff guards against impersonating a platform staff
	// account, closing off impersonation as a privilege-escalation ladder.
	CannotImpersonateStaff = "CANNOT_IMPERSONATE_STAFF"

	// OrgConfirmMismatch is DELETE /admin/organizations/:orgId's response
	// when the request body's confirm field doesn't equal the target org's
	// own slug (docs/11-admin-panel.md D4) — typing the slug out correctly
	// is the deliberate friction on an otherwise-irreversible delete. Not
	// in the original execution plan's Task 1.8 table; added here because
	// no existing code fits "the confirmation value didn't match" (403
	// ReauthFailed already covers the password half of the same request).
	OrgConfirmMismatch = "ORG_CONFIRM_MISMATCH"

	// TwoFactorRequired is RequirePlatformRole's response when
	// ADMIN_REQUIRE_2FA=true and the caller has no live admin:2fa:<userId>
	// Redis key (execution plan Task 6.3) — every /admin route except
	// POST /admin/2fa/{enroll,confirm,verify} is gated on it.
	TwoFactorRequired = "TWO_FACTOR_REQUIRED"

	// TOTPNotEnrolled is POST /admin/2fa/{confirm,verify}'s response when
	// the caller has no user_totp row (confirm) or no CONFIRMED row
	// (verify) — enroll must run first.
	TOTPNotEnrolled = "TOTP_NOT_ENROLLED"

	// InvalidTOTPCode is POST /admin/2fa/{confirm,verify}'s response to a
	// code that matches neither the current TOTP window nor (verify only)
	// any unused recovery code. Deliberately one code for both failure
	// modes — same reasoning as InvalidCredentials: which one is wrong
	// must not be observable.
	InvalidTOTPCode = "INVALID_TOTP_CODE"
)

// Map is the full code → (status, message) table from docs/02-api-contract.md.
// No service emits these codes yet in Phase 0 — the table exists so the
// server's error handler and later phases can rely on it immediately.
var Map = map[string]mapping{
	EmailTaken:          {409, "Email already taken"},
	InvalidCredentials:  {401, "Invalid email or password"},
	TooManyAttempts:     {429, "Too many login attempts, try again in 15 minutes"},
	InvalidRefreshToken: {401, "Invalid refresh token"},
	RefreshTokenReuse:   {401, "Refresh token reuse detected"},
	RefreshTokenExpired: {401, "Refresh token expired"},
	SlugTaken:           {409, "Organization slug already taken"},
	UserNotFound:        {404, "User not found"},
	AlreadyMember:       {409, "User is already a member"},
	MemberNotFound:      {404, "Member not found"},
	CannotRemoveOwner:   {403, "Cannot remove organization owner"},
	LimitExceeded:       {403, "Plan limit exceeded"},
	RoleNotFound:        {404, "Role not found"},
	Forbidden:           {403, "Insufficient permissions"},
	NotFound:            {404, "Resource not found"},

	ConnectorNameTaken:     {409, "Connector name already taken"},
	InvalidConnectorType:   {422, "Unsupported connector type"},
	HealthCheckUnsupported: {501, "Health check not supported for this connector type"},

	MCPKeyNotFound:  {404, "MCP key not found"},
	MCPKeyNameTaken: {409, "MCP key name already taken"},

	RateLimited: {429, "Rate limit exceeded, try again later"},

	AlreadyVerified:           {409, "Email already verified"},
	InvalidVerificationToken:  {400, "Invalid or expired verification token"},
	VerificationResendTooSoon: {429, "Verification email already sent, try again in a few minutes"},

	InvalidResetToken: {400, "Invalid or expired password reset token"},

	AccountSuspended:       {403, "Account suspended"},
	ReauthFailed:           {403, "Password confirmation failed"},
	CannotTargetSelf:       {403, "Cannot perform this action on your own account"},
	TargetIsPlatformStaff:  {409, "Demote this account before banning or deleting it"},
	SuperadminLimit:        {409, "Too many superadmin accounts"},
	PlanInUse:              {409, "Plan has active subscriptions"},
	ImpersonationReadOnly:  {403, "Impersonated sessions are read-only"},
	CannotImpersonateStaff: {403, "Cannot impersonate a platform staff account"},
	OrgConfirmMismatch:     {400, "Confirmation does not match the organization's slug"},

	TwoFactorRequired: {403, "Two-factor authentication required"},
	TOTPNotEnrolled:   {400, "Two-factor authentication not enrolled"},
	InvalidTOTPCode:   {401, "Invalid two-factor code"},
}

// Resolve returns the HTTP status and message for a known code, or
// (500, "Internal server error") for anything unrecognized.
func Resolve(code string) (int, string) {
	if m, ok := Map[code]; ok {
		return m.Status, m.Message
	}
	return 500, "Internal server error"
}
