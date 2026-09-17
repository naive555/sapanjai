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

// maxConnectorHealthBatchSize bounds CONNECTOR_HEALTH_BATCH_SIZE for the same
// reason maxCleanupBatchSize exists.
const maxConnectorHealthBatchSize = 1000

// maxUsageRollupBatchSize bounds USAGE_ROLLUP_BATCH_SIZE. usage_events is
// the largest table in the schema (one row per billable tool call, per
// migration 00014's comment), and its prune query is shaped exactly like
// DeleteExpiredSessions' (id IN (SELECT ... LIMIT n)), so it gets the same
// generous ceiling as maxCleanupBatchSize rather than
// maxConnectorHealthBatchSize's tighter one (that job's batch is bounded by
// how many upstream health probes a single sweep should attempt, an
// unrelated concern).
const maxUsageRollupBatchSize = 10_000

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

	// ConnectorHealthInterval is how often the connector-health sweep
	// (internal/job/connectorhealth) runs.
	ConnectorHealthInterval time.Duration

	// ConnectorHealthBatchSize is how many connectors one sweep checks,
	// oldest-checked-first (nulls -- never checked -- first).
	ConnectorHealthBatchSize int

	// UsageRollupInterval is how often internal/job/usagerollup folds
	// usage_events into usage_rollups, cross-checks the count against
	// audit_logs, and prunes usage_events past retention. The rollup query
	// is a single bounded aggregate (rollupLookbackMonths trailing calendar
	// months, not the whole table) and is idempotent, so running it often
	// is cheap and safe -- and running it often matters, because a stale
	// rollup is a stale view for whatever later enforces
	// max_tool_calls_per_month: a customer could blow well past a cap
	// before anyone (human or code) notices. 15m keeps that lag small
	// without re-aggregating on every gateway request the way a per-call
	// count would.
	UsageRollupInterval time.Duration

	// UsageEventsRetention is how long usage_events rows are kept before
	// internal/job/usagerollup prunes them, once folded into a durable
	// usage_rollups row. This number (2160h/90 days) and its reasoning were
	// decided in migration 00014's comment on usage_events, not here: it
	// covers roughly three monthly billing cycles of investigation
	// headroom -- disputes, the audit_logs drift cross-check's forensic
	// window -- well past any plausible rollup-job outage, without keeping
	// a per-call ledger forever.
	UsageEventsRetention time.Duration

	// UsageRollupBatchSize is how many usage_events rows one prune
	// statement deletes at a time (internal/job/usagerollup), following
	// SESSION_CLEANUP_BATCH_SIZE's shape -- the rollup step itself is a
	// single unbatched aggregate query, so this only bounds the prune.
	UsageRollupBatchSize int

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

	// StripeSecretKey authenticates every Stripe API call the billing module
	// (internal/module/billing) makes: Checkout Session creation, Customer
	// Portal session creation, and the lazy Customer create behind both.
	//
	// This SHOULD be a restricted key ("rk_...") scoped to Checkout, Billing,
	// and Customer write — not an account-wide secret key. Note the
	// divergence from the RESEND_API_KEY precedent, where the secret is read
	// only by cmd/worker so it never sits on the internet-facing service:
	// starting a checkout is request-driven, so the API must hold this one.
	// The restriction on the key is what bounds the blast radius instead.
	//
	// Optional, and empty by default — the same "degrade, don't fail"
	// posture RESEND_API_KEY takes. Unset, the /billing routes stay mounted
	// and stay guarded but answer BILLING_NOT_CONFIGURED, so a developer
	// with no Stripe account can still boot the API and run the whole test
	// suite. Set but malformed is a different matter and fails at boot: a
	// key that cannot possibly work is a typo an operator should hear about
	// immediately, not at the first customer's upgrade attempt.
	//
	// Never log this value. logger.redact.go already censors any attr key
	// spelled like a Stripe key; there is still no call site that should be
	// logging it.
	StripeSecretKey string

	// StripeWebhookSecret ("whsec_...") is the HMAC key POST /billing/webhook
	// verifies the Stripe-Signature header against. It is the ONLY
	// authentication that route has — it cannot sit on RequireAuth, because
	// Stripe presents no JWT — so an empty value does not mean "accept
	// everything", it means the route answers BILLING_NOT_CONFIGURED and
	// reconciles nothing.
	//
	// A DIFFERENT secret from StripeSecretKey, and not derivable from it:
	// Stripe issues one per webhook endpoint, and rotating either leaves the
	// other alone. Optional and empty by default, the same degrade-don't-fail
	// posture RESEND_API_KEY and STRIPE_SECRET_KEY take, so a developer with
	// no Stripe account can boot the API and run the whole test suite.
	//
	// Set but malformed fails at boot, for the same reason a malformed
	// STRIPE_SECRET_KEY does: a secret that cannot possibly verify is a typo
	// an operator should hear about immediately, not discover as a pile of
	// 400s in the Stripe dashboard's webhook log days later — by which point
	// Stripe has disabled the endpoint and the renewals it was silently
	// dropping are unrecoverable without a manual replay.
	//
	// Never log this value; logger.redact.go already censors "webhooksecret".
	StripeWebhookSecret string

	// StripeWebhookIPAllowlist narrows POST /billing/webhook to Stripe's
	// published egress CIDRs (billing plan step 7's "also allowlist Stripe's
	// published IPs on the webhook route — ADMIN_IP_ALLOWLIST is the existing
	// pattern to copy"). Same parsing, same middleware
	// (internal/middleware.IPAllowlist), same 404-not-403 rejection.
	//
	// Defence in depth only, never the primary control: the signature check
	// is what actually authenticates a delivery, and this list would be
	// worthless on its own. Which is also why it defaults to unset/disabled —
	// Stripe's IP ranges change, a stale list silently drops live billing
	// events, and c.RealIP() is only as trustworthy as the proxy chain in
	// front of this API (see server.go's e.IPExtractor comment). An operator
	// who sets it must have a plan for keeping it current.
	StripeWebhookIPAllowlist []*net.IPNet
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
		{"CONNECTOR_HEALTH_INTERVAL", "6h", &cfg.ConnectorHealthInterval},
		{"USAGE_ROLLUP_INTERVAL", "15m", &cfg.UsageRollupInterval},
		{"USAGE_EVENTS_RETENTION", "2160h", &cfg.UsageEventsRetention},
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

	connectorHealthBatchSize, err := strconv.Atoi(getEnv("CONNECTOR_HEALTH_BATCH_SIZE", "50"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("CONNECTOR_HEALTH_BATCH_SIZE is not a valid integer: %v", err))
	case connectorHealthBatchSize < 1 || connectorHealthBatchSize > maxConnectorHealthBatchSize:
		problems = append(problems, fmt.Sprintf("CONNECTOR_HEALTH_BATCH_SIZE must be between 1 and %d", maxConnectorHealthBatchSize))
	default:
		cfg.ConnectorHealthBatchSize = connectorHealthBatchSize
	}

	usageRollupBatchSize, err := strconv.Atoi(getEnv("USAGE_ROLLUP_BATCH_SIZE", "1000"))
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("USAGE_ROLLUP_BATCH_SIZE is not a valid integer: %v", err))
	case usageRollupBatchSize < 1 || usageRollupBatchSize > maxUsageRollupBatchSize:
		problems = append(problems, fmt.Sprintf("USAGE_ROLLUP_BATCH_SIZE must be between 1 and %d", maxUsageRollupBatchSize))
	default:
		cfg.UsageRollupBatchSize = usageRollupBatchSize
	}

	allowlist, err := parseCIDRList(os.Getenv("ADMIN_IP_ALLOWLIST"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("ADMIN_IP_ALLOWLIST is invalid: %v", err))
	} else {
		cfg.AdminIPAllowlist = allowlist
	}

	cfg.StripeSecretKey = strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
	if err := validateStripeKey(cfg.StripeSecretKey); err != nil {
		problems = append(problems, fmt.Sprintf("STRIPE_SECRET_KEY is invalid: %v", err))
	}

	cfg.StripeWebhookSecret = strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET"))
	if err := validateStripeWebhookSecret(cfg.StripeWebhookSecret); err != nil {
		problems = append(problems, fmt.Sprintf("STRIPE_WEBHOOK_SECRET is invalid: %v", err))
	}

	webhookAllowlist, err := parseCIDRList(os.Getenv("STRIPE_WEBHOOK_IP_ALLOWLIST"))
	if err != nil {
		problems = append(problems, fmt.Sprintf("STRIPE_WEBHOOK_IP_ALLOWLIST is invalid: %v", err))
	} else {
		cfg.StripeWebhookIPAllowlist = webhookAllowlist
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

// BillingEnabled reports whether a Stripe key is configured. When false the
// /billing routes are still registered and still permission-guarded, but
// every one of them answers apperror.BillingNotConfigured rather than
// dialling Stripe — mirroring EmailEnabled's degrade-don't-fail posture, and
// keeping the route surface identical between a Stripe-less local dev box
// and production so a guard test means the same thing in both.
func (c *Config) BillingEnabled() bool {
	return c.StripeSecretKey != ""
}

// validateStripeKey rejects a STRIPE_SECRET_KEY that cannot possibly
// authenticate, so the mistake surfaces at boot rather than at the first
// customer's checkout. "" is valid and means billing is disabled
// (BillingEnabled).
//
// The prefixes Stripe issues for server-side use are "rk_" (restricted, what
// this deployment should use) and "sk_" (account-wide secret). "pk_" is
// called out separately because pasting the publishable key into the secret
// slot is the specific, common mix-up worth naming in the error — it would
// otherwise fail every Stripe call at runtime with an opaque 401.
//
// Note what this does NOT do: it never echoes the key. An invalid-value
// error that quoted the offending string would put a live credential into
// the boot log of every crash-looping replica.
func validateStripeKey(key string) error {
	switch {
	case key == "":
		return nil
	case strings.HasPrefix(key, "rk_"), strings.HasPrefix(key, "sk_"):
		return nil
	case strings.HasPrefix(key, "pk_"):
		return fmt.Errorf("looks like a publishable key (pk_...); this must be a restricted key (rk_...) or a secret key (sk_...)")
	default:
		return fmt.Errorf("must be a Stripe restricted key (rk_...) or secret key (sk_...)")
	}
}

// WebhookEnabled reports whether a Stripe webhook signing secret is
// configured. When false, POST /billing/webhook is still mounted and still
// IP-gated, but answers apperror.BillingNotConfigured rather than pretending
// to verify anything — the same posture BillingEnabled takes, and tracked
// separately because the two secrets are issued, rotated, and can be
// forgotten independently.
func (c *Config) WebhookEnabled() bool {
	return c.StripeWebhookSecret != ""
}

// validateStripeWebhookSecret rejects a STRIPE_WEBHOOK_SECRET that cannot
// possibly verify a signature, so the mistake surfaces at boot rather than
// as a silent pile of 400s in Stripe's webhook log. "" is valid and means
// the webhook is disabled (WebhookEnabled).
//
// Stripe issues endpoint signing secrets as "whsec_...". The common mix-up
// worth naming is pasting an API key ("sk_"/"rk_"/"pk_") into this slot —
// the two live next to each other on the same dashboard page, and every
// delivery would then fail signature verification with nothing to explain
// why.
//
// Like validateStripeKey, this never echoes the value: an invalid-value
// error quoting the offending string would put a live signing secret into
// the boot log of every crash-looping replica.
func validateStripeWebhookSecret(secret string) error {
	switch {
	case secret == "":
		return nil
	case strings.HasPrefix(secret, "whsec_"):
		return nil
	case strings.HasPrefix(secret, "sk_"), strings.HasPrefix(secret, "rk_"), strings.HasPrefix(secret, "pk_"):
		return fmt.Errorf("looks like a Stripe API key; this must be the endpoint signing secret (whsec_...)")
	default:
		return fmt.Errorf("must be a Stripe webhook signing secret (whsec_...)")
	}
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
