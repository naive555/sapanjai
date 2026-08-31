// Package config loads and validates process configuration from environment
// variables, mirroring the env contract documented in docs/02-api-contract.md.
//
// Unlike the source Node app (which only checked REDIS_URL at boot), Load
// fails fast on ANY missing or invalid required variable and reports all
// problems at once.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sapanjai/backend/internal/shared/envelope"
)

const minSecretLen = 32

// defaultRedisKeyPrefix namespaces this application's keys so a Redis
// instance shared with a sibling project cannot collide with them. See
// Config.RedisKeyPrefix.
const defaultRedisKeyPrefix = "sapanjai:"

// maxCleanupBatchSize bounds SESSION_CLEANUP_BATCH_SIZE so a misconfigured
// deployment can't ask the cleanup job to delete unbounded rows in a single
// statement.
const maxCleanupBatchSize = 10_000

// maxEmailBatchSize and maxEmailAttempts bound EMAIL_DISPATCH_BATCH_SIZE and
// EMAIL_MAX_ATTEMPTS for the same reason maxCleanupBatchSize exists: a
// fat-fingered value should fail at boot, not at 3am against the outbox.
const (
	maxEmailBatchSize = 500
	maxEmailAttempts  = 20
)

type Config struct {
	AppName  string
	AppEnv   string
	Port     string
	LogLevel string

	DatabaseURL string
	RedisURL    string

	// RedisKeyPrefix is prepended to every Redis key this process reads or
	// writes (blacklist, login attempts, verification/reset tokens, the MCP
	// rate-limit buckets, and the worker job locks). It exists so a Redis
	// instance shared with another application cannot collide with ours:
	// several keys here ("blacklist:", "login:attempts:", "verify:resend:")
	// come from a platform-core template that sibling projects also derive
	// from, so a shared instance silently shares those counters.
	//
	// It MUST be identical on the api and worker processes — they meet on
	// "<prefix>worker:lock:<job>" and "<prefix>blacklist:<token>" — which is
	// why it is a fixed default rather than being derived from APP_NAME.
	// Changing it orphans every live key: in-flight verification and
	// password-reset links stop resolving and the blacklist forgets prior
	// logouts. Nothing needs migrating; the orphans expire on their own TTLs.
	//
	// Explicitly setting REDIS_KEY_PREFIX="" opts out and restores the
	// unprefixed keys, for a deployment that already owns its Redis.
	RedisKeyPrefix string

	JWTAccessSecret     string
	JWTRefreshSecret    string
	JWTAccessExpiresIn  time.Duration
	JWTRefreshExpiresIn time.Duration

	// ConnectorMasterKey is the decoded master key wrapping every
	// connector's data key (internal/shared/envelope). Decoded, not raw
	// base64, so a malformed value fails at boot rather than on first use.
	ConnectorMasterKey []byte

	// ConnectorMasterKeysRetired are previous master keys kept for
	// decrypt-only use, so rows sealed before a CONNECTOR_MASTER_KEY
	// rotation still open under rotate-on-read. Newest-retired first;
	// optional and nil when no rotation has happened yet.
	ConnectorMasterKeysRetired [][]byte

	WorkerPort       string
	WorkerJobTimeout time.Duration

	SessionCleanupInterval  time.Duration
	SessionCleanupRetention time.Duration
	SessionCleanupBatchSize int

	// MCPRateLimitPerMin caps upstream-Google-API requests per connector,
	// per minute (internal/infra/redis.RateLimiter, key
	// "mcp:ratelimit:<connectorId>"), enforced in internal/module/mcp
	// before a tools/call is dispatched. See
	// docs/07-sheets-adapter-decisions.md step 4.
	MCPRateLimitPerMin int

	// ResendAPIKey authenticates the transactional-mail sender. Optional and
	// empty by default: with no key the worker falls back to
	// email.LogSender, so a developer never needs a Resend account. Only the
	// worker process sends mail — the API only ever enqueues — so this needs
	// to be set on the worker service and nowhere else.
	ResendAPIKey string

	// EmailFrom is the sending identity ("Name <addr@domain>"). In production
	// its domain must be verified in Resend or every send 403s.
	EmailFrom string

	// AppPublicURL is the browser-facing frontend origin that verification
	// and password-reset links are built from. Deliberately NOT BACKEND_URL:
	// that address is dialled server-side by the Next.js proxy and is
	// frequently unreachable from a mail client (compose's "http://api:3000",
	// Railway's private domain). Stored without a trailing slash.
	AppPublicURL string

	// EmailDispatchInterval is how often the outbox drain job runs, and
	// therefore also its Redis lock TTL (internal/worker).
	EmailDispatchInterval time.Duration

	// EmailDispatchBatchSize is how many outbox rows one run claims.
	EmailDispatchBatchSize int

	// EmailMaxAttempts is how many sends a row gets before it is marked
	// 'failed' and stops being retried.
	EmailMaxAttempts int

	// EmailOutboxRetention is how long 'sent'/'failed' rows are kept before
	// the dispatch job prunes them.
	EmailOutboxRetention time.Duration

	// AdminIPAllowlist gates the /admin route group (execution plan Task
	// 6.2, docs/11-admin-panel.md) before RequireAuth runs at all — an
	// off-network request never reaches the login/2FA surface. Parsed at
	// boot, not per-request, so a malformed CIDR fails startup instead of
	// silently letting every request through (or none). Nil/empty disables
	// the check entirely, which is the required default for local dev and
	// for any deployment that hasn't set ADMIN_IP_ALLOWLIST.
	//
	// See internal/server/server.go's e.IPExtractor comment and
	// docs/09-railway-deploy.md for what c.RealIP() — the value this list
	// is matched against — actually depends on: through this app's own
	// frontend proxy (100% of the console's browser traffic), it resolves
	// to the frontend's own network address, not an individual staff
	// member's, so this allowlist is not a substitute for restricting
	// access at the platform edge if that granularity is required.
	AdminIPAllowlist []*net.IPNet

	// AdminRequire2FA gates every /admin route except
	// POST /admin/2fa/{enroll,confirm,verify} behind a confirmed
	// admin:2fa:<userId> Redis key (execution plan Task 6.3). Defaults true;
	// set false only for local development, where minting a TOTP enrollment
	// for every throwaway seeded account is friction with no security
	// benefit.
	AdminRequire2FA bool
}

