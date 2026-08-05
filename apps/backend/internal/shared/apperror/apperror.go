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
}

// Resolve returns the HTTP status and message for a known code, or
// (500, "Internal server error") for anything unrecognized.
func Resolve(code string) (int, string) {
	if m, ok := Map[code]; ok {
		return m.Status, m.Message
	}
	return 500, "Internal server error"
}
