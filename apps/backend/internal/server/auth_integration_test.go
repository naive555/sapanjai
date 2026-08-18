package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sapanjai/backend/internal/config"
	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	appredis "github.com/sapanjai/backend/internal/infra/redis"
	"github.com/sapanjai/backend/internal/module/auth"
	"github.com/sapanjai/backend/internal/server"
	"github.com/sapanjai/backend/internal/shared/envelope"
	applogger "github.com/sapanjai/backend/internal/shared/logger"
	"github.com/sapanjai/backend/migrations"
)

// setupTestServer skips unless DATABASE_URL and REDIS_URL are set, runs
// migrations against DATABASE_URL, boots the real server.New(...), and
// returns an httptest.Server plus the pieces subtests need for direct DB/
// Redis assertions. Each test uses unique emails (uuid-suffixed) rather
// than flushing Redis wholesale, so cases stay isolated without touching
// data outside their own keys.
//
// configure is optional and applied to the *config.Config after its
// defaults are set but before server.New builds against it — e.g. the MCP
// rate-limit test lowers MCPRateLimitPerMin so it can trip the bucket in a
// handful of calls instead of issuing 61 real ones. Existing callers that
// pass none get the same defaults as before this parameter existed.
func setupTestServer(t *testing.T, configure ...func(*config.Config)) (*httptest.Server, *config.Config, *database.Store) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	redisURL := os.Getenv("REDIS_URL")
	if databaseURL == "" || redisURL == "" {
		t.Skip("DATABASE_URL/REDIS_URL not set; skipping integration test")
	}

	ctx := context.Background()

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open database/sql: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	cfg := &config.Config{
		AppName:             "sapanjai-api-test",
		AppEnv:              "test",
		Port:                "0",
		LogLevel:            "error",
		DatabaseURL:         databaseURL,
		RedisURL:            redisURL,
		JWTAccessSecret:     "integration-access-secret-aaaaaaaaaaaaaaaaaaa",
		JWTRefreshSecret:    "integration-refresh-secret-bbbbbbbbbbbbbbbbbb",
		JWTAccessExpiresIn:  15 * time.Minute,
		JWTRefreshExpiresIn: 7 * 24 * time.Hour,
		ConnectorMasterKey:  bytes.Repeat([]byte{7}, envelope.MasterKeyLen), // fixed test key
		MCPRateLimitPerMin:  60,                                             // matches config.Load's own default
	}
	for _, mutate := range configure {
		mutate(cfg)
	}

	pool, err := database.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(pool.Close)
	store := database.NewStore(pool)

	rdb, err := appredis.New(ctx, redisURL)
	if err != nil {
		t.Fatalf("appredis.New: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	log := applogger.New(cfg.AppEnv, cfg.LogLevel)

	e, err := server.New(cfg, log, pool, rdb)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(e)
	t.Cleanup(ts.Close)

	return ts, cfg, store
}

func uniqueEmail(prefix string) string {
	return prefix + "-" + uuid.NewString() + "@example.com"
}

// doJSON issues an HTTP request with a JSON (or raw string) body and
// decodes the JSON response, failing the test on any transport error.
func doJSON(t *testing.T, client *http.Client, baseURL, method, path string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()

	var buf *bytes.Buffer
	if s, ok := body.(string); ok {
		buf = bytes.NewBufferString(s)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewBuffer(b)
	}

	req, err := http.NewRequest(method, baseURL+path, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, decoded
}

func TestIntegration_Register(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	t.Run("happy path", func(t *testing.T) {
		email := uniqueEmail("register-happy")
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": "password123", "displayName": "Ann"}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		if _, ok := body["accessToken"].(string); !ok || body["accessToken"] == "" {
			t.Fatalf("missing/empty accessToken: %v", body)
		}
		if _, ok := body["refreshToken"].(string); !ok || body["refreshToken"] == "" {
			t.Fatalf("missing/empty refreshToken: %v", body)
		}
	})

	t.Run("duplicate email", func(t *testing.T) {
		email := uniqueEmail("register-dup")
		doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": "password123"}, nil)

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": "password123"}, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Email already taken" {
			t.Fatalf("message = %v, want %q", body["message"], "Email already taken")
		}
	})

	t.Run("validation failure", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": "not-an-email", "password": "short"}, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Validation failed" {
			t.Fatalf("message = %v, want %q", body["message"], "Validation failed")
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register", `{"email":`, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Invalid request body" {
			t.Fatalf("message = %v, want %q", body["message"], "Invalid request body")
		}
	})
}

func TestIntegration_Login(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()
	password := "password123"

	t.Run("happy path", func(t *testing.T) {
		email := uniqueEmail("login-happy")
		doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": password}, nil)

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
			map[string]any{"email": email, "password": password}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
	})

	t.Run("wrong password five times then rate limited", func(t *testing.T) {
		email := uniqueEmail("login-ratelimit")
		doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": password}, nil)

		for i := 1; i <= 5; i++ {
			resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
				map[string]any{"email": email, "password": "wrong-password"}, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("attempt %d: status = %d, want 401; body = %v", i, resp.StatusCode, body)
			}
			if body["message"] != "Invalid email or password" {
				t.Fatalf("attempt %d: message = %v", i, body["message"])
			}
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
			map[string]any{"email": email, "password": "wrong-password"}, nil)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("6th attempt: status = %d, want 429; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Too many login attempts, try again in 15 minutes" {
			t.Fatalf("6th attempt: message = %v", body["message"])
		}
	})

	t.Run("successful login resets the rate-limit counter", func(t *testing.T) {
		email := uniqueEmail("login-reset")
		doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": password}, nil)

		for range 3 {
			doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
				map[string]any{"email": email, "password": "wrong-password"}, nil)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
			map[string]any{"email": email, "password": password}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("successful login: status = %d, want 200; body = %v", resp.StatusCode, body)
		}

		// post-reset: 5 more wrong attempts should be allowed before hitting the limit again.
		for i := 1; i <= 5; i++ {
			resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/login",
				map[string]any{"email": email, "password": "wrong-password"}, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("post-reset attempt %d: status = %d, want 401; body = %v", i, resp.StatusCode, body)
			}
		}
	})
}