// Load reads configuration from the environment, applies defaults, and
// validates required fields. It returns a single error aggregating every
// problem found so an operator can fix them all in one pass.
func Load() (*Config, error) {
	cfg := &Config{
		AppName:  getEnv("APP_NAME", "sapanjai-api"),
		AppEnv:   getEnv("APP_ENV", "development"),
		Port:     getEnv("PORT", "3000"),
		LogLevel: getEnv("LOG_LEVEL", "info"),

		DatabaseURL: os.Getenv("DATABASE_URL"),
		RedisURL:    os.Getenv("REDIS_URL"),

		RedisKeyPrefix: redisKeyPrefix(),

		JWTAccessSecret:  os.Getenv("JWT_ACCESS_SECRET"),
		JWTRefreshSecret: os.Getenv("JWT_REFRESH_SECRET"),

		WorkerPort: getEnv("WORKER_PORT", "3001"),
	}

	var problems []string

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if cfg.RedisURL == "" {
		problems = append(problems, "REDIS_URL is required")
	}

	if len(cfg.JWTAccessSecret) < minSecretLen {
		problems = append(problems, fmt.Sprintf("JWT_ACCESS_SECRET must be at least %d characters", minSecretLen))
	}
	if len(cfg.JWTRefreshSecret) < minSecretLen {
		problems = append(problems, fmt.Sprintf("JWT_REFRESH_SECRET must be at least %d characters", minSecretLen))
	}

	masterKey, err := envelope.DecodeMasterKey(os.Getenv("CONNECTOR_MASTER_KEY"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("CONNECTOR_MASTER_KEY must be a base64-encoded %d-byte key: %v", envelope.MasterKeyLen, err))
	} else {
		cfg.ConnectorMasterKey = masterKey
	}

	retiredKeys, err := envelope.DecodeMasterKeys(os.Getenv("CONNECTOR_MASTER_KEY_PREVIOUS"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("CONNECTOR_MASTER_KEY_PREVIOUS must be a comma-separated list of base64-encoded %d-byte keys: %v", envelope.MasterKeyLen, err))
	} else {
		cfg.ConnectorMasterKeysRetired = retiredKeys
	}

	accessExp, err := time.ParseDuration(getEnv("JWT_ACCESS_EXPIRES_IN", "15m"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("JWT_ACCESS_EXPIRES_IN is not a valid duration: %v", err))
	} else {
		cfg.JWTAccessExpiresIn = accessExp
	}

	refreshExpSeconds, err := strconv.Atoi(getEnv("JWT_REFRESH_EXPIRES_IN", "604800"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("JWT_REFRESH_EXPIRES_IN is not a valid integer (seconds): %v", err))
	} else {
		cfg.JWTRefreshExpiresIn = time.Duration(refreshExpSeconds) * time.Second
	}

	for _, d := range []struct {
		key      string
		fallback string
		target   *time.Duration
	}{
		{"WORKER_JOB_TIMEOUT", "5m", &cfg.WorkerJobTimeout},
		{"SESSION_CLEANUP_INTERVAL", "1h", &cfg.SessionCleanupInterval},
		{"SESSION_CLEANUP_RETENTION", "720h", &cfg.SessionCleanupRetention},
		{"EMAIL_DISPATCH_INTERVAL", "15s", &cfg.EmailDispatchInterval},
		{"EMAIL_OUTBOX_RETENTION", "168h", &cfg.EmailOutboxRetention},
	} {
		parsed, err := time.ParseDuration(getEnv(d.key, d.fallback))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s is not a valid duration: %v", d.key, err))
		case parsed <= 0:
			problems = append(problems, d.key+" must be greater than zero")
		default:
			*d.target = parsed
		}
	}

	batchSize, err := strconv.Atoi(getEnv("SESSION_CLEANUP_BATCH_SIZE", "1000"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("SESSION_CLEANUP_BATCH_SIZE is not a valid integer: %v", err))
	case batchSize < 1 || batchSize > maxCleanupBatchSize:
		problems = append(problems, fmt.Sprintf("SESSION_CLEANUP_BATCH_SIZE must be between 1 and %d", maxCleanupBatchSize))
	default:
		cfg.SessionCleanupBatchSize = batchSize
	}

	mcpRateLimit, err := strconv.Atoi(getEnv("MCP_RATE_LIMIT_PER_MIN", "60"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("MCP_RATE_LIMIT_PER_MIN is not a valid integer: %v", err))
	case mcpRateLimit < 1:
		problems = append(problems, "MCP_RATE_LIMIT_PER_MIN must be greater than zero")
	default:
		cfg.MCPRateLimitPerMin = mcpRateLimit
	}

	cfg.ResendAPIKey = os.Getenv("RESEND_API_KEY")
	cfg.EmailFrom = getEnv("EMAIL_FROM", "Sapanjai <noreply@localhost>")

	// Trailing slashes are stripped so link-building can always safely do
	// AppPublicURL + "/verify-email?token=..." without risking "//verify-email".
	cfg.AppPublicURL = strings.TrimRight(getEnv("APP_PUBLIC_URL", "http://localhost:4000"), "/")

	emailBatchSize, err := strconv.Atoi(getEnv("EMAIL_DISPATCH_BATCH_SIZE", "20"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("EMAIL_DISPATCH_BATCH_SIZE is not a valid integer: %v", err))
	case emailBatchSize < 1 || emailBatchSize > maxEmailBatchSize:
		problems = append(problems, fmt.Sprintf("EMAIL_DISPATCH_BATCH_SIZE must be between 1 and %d", maxEmailBatchSize))
	default:
		cfg.EmailDispatchBatchSize = emailBatchSize
	}

	emailMaxAttempts, err := strconv.Atoi(getEnv("EMAIL_MAX_ATTEMPTS", "5"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("EMAIL_MAX_ATTEMPTS is not a valid integer: %v", err))
	case emailMaxAttempts < 1 || emailMaxAttempts > maxEmailAttempts:
		problems = append(problems, fmt.Sprintf("EMAIL_MAX_ATTEMPTS must be between 1 and %d", maxEmailAttempts))
	default:
		cfg.EmailMaxAttempts = emailMaxAttempts
	}

	allowlist, err := parseCIDRList(os.Getenv("ADMIN_IP_ALLOWLIST"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("ADMIN_IP_ALLOWLIST is invalid: %v", err))
	} else {
		cfg.AdminIPAllowlist = allowlist
	}

	require2FA, err := strconv.ParseBool(getEnv("ADMIN_REQUIRE_2FA", "true"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("ADMIN_REQUIRE_2FA is not a valid boolean: %v", err))
	} else {
		cfg.AdminRequire2FA = require2FA
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}

	return cfg, nil
}

