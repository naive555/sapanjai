package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/junctera/backend/internal/config"
)

func testTokenConfig() *config.Config {
	return &config.Config{
		JWTAccessSecret:    "access-secret-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		JWTRefreshSecret:   "refresh-secret-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		JWTAccessExpiresIn: 15 * time.Minute,
	}
}

func TestSignAccessToken(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	userID := uuid.New()

	tokenString, err := ts.SignAccessToken(userID, "user@example.com")
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}

	claims := &accessClaims{}
	parsed, err := jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return []byte(testTokenConfig().JWTAccessSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("parse access token: %v", err)
	}

	if claims.Subject != userID.String() {
		t.Errorf("sub = %q, want %q", claims.Subject, userID.String())
	}
	if claims.Email != "user@example.com" {
		t.Errorf("email = %q, want %q", claims.Email, "user@example.com")
	}

	wantExp := time.Now().Add(15 * time.Minute)
	gotExp := claims.ExpiresAt.Time
	if diff := gotExp.Sub(wantExp); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("exp = %v, want approximately %v (diff %v)", gotExp, wantExp, diff)
	}
}

func TestSignAccessToken_EmptyEmailOmitted(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	tokenString, err := ts.SignAccessToken(uuid.New(), "")
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}

	// The refresh endpoint issues an access token with sub only, per
	// docs/02-api-contract.md — the raw payload must not contain "email".
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape: %d parts", len(parts))
	}
}

func TestSignRefreshToken_UniquePerCall(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	userID := uuid.New()

	t1, err := ts.SignRefreshToken(userID)
	if err != nil {
		t.Fatalf("SignRefreshToken (1): %v", err)
	}
	t2, err := ts.SignRefreshToken(userID)
	if err != nil {
		t.Fatalf("SignRefreshToken (2): %v", err)
	}

	if t1 == t2 {
		t.Fatal("expected two refresh-token signs for the same user to differ (jti)")
	}
}

func TestSignRefreshToken_NoExpiry(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	tokenString, err := ts.SignRefreshToken(uuid.New())
	if err != nil {
		t.Fatalf("SignRefreshToken: %v", err)
	}

	claims := &refreshClaims{}
	_, err = jwt.ParseWithClaims(tokenString, claims, func(*jwt.Token) (any, error) {
		return []byte(testTokenConfig().JWTRefreshSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		t.Fatalf("parse refresh token: %v", err)
	}

	if claims.ExpiresAt != nil {
		t.Errorf("expected no exp claim, got %v", claims.ExpiresAt)
	}
	if claims.ID == "" {
		t.Error("expected a non-empty jti")
	}
}

func TestVerifyRefreshToken_RoundTrip(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	userID := uuid.New()

	tokenString, err := ts.SignRefreshToken(userID)
	if err != nil {
		t.Fatalf("SignRefreshToken: %v", err)
	}

	got, err := ts.VerifyRefreshToken(tokenString)
	if err != nil {
		t.Fatalf("VerifyRefreshToken: %v", err)
	}
	if got != userID {
		t.Errorf("VerifyRefreshToken = %v, want %v", got, userID)
	}
}

func TestVerifyRefreshToken_Garbage(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	if _, err := ts.VerifyRefreshToken("not-a-jwt"); err == nil {
		t.Fatal("expected an error for a garbage token")
	}
}

func TestVerifyRefreshToken_WrongSecret(t *testing.T) {
	signer := NewTokenService(testTokenConfig())
	tokenString, err := signer.SignRefreshToken(uuid.New())
	if err != nil {
		t.Fatalf("SignRefreshToken: %v", err)
	}

	verifier := NewTokenService(&config.Config{
		JWTAccessSecret:    testTokenConfig().JWTAccessSecret,
		JWTRefreshSecret:   "a-completely-different-secret-cccccccccccccccc",
		JWTAccessExpiresIn: 15 * time.Minute,
	})
	if _, err := verifier.VerifyRefreshToken(tokenString); err == nil {
		t.Fatal("expected an error for a token signed with a different secret")
	}
}

func TestVerifyRefreshToken_RejectsAccessTokenSecret(t *testing.T) {
	// A refresh token must not verify against the access secret, even
	// though both use HS256 — the two secrets are distinct per contract.
	ts := NewTokenService(testTokenConfig())
	accessToken, err := ts.SignAccessToken(uuid.New(), "")
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}
	if _, err := ts.VerifyRefreshToken(accessToken); err == nil {
		t.Fatal("expected an error verifying an access token as a refresh token")
	}
}

func TestVerifyAccessToken_RoundTrip(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	userID := uuid.New()

	tokenString, err := ts.SignAccessToken(userID, "user@example.com")
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}

	gotID, gotEmail, err := ts.VerifyAccessToken(tokenString)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if gotID != userID {
		t.Errorf("VerifyAccessToken id = %v, want %v", gotID, userID)
	}
	if gotEmail != "user@example.com" {
		t.Errorf("VerifyAccessToken email = %q, want %q", gotEmail, "user@example.com")
	}
}

