package server_test

import (
	"context"
	"net/http"
	"regexp"
	"testing"

	"github.com/sapanjai/backend/internal/infra/database"
)

// verifyURLPattern extracts the verification link's token query param from
// a rendered email body (either part — both contain the same URL).
var verifyURLPattern = regexp.MustCompile(`token=([^"&\s]+)`)

// authHeader builds an Authorization header map for doJSON.
func authHeader(accessToken string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + accessToken}
}

// extractVerifyToken reads the most recently enqueued outbox row addressed
// to email and pulls the raw verification token out of its body — standing
// in for "read the link out of the worker log" in a test that never runs
// the dispatch job. Only the API enqueues in these tests (no worker), so
// the row is still 'pending' with its body intact.
func extractVerifyToken(t *testing.T, store *database.Store, email string) string {
	t.Helper()

	var bodyText, bodyHTML *string
	err := store.Pool.QueryRow(context.Background(),
		`SELECT body_text, body_html FROM email_outbox WHERE to_address = $1 ORDER BY created_at DESC LIMIT 1`,
		email,
	).Scan(&bodyText, &bodyHTML)
	if err != nil {
		t.Fatalf("query email_outbox: %v", err)
	}
	haystack := ""
	if bodyText != nil {
		haystack = *bodyText
	} else if bodyHTML != nil {
		haystack = *bodyHTML
	}
	m := verifyURLPattern.FindStringSubmatch(haystack)
	if m == nil {
		t.Fatalf("no token found in outbox body: %q", haystack)
	}
	return m[1]
}

func TestIntegration_VerifyEmail(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	t.Run("register enqueues exactly one outbox row", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "verify-enqueue")

		var n int
		if err := store.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM email_outbox WHERE to_address = $1`, user.Email).Scan(&n); err != nil {
			t.Fatalf("count outbox rows: %v", err)
		}
		if n != 1 {
			t.Fatalf("outbox rows for %s = %d, want 1", user.Email, n)
		}
	})

	t.Run("happy path: verify then GET /auth/me reflects isVerified", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "verify-happy")
		token := extractVerifyToken(t, store, user.Email)

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/verify-email",
			map[string]any{"token": token}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("verify-email status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["success"] != true {
			t.Fatalf("body = %v, want success:true", body)
		}

		resp, me := doJSON(t, client, ts.URL, http.MethodGet, "/auth/me", nil, authHeader(user.AccessToken))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("me status = %d, want 200; body = %v", resp.StatusCode, me)
		}
		if me["isVerified"] != true {
			t.Fatalf("isVerified = %v, want true", me["isVerified"])
		}
		if me["email"] != user.Email {
			t.Fatalf("email = %v, want %q", me["email"], user.Email)
		}
	})

	t.Run("replayed token is rejected", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "verify-replay")
		token := extractVerifyToken(t, store, user.Email)

		resp, _ := doJSON(t, client, ts.URL, http.MethodPost, "/auth/verify-email",
			map[string]any{"token": token}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first verify status = %d, want 200", resp.StatusCode)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/verify-email",
			map[string]any{"token": token}, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("replay status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Invalid or expired verification token" {
			t.Fatalf("message = %v", body["message"])
		}
	})

	t.Run("unknown token", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/verify-email",
			map[string]any{"token": "not-a-real-token"}, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Invalid or expired verification token" {
			t.Fatalf("message = %v", body["message"])
		}
	})

	t.Run("validation failure: missing token", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/verify-email",
			map[string]any{}, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
	})
}

func TestIntegration_ResendVerification(t *testing.T) {
	ts, _, store := setupTestServer(t)
	client := ts.Client()

	t.Run("requires auth", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/resend-verification", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("happy path enqueues a second outbox row", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "resend-happy")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/resend-verification", nil, authHeader(user.AccessToken))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["success"] != true {
			t.Fatalf("body = %v, want success:true", body)
		}

		var n int
		if err := store.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM email_outbox WHERE to_address = $1`, user.Email).Scan(&n); err != nil {
			t.Fatalf("count outbox rows: %v", err)
		}
		if n != 2 {
			t.Fatalf("outbox rows for %s = %d, want 2 (register + resend)", user.Email, n)
		}
	})

	t.Run("cooldown active on a second immediate resend", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "resend-cooldown")

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/resend-verification", nil, authHeader(user.AccessToken))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first resend status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		resp, body = doJSON(t, client, ts.URL, http.MethodPost, "/auth/resend-verification", nil, authHeader(user.AccessToken))
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second resend status = %d, want 429; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Verification email already sent, try again in a few minutes" {
			t.Fatalf("message = %v", body["message"])
		}
	})

	t.Run("already verified", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "resend-verified")
		token := extractVerifyToken(t, store, user.Email)

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/verify-email", map[string]any{"token": token}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("verify status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		resp, body = doJSON(t, client, ts.URL, http.MethodPost, "/auth/resend-verification", nil, authHeader(user.AccessToken))
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Email already verified" {
			t.Fatalf("message = %v", body["message"])
		}
	})
}

func TestIntegration_Me(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("requires auth", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/auth/me", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("returns the caller's own profile, unverified right after register", func(t *testing.T) {
		user := registerUser(t, client, ts.URL, "me-happy")

		resp, body := doJSON(t, client, ts.URL, http.MethodGet, "/auth/me", nil, authHeader(user.AccessToken))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if body["email"] != user.Email {
			t.Fatalf("email = %v, want %q", body["email"], user.Email)
		}
		if body["isVerified"] != false {
			t.Fatalf("isVerified = %v, want false", body["isVerified"])
		}
		if _, ok := body["id"].(string); !ok {
			t.Fatalf("missing id: %v", body)
		}
		if _, ok := body["createdAt"].(string); !ok {
			t.Fatalf("missing createdAt: %v", body)
		}
	})
}
