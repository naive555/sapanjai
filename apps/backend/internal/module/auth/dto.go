package auth

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
