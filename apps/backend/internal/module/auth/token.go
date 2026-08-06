// Package auth implements the /auth module: register, login, refresh,
// logout. Mirrors src/modules/auth in the source app.
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/config"
)

// TokenService signs and verifies the access/refresh JWT pair, HS256, using
// the secrets and access-token TTL from config. Mirrors @elysiajs/jwt usage
// in the source app's src/modules/auth/index.ts.
type TokenService struct {
	accessSecret  []byte
	refreshSecret []byte
	accessTTL     time.Duration
}

// NewTokenService builds a TokenService from application config.
func NewTokenService(cfg *config.Config) *TokenService {
	return &TokenService{
		accessSecret:  []byte(cfg.JWTAccessSecret),
		refreshSecret: []byte(cfg.JWTRefreshSecret),
		accessTTL:     cfg.JWTAccessExpiresIn,
	}
}

// accessClaims is the access-token payload: { sub, email? }. email is
// omitted when empty — POST /auth/refresh issues an access token with sub
// only, per docs/02-api-contract.md.
type accessClaims struct {
	Email string `json:"email,omitempty"`
	jwt.RegisteredClaims
}

// refreshClaims is the refresh-token payload: { sub, jti }. No exp — the
// session row's expires_at is the sole expiry authority, mirroring the
// source, which signs refresh tokens with no embedded exp.
type refreshClaims struct {
	jwt.RegisteredClaims
}

// SignAccessToken signs a short-lived access token. email is embedded only
// when non-empty.
func (t *TokenService) SignAccessToken(userID uuid.UUID, email string) (string, error) {
	now := time.Now()
	claims := accessClaims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.accessTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.accessSecret)
}

// SignRefreshToken signs a refresh token carrying sub and a random jti. The
// jti guarantees a unique token string per call — sessions.refresh_token is
// UNIQUE, and a {sub}-only payload (as in source) would otherwise collide
// for repeated signs of the same user within the same second.
func (t *TokenService) SignRefreshToken(userID uuid.UUID) (string, error) {
	claims := refreshClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  userID.String(),
			IssuedAt: jwt.NewNumericDate(time.Now()),
			ID:       uuid.NewString(),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.refreshSecret)
}

// VerifyRefreshToken parses and validates a refresh token's signature and
// returns its subject as a user ID. Any parse/signature failure is returned
// as an error; handlers map it to 401 "Invalid refresh token".
func (t *TokenService) VerifyRefreshToken(tokenString string) (uuid.UUID, error) {
	claims := &refreshClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return t.refreshSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return uuid.Nil, err
	}

	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return uuid.Nil, errors.New("refresh token missing subject")
	}

	userID, err := uuid.Parse(sub)
	if err != nil {
		return uuid.Nil, err
	}

	return userID, nil
}

// VerifyAccessToken parses and validates an access token's HS256 signature
// with the access secret and returns its subject (user id) and embedded
// email. Any parse/signature/expiry failure or missing subject is returned
// as an error; the auth guard maps it to 401 "Unauthorized".
func (t *TokenService) VerifyAccessToken(tokenString string) (uuid.UUID, string, error) {
	claims := &accessClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return t.accessSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return uuid.Nil, "", err
	}

	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return uuid.Nil, "", errors.New("access token missing subject")
	}

	userID, err := uuid.Parse(sub)
	if err != nil {
		return uuid.Nil, "", err
	}

	return userID, claims.Email, nil
}