func TestIntegration_Refresh(t *testing.T) {
	ts, cfg, store := setupTestServer(t)
	client := ts.Client()

	t.Run("rotation issues a distinct token and revokes the old one", func(t *testing.T) {
		email := uniqueEmail("refresh-rotate")
		_, regBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": "password123"}, nil)
		oldRefresh, _ := regBody["refreshToken"].(string)
		if oldRefresh == "" {
			t.Fatalf("setup: register failed: %v", regBody)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": oldRefresh}, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, body)
		}
		newRefresh, _ := body["refreshToken"].(string)
		if newRefresh == "" || newRefresh == oldRefresh {
			t.Fatalf("expected a new, different refresh token; got %q (old %q)", newRefresh, oldRefresh)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": oldRefresh}, nil)
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("reuse of old token: status = %d, want 401; body = %v", resp2.StatusCode, body2)
		}
		if body2["message"] != "Refresh token reuse detected" {
			t.Fatalf("reuse of old token: message = %v", body2["message"])
		}
	})

	t.Run("reuse revokes the whole family", func(t *testing.T) {
		email := uniqueEmail("refresh-family")
		_, regBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": "password123"}, nil)
		rt1, _ := regBody["refreshToken"].(string)
		if rt1 == "" {
			t.Fatalf("setup: register failed: %v", regBody)
		}

		_, rotBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": rt1}, nil)
		rt2, _ := rotBody["refreshToken"].(string)
		if rt2 == "" {
			t.Fatalf("setup: rotation failed: %v", rotBody)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": rt1}, nil)
		if resp.StatusCode != http.StatusUnauthorized || body["message"] != "Refresh token reuse detected" {
			t.Fatalf("reuse rt1: status = %d, body = %v", resp.StatusCode, body)
		}

		// rt2 is rt1's family-mate: the reuse above must have revoked it too.
		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": rt2}, nil)
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("family-mate rt2: status = %d, want 401; body = %v", resp2.StatusCode, body2)
		}
	})

	t.Run("garbage token", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": "garbage"}, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Invalid refresh token" {
			t.Fatalf("message = %v, want %q", body["message"], "Invalid refresh token")
		}
	})

	t.Run("expired session", func(t *testing.T) {
		ctx := context.Background()

		user, err := store.CreateUser(ctx, db.CreateUserParams{
			Email:        uniqueEmail("refresh-expired"),
			PasswordHash: "bcrypt-hash-placeholder",
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		// Sign with the same secret the test server uses, so
		// VerifyRefreshToken succeeds and the request actually reaches
		// RotateSession's expiry check (a garbage string would instead
		// fail JWT verification and short-circuit to "Invalid refresh
		// token" without ever consulting the session row).
		tokenSvc := auth.NewTokenService(cfg)
		expiredRefresh, err := tokenSvc.SignRefreshToken(user.ID)
		if err != nil {
			t.Fatalf("SignRefreshToken: %v", err)
		}

		// UTC: sessions.expires_at is "timestamp without time zone", and
		// RotateSession compares it against time.Now().UTC() (see the
		// comment in service.go) — the fixture must use the same UTC
		// arithmetic to actually land in the past from the app's
		// perspective, not just the host's local zone.
		if _, err := store.CreateSession(ctx, db.CreateSessionParams{
			UserID:       user.ID,
			RefreshToken: expiredRefresh,
			Family:       uuid.New(),
			ExpiresAt:    time.Now().UTC().Add(-1 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": expiredRefresh}, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, body)
		}
		if body["message"] != "Refresh token expired" {
			t.Fatalf("message = %v, want %q", body["message"], "Refresh token expired")
		}
	})
}

func TestIntegration_Logout(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	rdb, err := appredis.New(context.Background(), os.Getenv("REDIS_URL"))
	if err != nil {
		t.Fatalf("appredis.New: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	t.Run("blacklists the access token and revokes all sessions", func(t *testing.T) {
		email := uniqueEmail("logout")
		_, regBody := doJSON(t, client, ts.URL, http.MethodPost, "/auth/register",
			map[string]any{"email": email, "password": "password123"}, nil)
		accessToken, _ := regBody["accessToken"].(string)
		refreshToken, _ := regBody["refreshToken"].(string)
		if accessToken == "" || refreshToken == "" {
			t.Fatalf("setup: register failed: %v", regBody)
		}

		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/logout",
			map[string]any{"refreshToken": refreshToken},
			map[string]string{"Authorization": "Bearer " + accessToken})
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("logout: status = %d, body = %v", resp.StatusCode, body)
		}

		ttl, err := rdb.TTL(context.Background(), "blacklist:"+accessToken).Result()
		if err != nil {
			t.Fatalf("redis TTL: %v", err)
		}
		if ttl <= 0 || ttl > 15*time.Minute {
			t.Fatalf("blacklist TTL = %v, want in (0, 15m]", ttl)
		}

		resp2, body2 := doJSON(t, client, ts.URL, http.MethodPost, "/auth/refresh",
			map[string]any{"refreshToken": refreshToken}, nil)
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("post-logout refresh: status = %d, want 401; body = %v", resp2.StatusCode, body2)
		}
	})

	t.Run("unknown refresh token still succeeds", func(t *testing.T) {
		resp, body := doJSON(t, client, ts.URL, http.MethodPost, "/auth/logout",
			map[string]any{"refreshToken": "unknown-" + uuid.NewString()}, nil)
		if resp.StatusCode != http.StatusOK || body["success"] != true {
			t.Fatalf("logout unknown token: status = %d, body = %v", resp.StatusCode, body)
		}
	})
}

func TestIntegration_RequestIDEchoed(t *testing.T) {
	ts, _, _ := setupTestServer(t)
	client := ts.Client()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Request-Id", "test-request-id-123")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Request-Id"); got != "test-request-id-123" {
		t.Fatalf("X-Request-Id = %q, want %q", got, "test-request-id-123")
	}
}
