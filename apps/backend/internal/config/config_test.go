package config

import (
	"strings"
	"testing"
	"time"
)

// setBaselineEnv sets every variable Load requires, so each test can vary the
// one thing it is about. t.Setenv restores the previous value on cleanup and
// forbids t.Parallel, which is what we want here — Load reads process-global
// state.
func setBaselineEnv(t *testing.T) {
	t.Helper()

	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/sapanjai?sslmode=disable")
	t.Setenv("REDIS_URL", "redis://localhost:6379")
	t.Setenv("JWT_ACCESS_SECRET", strings.Repeat("a", minSecretLen))
	t.Setenv("JWT_REFRESH_SECRET", strings.Repeat("b", minSecretLen))
	t.Setenv("CONNECTOR_MASTER_KEY", "JQ5uRM447OIEr/ty3J/+KKbXqDHQiKZnwdWcLKlOh/E=")
	t.Setenv("CONNECTOR_MASTER_KEY_PREVIOUS", "")

	// Clear every optional var so a value exported in the developer's real
	// shell cannot make a defaults test pass — or fail — for the wrong
	// reason. Someone with PORT=8080 in their environment would otherwise see
	// TestLoad_ExistingDefaultsUnchanged fail on a clean tree.
	for _, k := range []string{
		"APP_NAME", "APP_ENV", "PORT", "LOG_LEVEL", "WORKER_PORT",
		"JWT_ACCESS_EXPIRES_IN", "JWT_REFRESH_EXPIRES_IN",
		"WORKER_JOB_TIMEOUT", "MCP_RATE_LIMIT_PER_MIN",
		"SESSION_CLEANUP_INTERVAL", "SESSION_CLEANUP_RETENTION", "SESSION_CLEANUP_BATCH_SIZE",
		"RESEND_API_KEY", "EMAIL_FROM", "APP_PUBLIC_URL",
		"EMAIL_DISPATCH_INTERVAL", "EMAIL_DISPATCH_BATCH_SIZE",
		"EMAIL_MAX_ATTEMPTS", "EMAIL_OUTBOX_RETENTION",
	} {
		t.Setenv(k, "")
	}
}

func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// A deployment that never sets a single email variable must still boot, with
// working defaults. This is what keeps the feature additive for the existing
// Railway services.
func TestLoad_EmailDefaults(t *testing.T) {
	setBaselineEnv(t)

	cfg := mustLoad(t)

	if cfg.ResendAPIKey != "" {
		t.Errorf("ResendAPIKey = %q, want empty by default", cfg.ResendAPIKey)
	}
	if cfg.EmailFrom == "" {
		t.Error("EmailFrom has no default; want a usable placeholder sender")
	}
	if cfg.AppPublicURL != "http://localhost:4000" {
		t.Errorf("AppPublicURL = %q, want http://localhost:4000 (the make web port)", cfg.AppPublicURL)
	}
	if cfg.EmailDispatchInterval != 15*time.Second {
		t.Errorf("EmailDispatchInterval = %v, want 15s", cfg.EmailDispatchInterval)
	}
	if cfg.EmailDispatchBatchSize != 20 {
		t.Errorf("EmailDispatchBatchSize = %d, want 20", cfg.EmailDispatchBatchSize)
	}
	if cfg.EmailMaxAttempts != 5 {
		t.Errorf("EmailMaxAttempts = %d, want 5", cfg.EmailMaxAttempts)
	}
	if cfg.EmailOutboxRetention != 168*time.Hour {
		t.Errorf("EmailOutboxRetention = %v, want 168h", cfg.EmailOutboxRetention)
	}
}

func TestLoad_EmailOverrides(t *testing.T) {
	setBaselineEnv(t)
	t.Setenv("RESEND_API_KEY", "re_live_abc123")
	t.Setenv("EMAIL_FROM", "Sapanjai <noreply@sapanjai.io>")
	t.Setenv("APP_PUBLIC_URL", "https://app.sapanjai.io")
	t.Setenv("EMAIL_DISPATCH_INTERVAL", "30s")
	t.Setenv("EMAIL_DISPATCH_BATCH_SIZE", "50")
	t.Setenv("EMAIL_MAX_ATTEMPTS", "3")
	t.Setenv("EMAIL_OUTBOX_RETENTION", "72h")

	cfg := mustLoad(t)

	if cfg.ResendAPIKey != "re_live_abc123" {
		t.Errorf("ResendAPIKey = %q", cfg.ResendAPIKey)
	}
	if cfg.EmailFrom != "Sapanjai <noreply@sapanjai.io>" {
		t.Errorf("EmailFrom = %q", cfg.EmailFrom)
	}
	if cfg.AppPublicURL != "https://app.sapanjai.io" {
		t.Errorf("AppPublicURL = %q", cfg.AppPublicURL)
	}
	if cfg.EmailDispatchInterval != 30*time.Second {
		t.Errorf("EmailDispatchInterval = %v", cfg.EmailDispatchInterval)
	}
	if cfg.EmailDispatchBatchSize != 50 {
		t.Errorf("EmailDispatchBatchSize = %d", cfg.EmailDispatchBatchSize)
	}
	if cfg.EmailMaxAttempts != 3 {
		t.Errorf("EmailMaxAttempts = %d", cfg.EmailMaxAttempts)
	}
	if cfg.EmailOutboxRetention != 72*time.Hour {
		t.Errorf("EmailOutboxRetention = %v", cfg.EmailOutboxRetention)
	}
}