// parseCIDRList parses a comma-separated list of CIDRs (ADMIN_IP_ALLOWLIST).
// "" returns (nil, nil) — an empty list, not an error — since an unset
// variable must disable the allowlist rather than fail startup. Every
// non-empty entry must parse as a CIDR or the whole config load fails: a
// single typo'd entry silently narrowing (or, worse, silently widening,
// if the bad entry were just dropped) the allowlist is exactly the kind of
// mistake that must be caught at boot, not discovered when staff are
// locked out.
func parseCIDRList(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var nets []*net.IPNet
	var problems []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(entry)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%q: %v", entry, err))
			continue
		}
		nets = append(nets, ipNet)
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nets, nil
}

// EmailEnabled reports whether a real Resend key is configured. When false
// the worker wires up email.LogSender instead of email.ResendSender.
func (c *Config) EmailEnabled() bool {
	return c.ResendAPIKey != ""
}

// redisKeyPrefix resolves REDIS_KEY_PREFIX, normalising a non-empty value to
// end in ":" so "sapanjai" and "sapanjai:" behave identically rather than the
// former silently producing "sapanjaiblacklist:<token>".
//
// It reads with LookupEnv rather than getEnv because "" is a meaningful value
// here — an operator whose Redis is not shared can set REDIS_KEY_PREFIX= to
// opt out — and getEnv cannot distinguish that from unset.
func redisKeyPrefix() string {
	raw, ok := os.LookupEnv("REDIS_KEY_PREFIX")
	if !ok {
		return defaultRedisKeyPrefix
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasSuffix(raw, ":") {
		return raw
	}
	return raw + ":"
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