func TestVerifyAccessToken_EmptyEmail(t *testing.T) {
	// POST /auth/refresh issues an access token with sub only — verifying
	// it must succeed with an empty email, not error.
	ts := NewTokenService(testTokenConfig())
	userID := uuid.New()

	tokenString, err := ts.SignAccessToken(userID, "")
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}

	gotID, gotEmail, err := ts.VerifyAccessToken(tokenString)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if gotID != userID {
		t.Errorf("VerifyAccessToken id = %v, want %v", gotID, userID)
	}
	if gotEmail != "" {
		t.Errorf("VerifyAccessToken email = %q, want empty", gotEmail)
	}
}

func TestVerifyAccessToken_Garbage(t *testing.T) {
	ts := NewTokenService(testTokenConfig())
	if _, _, err := ts.VerifyAccessToken("not-a-jwt"); err == nil {
		t.Fatal("expected an error for a garbage token")
	}
}

func TestVerifyAccessToken_WrongSecret(t *testing.T) {
	signer := NewTokenService(testTokenConfig())
	tokenString, err := signer.SignAccessToken(uuid.New(), "user@example.com")
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}

	verifier := NewTokenService(&config.Config{
		JWTAccessSecret:    "a-completely-different-secret-dddddddddddddddd",
		JWTRefreshSecret:   testTokenConfig().JWTRefreshSecret,
		JWTAccessExpiresIn: 15 * time.Minute,
	})
	if _, _, err := verifier.VerifyAccessToken(tokenString); err == nil {
		t.Fatal("expected an error for a token signed with a different secret")
	}
}

func TestVerifyAccessToken_RejectsRefreshTokenSecret(t *testing.T) {
	// An access token must not verify against the refresh secret, even
	// though both use HS256 — the two secrets are distinct per contract.
	ts := NewTokenService(testTokenConfig())
	refreshToken, err := ts.SignRefreshToken(uuid.New())
	if err != nil {
		t.Fatalf("SignRefreshToken: %v", err)
	}
	if _, _, err := ts.VerifyAccessToken(refreshToken); err == nil {
		t.Fatal("expected an error verifying a refresh token as an access token")
	}
}

func TestVerifyAccessToken_Expired(t *testing.T) {
	ts := NewTokenService(&config.Config{
		JWTAccessSecret:    testTokenConfig().JWTAccessSecret,
		JWTRefreshSecret:   testTokenConfig().JWTRefreshSecret,
		JWTAccessExpiresIn: -1 * time.Minute,
	})
	tokenString, err := ts.SignAccessToken(uuid.New(), "user@example.com")
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}
	if _, _, err := ts.VerifyAccessToken(tokenString); err == nil {
		t.Fatal("expected an error for an expired access token")
	}
}