func TestConfig_EmailEnabled(t *testing.T) {
	setBaselineEnv(t)
	if mustLoad(t).EmailEnabled() {
		t.Error("EmailEnabled() = true with no RESEND_API_KEY, want false")
	}

	t.Setenv("RESEND_API_KEY", "re_live_abc123")
	if !mustLoad(t).EmailEnabled() {
		t.Error("EmailEnabled() = false with RESEND_API_KEY set, want true")
	}
}

// Links are built as AppPublicURL + "/verify-email?token=...". An operator who
// pastes a URL with a trailing slash would otherwise get "//verify-email",
// which some routers 404 and some redirect — either way a broken link in an
// email nobody can un-send.
func TestLoad_AppPublicURLDropsTrailingSlash(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://app.sapanjai.io/", "https://app.sapanjai.io"},
		{"https://app.sapanjai.io///", "https://app.sapanjai.io"},
		{"https://app.sapanjai.io", "https://app.sapanjai.io"},
		{"http://localhost:4000/", "http://localhost:4000"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			setBaselineEnv(t)
			t.Setenv("APP_PUBLIC_URL", tc.in)

			if got := mustLoad(t).AppPublicURL; got != tc.want {
				t.Errorf("AppPublicURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad_RejectsInvalidEmailDurations(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"EMAIL_DISPATCH_INTERVAL", "not-a-duration"},
		{"EMAIL_DISPATCH_INTERVAL", "0s"},
		{"EMAIL_DISPATCH_INTERVAL", "-5s"},
		{"EMAIL_OUTBOX_RETENTION", "banana"},
		{"EMAIL_OUTBOX_RETENTION", "0h"},
		{"EMAIL_OUTBOX_RETENTION", "-1h"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			setBaselineEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name %s, so the operator cannot fix it: %v", tc.key, err)
			}
		})
	}
}

func TestLoad_RejectsOutOfRangeEmailIntegers(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"EMAIL_DISPATCH_BATCH_SIZE", "not-a-number"},
		{"EMAIL_DISPATCH_BATCH_SIZE", "0"},
		{"EMAIL_DISPATCH_BATCH_SIZE", "-1"},
		{"EMAIL_DISPATCH_BATCH_SIZE", "501"},
		{"EMAIL_MAX_ATTEMPTS", "not-a-number"},
		{"EMAIL_MAX_ATTEMPTS", "0"},
		{"EMAIL_MAX_ATTEMPTS", "-3"},
		{"EMAIL_MAX_ATTEMPTS", "21"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			setBaselineEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name %s: %v", tc.key, err)
			}
		})
	}
}

func TestLoad_AcceptsEmailIntegerBounds(t *testing.T) {
	setBaselineEnv(t)
	t.Setenv("EMAIL_DISPATCH_BATCH_SIZE", "1")
	t.Setenv("EMAIL_MAX_ATTEMPTS", "1")
	if cfg := mustLoad(t); cfg.EmailDispatchBatchSize != 1 || cfg.EmailMaxAttempts != 1 {
		t.Errorf("lower bounds rejected: batch=%d attempts=%d", cfg.EmailDispatchBatchSize, cfg.EmailMaxAttempts)
	}

	t.Setenv("EMAIL_DISPATCH_BATCH_SIZE", "500")
	t.Setenv("EMAIL_MAX_ATTEMPTS", "20")
	if cfg := mustLoad(t); cfg.EmailDispatchBatchSize != 500 || cfg.EmailMaxAttempts != 20 {
		t.Errorf("upper bounds rejected: batch=%d attempts=%d", cfg.EmailDispatchBatchSize, cfg.EmailMaxAttempts)
	}
}

// Load's contract is to report every problem at once so an operator fixes
// them in one pass. The new email vars must join that aggregate rather than
// short-circuiting it.
func TestLoad_AggregatesEmailProblemsWithTheRest(t *testing.T) {
	setBaselineEnv(t)
	t.Setenv("JWT_ACCESS_SECRET", "too-short")
	t.Setenv("EMAIL_MAX_ATTEMPTS", "0")
	t.Setenv("EMAIL_DISPATCH_INTERVAL", "nope")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an invalid configuration")
	}
	for _, want := range []string{"JWT_ACCESS_SECRET", "EMAIL_MAX_ATTEMPTS", "EMAIL_DISPATCH_INTERVAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregate error omits %s: %v", want, err)
		}
	}
}

// Guards the existing behaviour this change must not break.
func TestLoad_ExistingDefaultsUnchanged(t *testing.T) {
	setBaselineEnv(t)

	cfg := mustLoad(t)

	if cfg.Port != "3000" {
		t.Errorf("Port = %q, want 3000", cfg.Port)
	}
	if cfg.WorkerPort != "3001" {
		t.Errorf("WorkerPort = %q, want 3001", cfg.WorkerPort)
	}
	if cfg.JWTAccessExpiresIn != 15*time.Minute {
		t.Errorf("JWTAccessExpiresIn = %v, want 15m", cfg.JWTAccessExpiresIn)
	}
	if cfg.JWTRefreshExpiresIn != 604800*time.Second {
		t.Errorf("JWTRefreshExpiresIn = %v, want 604800s", cfg.JWTRefreshExpiresIn)
	}
	if cfg.MCPRateLimitPerMin != 60 {
		t.Errorf("MCPRateLimitPerMin = %d, want 60", cfg.MCPRateLimitPerMin)
	}
	if cfg.SessionCleanupBatchSize != 1000 {
		t.Errorf("SessionCleanupBatchSize = %d, want 1000", cfg.SessionCleanupBatchSize)
	}
}
