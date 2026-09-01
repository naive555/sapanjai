// Package auth implements the /auth module: register, login, refresh,
// logout. Mirrors src/modules/auth in the source app.
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/shared/authtoken"
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

// accessClaims is the access-token payload: { sub, email?, act?, imp? }.
// email is omitted when empty — POST /auth/refresh issues an access token
// with sub only, per docs/02-api-contract.md.
//
// act/imp are present only on impersonation tokens (docs/11-admin-panel.md
// §5). They look like they contradict D1 ("roles are not claims"), and the
// distinction is worth stating: act/imp describe *this token* — immutable
// for its lifetime and gone in 10 minutes, so there is no stale state for
// them to hold. platform_role describes a mutable database row, which is
// exactly why it is re-read per request instead.
type accessClaims struct {
	Email string `json:"email,omitempty"`
	// Actor is the platform-staff user id on whose behalf this token was
	// issued (RFC 8693's "act"). Present only on impersonation tokens.
	Actor string `json:"act,omitempty"`
	// Impersonated marks the token read-only at the guard.
	Impersonated bool `json:"imp,omitempty"`
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

// SignImpersonationToken signs a short-lived access token that authenticates
// AS targetID while recording actorID (the platform staff member) in the
// "act" claim. ttl is passed explicitly rather than read from config's
// access TTL: an impersonation token is deliberately much shorter-lived
// than an ordinary one, and the caller owns that policy.
//
// There is no matching refresh token by design — the token cannot be
// extended, only re-issued, and a re-issue writes a fresh audit entry
// (docs/11-admin-panel.md §5). No sessions row is created either, which is
// what makes POST /auth/refresh reject it: the refresh path looks the token
// up in sessions and finds nothing.
func (t *TokenService) SignImpersonationToken(targetID uuid.UUID, targetEmail string, actorID uuid.UUID, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := accessClaims{
		Email:        targetEmail,
		Actor:        actorID.String(),
		Impersonated: true,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   targetID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
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
// with the access secret and returns its claims. Any parse/signature/expiry
// failure or missing subject is returned as an error; the auth guard maps
// it to 401 "Unauthorized".
//
// An impersonation token whose "act" claim is unparseable is rejected
// rather than downgraded to a normal token: imp and act are set together
// at signing, so a token carrying one without a usable other has been
// tampered with or mis-minted, and silently treating it as an ordinary
// token would strip exactly the marker that makes it read-only.
func (t *TokenService) VerifyAccessToken(tokenString string) (authtoken.AccessToken, error) {
	claims := &accessClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return t.accessSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return authtoken.AccessToken{}, err
	}

	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return authtoken.AccessToken{}, errors.New("access token missing subject")
	}

	userID, err := uuid.Parse(sub)
	if err != nil {
		return authtoken.AccessToken{}, err
	}

	out := authtoken.AccessToken{UserID: userID, Email: claims.Email, Impersonated: claims.Impersonated}
	if claims.Impersonated {
		actorID, err := uuid.Parse(claims.Actor)
		if err != nil {
			return authtoken.AccessToken{}, errors.New("impersonation token missing a valid actor claim")
		}
		out.ActorID = actorID
	}

	return out, nil
}
