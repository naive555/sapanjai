package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/infra/redis"
	appmw "github.com/sapanjai/backend/internal/middleware"
	"github.com/sapanjai/backend/internal/shared/httpx"
)

// blacklistTTL mirrors the hard-coded 15-minute access-token blacklist TTL
// in the source app's src/modules/auth/index.ts logout handler.
const blacklistTTL = 15 * time.Minute

// sessionStore is the subset of *database.Store the handler needs directly:
// logout looks up the session by refresh token itself (not through Service)
// since an unknown/expired refresh token must still return a 200 success;
// me reads the caller's own row directly rather than through Service, since
// it is a plain read with no business rule attached.
type sessionStore interface {
	GetSessionByRefreshToken(ctx context.Context, refreshToken string) (db.Session, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)
}

// blacklister is the subset of *redis.Auth the handler needs for logout.
type blacklister interface {
	BlacklistToken(ctx context.Context, token string, ttl time.Duration) error
}

var (
	_ sessionStore = (*database.Store)(nil)
	_ blacklister  = (*redis.Auth)(nil)
)

// Handler implements the four public /auth routes, mirroring
// src/modules/auth/index.ts.
type Handler struct {
	service    *Service
	token      *TokenService
	store      sessionStore
	blacklist  blacklister
	refreshTTL time.Duration
}

// NewHandler builds an auth Handler. refreshTTL is the session lifetime
// (cfg.JWTRefreshExpiresIn) used to compute each new session's expires_at.
func NewHandler(service *Service, token *TokenService, store sessionStore, blacklist blacklister, refreshTTL time.Duration) *Handler {
	return &Handler{service: service, token: token, store: store, blacklist: blacklist, refreshTTL: refreshTTL}
}

// Register mounts the /auth routes on the given group. verify-email stays
// public (see the email-verification plan §1: the frontend page is the GET
// target, and it POSTs the token here); resend-verification and me require
// a valid access token. forgot-password and reset-password are public too,
// for the same reason as verify-email plus one more: forgot-password's
// entire security property is that its response cannot depend on whether
// the caller is authenticated as anyone in particular.
func (h *Handler) Register(g *echo.Group, guards *appmw.Guards) {
	g.POST("/register", h.register)
	g.POST("/login", h.login)
	g.POST("/refresh", h.refresh)
	g.POST("/logout", h.logout)
	g.POST("/verify-email", h.verifyEmail)
	g.POST("/resend-verification", h.resendVerification, guards.RequireAuth())
	g.GET("/me", h.me, guards.RequireAuth())
	g.POST("/forgot-password", h.forgotPassword)
	g.POST("/reset-password", h.resetPassword)
}

// register creates a new user and returns a fresh access/refresh token pair.
// @Summary  Register a new user
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    body  body      RegisterRequest  true  "Registration payload"
// @Success  200   {object}  TokenResponse
// @Failure  409   {object}  httpx.ErrorResponse  "EMAIL_TAKEN"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /auth/register [post]
func (h *Handler) register(c echo.Context) error {
	var req RegisterRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword(truncatePassword(req.Password), bcryptCost)
	if err != nil {
		return err
	}

	user, err := h.service.Register(c.Request().Context(), req.Email, string(hash), req.DisplayName)
	if err != nil {
		return err
	}

	return h.issueTokenPair(c, user.ID, user.Email)
}

// login authenticates a user and returns a fresh access/refresh token pair.
// @Summary  Log in
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    body  body      LoginRequest  true  "Login credentials"
// @Success  200   {object}  TokenResponse
// @Failure  401   {object}  httpx.ErrorResponse  "INVALID_CREDENTIALS"
// @Failure  429   {object}  httpx.ErrorResponse  "TOO_MANY_ATTEMPTS"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /auth/login [post]
func (h *Handler) login(c echo.Context) error {
	var req LoginRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.service.Login(c.Request().Context(), req.Email, req.Password)
	if err != nil {
		return err
	}

	return h.issueTokenPair(c, user.ID, user.Email)
}

// refresh rotates a refresh token and returns a new access/refresh pair.
// Reuse of a revoked/expired token revokes its entire session family.
// @Summary  Rotate a refresh token
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    body  body      RefreshRequest  true  "Refresh token"
// @Success  200   {object}  TokenResponse
// @Failure  401   {object}  httpx.ErrorResponse  "INVALID_REFRESH_TOKEN / REFRESH_TOKEN_REUSE / REFRESH_TOKEN_EXPIRED"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /auth/refresh [post]
func (h *Handler) refresh(c echo.Context) error {
	var req RefreshRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	userID, err := h.token.VerifyRefreshToken(req.RefreshToken)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "Invalid refresh token")
	}

	newRefreshToken, err := h.token.SignRefreshToken(userID)
	if err != nil {
		return err
	}

	// UTC: sessions.expires_at is "timestamp without time zone" — see the
	// comment on the equivalent expiry check in service.go.
	rotatedUserID, err := h.service.RotateSession(c.Request().Context(), req.RefreshToken, newRefreshToken, time.Now().UTC().Add(h.refreshTTL))
	if err != nil {
		return err
	}

	// sub only, no email — per docs/02-api-contract.md.
	accessToken, err := h.token.SignAccessToken(rotatedUserID, "")
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, TokenResponse{AccessToken: accessToken, RefreshToken: newRefreshToken})
}

