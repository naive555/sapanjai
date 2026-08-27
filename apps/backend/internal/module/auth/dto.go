package auth

import (
	"time"

	"github.com/google/uuid"
)

// RegisterRequest is the POST /auth/register body, mirroring
// AuthModel.registerBody in the source app.
type RegisterRequest struct {
	Email       string  `json:"email" validate:"required,email"`
	Password    string  `json:"password" validate:"required,min=8"`
	DisplayName *string `json:"displayName" validate:"omitempty,min=1"`
}

// LoginRequest is the POST /auth/login body, mirroring AuthModel.loginBody.
type LoginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

// RefreshRequest is the POST /auth/refresh and POST /auth/logout body,
// mirroring AuthModel.refreshBody (both routes accept { refreshToken }).
type RefreshRequest struct {
	RefreshToken string `json:"refreshToken" validate:"required"`
}

// TokenResponse is the response body for register/login/refresh, mirroring
// AuthModel.tokenResponse.
type TokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// LogoutResponse is the response body for POST /auth/logout.
type LogoutResponse struct {
	Success bool `json:"success"`
}

// VerifyEmailRequest is the POST /auth/verify-email body.
type VerifyEmailRequest struct {
	Token string `json:"token" validate:"required"`
}

// SuccessResponse is the response body for POST /auth/verify-email and
// POST /auth/resend-verification.
type SuccessResponse struct {
	Success bool `json:"success"`
}

// MeResponse is the response body for GET /auth/me. IsVerified is read
// fresh from the database on every call rather than carried in the access
// token — see the handler's doc comment.
type MeResponse struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName *string   `json:"displayName"`
	IsVerified  bool      `json:"isVerified"`
	CreatedAt   time.Time `json:"createdAt"`
}
