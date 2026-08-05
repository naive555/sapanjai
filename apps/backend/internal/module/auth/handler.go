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

	"github.com/junctera/backend/internal/infra/database"
	"github.com/junctera/backend/internal/infra/database/db"
	"github.com/junctera/backend/internal/infra/redis"
	"github.com/junctera/backend/internal/shared/httpx"
)

// blacklistTTL mirrors the hard-coded 15-minute access-token blacklist TTL
// in the source app's src/modules/auth/index.ts logout handler.
const blacklistTTL = 15 * time.Minute

// sessionStore is the subset of *database.Store the handler needs directly:
// logout looks up the session by refresh token itself (not through Service)
// since an unknown/expired refresh token must still return a 200 success.
type sessionStore interface {
	GetSessionByRefreshToken(ctx context.Context, refreshToken string) (db.Session, error)
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

// Register mounts the four /auth routes on the given group.
func (h *Handler) Register(g *echo.Group) {
	g.POST("/register", h.register)
	g.POST("/login", h.login)
	g.POST("/refresh", h.refresh)
	g.POST("/logout", h.logout)
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