// logout blacklists the caller's access token (if present) for 15 minutes
// and revokes all sessions for the refresh token's owner. Always succeeds,
// even for an unknown refresh token.
// @Summary  Log out
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    Authorization  header    string          false  "Bearer <accessToken>"
// @Param    body           body      RefreshRequest  true   "Refresh token"
// @Success  200            {object}  LogoutResponse
// @Failure  422            {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /auth/logout [post]
func (h *Handler) logout(c echo.Context) error {
	var req RefreshRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	ctx := c.Request().Context()

	if accessToken := bearerToken(c); accessToken != "" {
		if err := h.blacklist.BlacklistToken(ctx, accessToken, blacklistTTL); err != nil {
			return err
		}
	}

	session, err := h.store.GetSessionByRefreshToken(ctx, req.RefreshToken)
	switch {
	case err == nil:
		if err := h.service.RevokeAllSessions(ctx, session.UserID); err != nil {
			return err
		}
	case errors.Is(err, pgx.ErrNoRows):
		// unknown refresh token — still a success, matching source.
	default:
		return err
	}

	return c.JSON(http.StatusOK, LogoutResponse{Success: true})
}

// issueTokenPair signs a fresh access/refresh pair for userID, opens a new
// session for the refresh token, and writes the { accessToken, refreshToken
// } response. Shared by register and login.
func (h *Handler) issueTokenPair(c echo.Context, userID uuid.UUID, email string) error {
	ctx := c.Request().Context()

	accessToken, err := h.token.SignAccessToken(userID, email)
	if err != nil {
		return err
	}

	refreshToken, err := h.token.SignRefreshToken(userID)
	if err != nil {
		return err
	}

	// UTC: sessions.expires_at is "timestamp without time zone" — see the
	// comment on the equivalent expiry check in service.go.
	family := uuid.New()
	if err := h.service.CreateSession(ctx, userID, refreshToken, family, time.Now().UTC().Add(h.refreshTTL)); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, TokenResponse{AccessToken: accessToken, RefreshToken: refreshToken})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, or "" if absent — mirrors
// headers.authorization?.replace("Bearer ", "") in the source app.
func bearerToken(c echo.Context) string {
	return strings.TrimPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
}

// verifyEmail consumes a single-use verification token and marks its owning
// user verified. Public: the frontend page at /verify-email?token=... is
// the GET target and POSTs the token here, so link-scanners that prefetch
// the page never burn the token.
// @Summary  Verify an email address
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    body  body      VerifyEmailRequest  true  "Verification token"
// @Success  200   {object}  SuccessResponse
// @Failure  400   {object}  httpx.ErrorResponse  "INVALID_VERIFICATION_TOKEN"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /auth/verify-email [post]
func (h *Handler) verifyEmail(c echo.Context) error {
	var req VerifyEmailRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.VerifyEmail(c.Request().Context(), req.Token); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// resendVerification re-sends the verification email for the authenticated
// caller, subject to a 5-minute cooldown.
// @Summary  Resend the verification email
// @Tags     auth
// @Produce  json
// @Security BearerAuth
// @Success  200  {object}  SuccessResponse
// @Failure  404  {object}  httpx.ErrorResponse  "USER_NOT_FOUND"
// @Failure  409  {object}  httpx.ErrorResponse  "ALREADY_VERIFIED"
// @Failure  429  {object}  httpx.ErrorResponse  "VERIFICATION_RESEND_TOO_SOON"
// @Router   /auth/resend-verification [post]
func (h *Handler) resendVerification(c echo.Context) error {
	userID := appmw.UserID(c)

	if err := h.service.ResendVerificationEmail(c.Request().Context(), userID); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// me returns the authenticated caller's own profile. IsVerified is read
// fresh from the database rather than carried in the access token: the
// contract pins access-token claims at { sub, email } deliberately, since a
// claim would go stale for up to JWT_ACCESS_EXPIRES_IN after verifying and
// the frontend's verification banner would linger past the moment it stops
// being true.
// @Summary  Get the authenticated caller's profile
// @Tags     auth
// @Produce  json
// @Security BearerAuth
// @Success  200  {object}  MeResponse
// @Router   /auth/me [get]
func (h *Handler) me(c echo.Context) error {
	userID := appmw.UserID(c)

	user, err := h.store.GetUserByID(c.Request().Context(), userID)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, MeResponse{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		IsVerified:  user.IsVerified,
		CreatedAt:   user.CreatedAt,
	})
}

// forgotPassword always returns 200 { success: true }, whether or not req.Email
// belongs to an account and whether or not the per-email resend cooldown is
// currently active — see Service.RequestPasswordReset's doc comment for why
// that uniformity is the entire security property this route provides.
// Public.
// @Summary  Request a password-reset email
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    body  body      ForgotPasswordRequest  true  "Account email"
// @Success  200   {object}  SuccessResponse
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /auth/forgot-password [post]
func (h *Handler) forgotPassword(c echo.Context) error {
	var req ForgotPasswordRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.RequestPasswordReset(c.Request().Context(), req.Email); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}

// resetPassword consumes a single-use password-reset token and sets a new
// password for the user it names, revoking every one of their sessions in
// the process. Public. Hashing (and the shared 72-byte truncation) happens
// here rather than in the service, mirroring register's split — see
// truncatePassword's doc comment for why skipping it would make a long
// password behave differently here than at registration.
// @Summary  Reset a password using a reset token
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    body  body      ResetPasswordRequest  true  "Reset token and new password"
// @Success  200   {object}  SuccessResponse
// @Failure  400   {object}  httpx.ErrorResponse  "INVALID_RESET_TOKEN"
// @Failure  422   {object}  httpx.ErrorResponse  "Validation failed"
// @Router   /auth/reset-password [post]
func (h *Handler) resetPassword(c echo.Context) error {
	var req ResetPasswordRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword(truncatePassword(req.Password), bcryptCost)
	if err != nil {
		return err
	}

	if err := h.service.ResetPassword(c.Request().Context(), req.Token, string(hash)); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, SuccessResponse{Success: true})
}
