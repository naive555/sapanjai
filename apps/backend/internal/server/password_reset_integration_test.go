package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/shared/email"
)

// countOutboxRowsBySubject counts email_outbox rows addressed to `to` with
// the password-reset subject specifically — register() also enqueues a
// verification mail to the same address, so a plain to_address count would
// overcount.
func countOutboxRowsBySubject(t *testing.T, store *database.Store, to string) int {
	t.Helper()

	var n int
	if err := store.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM email_outbox WHERE to_address = $1 AND subject = $2`, to, email.SubjectPasswordReset,
	).Scan(&n); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return n
}

func TestIntegration_ForgotPassword(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	t.Run("unknown email: 200, zero outbox rows", func(t *testing.T) {
		addr := uniqueEmail("forgot-unknown")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/forgot-password",
			map[string]any{"email": addr}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["success"] != true {
			t.Fatalf("body = %v, want success:true", body)
		}

		if n := countOutboxRowsBySubject(t, store, addr); n != 0 {
			t.Fatalf("outbox rows for unknown email = %d, want 0", n)
		}
	})

	t.Run("known email: 200, exactly one reset outbox row", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "forgot-known")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/forgot-password",
			map[string]any{"email": user.Email}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["success"] != true {
			t.Fatalf("body = %v, want success:true", body)
		}

		if n := countOutboxRowsBySubject(t, store, user.Email); n != 1 {
			t.Fatalf("password-reset outbox rows for %s = %d, want 1", user.Email, n)
		}
	})

	t.Run("second request inside the cooldown: 200, still one outbox row", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "forgot-cooldown")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/forgot-password",
			map[string]any{"email": user.Email}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first request: status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/forgot-password",
			map[string]any{"email": user.Email}, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("second request: status = %d, want 200 (the cooldown must not be observable); body = %v", resp2.StatusCode, body2)
		}
		if body2["success"] != true {
			t.Fatalf("second request body = %v, want success:true", body2)
		}

		if n := countOutboxRowsBySubject(t, store, user.Email); n != 1 {
			t.Fatalf("password-reset outbox rows for %s = %d, want 1 (cooldown must have blocked the second send)", user.Email, n)
		}
	})

	t.Run("validation failure: malformed email", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/forgot-password",
			map[string]any{"email": "not-an-email"}, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
	})
}

func TestIntegration_ResetPassword(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	t.Run("bad token", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/reset-password",
			map[string]any{"token": "not-a-real-token", "password": "brand-new-password"}, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Invalid or expired password reset token" {
			t.Fatalf("message = %v", body["message"])
		}
	})

	t.Run("replayed token is rejected", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "reset-replay")
		doJSON(t, client, ts.URL, http.MethodPost, "/auth/forgot-password",
			map[string]any{"email": user.Email}, nil)
		token := extractVerifyToken(t, store, user.Email)

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/reset-password",
			map[string]any{"token": token, "password": "brand-new-password"}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first reset: status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/reset-password",
			map[string]any{"token": token, "password": "another-password"}, nil)
		if resp2.StatusCode != http.StatusBadRequest {
			t.Fatalf("replay: status = %d, want 400; body = %v", resp2.StatusCode, body2)
		}
		if body2["message"] != "Invalid or expired password reset token" {
			t.Fatalf("replay message = %v", body2["message"])
		}
	})

	t.Run("validation failure: password shorter than 8", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/reset-password",
			map[string]any{"token": "whatever", "password": "short"}, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Validation failed" {
			t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
		}
	})

	t.Run("happy path: password changed, all sessions revoked, is_verified true", func(t *testing.T) {
		addr := uniqueEmail("reset-happy")
		_, regBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": addr, "password": "original-password"}, nil)
		oldRefresh, _ := regBody["refreshToken"].(string)
		if oldRefresh == "" {
			t.Fatalf("setup: register failed: %v", regBody)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/forgot-password",
			map[string]any{"email": addr}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("forgot-password: status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		token := extractVerifyToken(t, store, addr)

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/reset-password",
			map[string]any{"token": token, "password": "brand-new-password"}, nil)
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("reset-password: status = %d, want 200; body = %v", resp2.StatusCode, body2)
		}
		if body2["success"] != true {
			t.Fatalf("reset-password body = %v, want success:true", body2)
		}

		// Old refresh token must no longer work: ResetPassword revokes
		// every session for the user.
		resp3, body3 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": oldRefresh}, nil)
		if resp3.StatusCode != http.StatusUnauthorized {
			t.Fatalf("refresh with pre-reset token: status = %d, want 401; body = %v", resp3.StatusCode, body3)
		}

		// The old password must no longer authenticate.
		resp4, body4 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
			map[string]any{"email": addr, "password": "original-password"}, nil)
		if resp4.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login with old password: status = %d, want 401; body = %v", resp4.StatusCode, body4)
		}

		// The new password must authenticate, and the account must now
		// read as verified (reaching the reset link proves mailbox
		// control).
		resp5, body5 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
			map[string]any{"email": addr, "password": "brand-new-password"}, nil)
		if resp5.StatusCode != http.StatusOK {
			t.Fatalf("login with new password: status = %d, want 200; body = %v", resp5.StatusCode, body5)
		}
		newAccessToken, _ := body5["accessToken"].(string)
		if newAccessToken == "" {
			t.Fatalf("login with new password: missing accessToken: %v", body5)
		}

		resp6, body6 := doJSON(t, client, ts.URL, http.MethodGet, "/auth/me", nil, authHeader(newAccessToken))
		if resp6.StatusCode != http.StatusOK {
			t.Fatalf("me: status = %d, want 200; body = %v", resp6.StatusCode, body6)
		}
		if body6["isVerified"] != true {
			t.Fatalf("isVerified = %v, want true", body6["isVerified"])
		}
	})
}
