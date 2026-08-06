// Package config loads and validates process configuration from environment
// variables, mirroring the env contract documented in docs/02-api-contract.md.
//
// Unlike the source Node app (which only checked REDIS_URL at boot), Load
// fails fast on ANY missing or invalid required variable and reports all
// problems at once.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const minSecretLen = 32

// maxCleanupBatchSize bounds SESSION_CLEANUP_BATCH_SIZE so a misconfigured
// deployment can't ask the cleanup job to delete unbounded rows in a single
// statement.
const maxCleanupBatchSize = 10_000

type Config struct {
	AppName  string
	AppEnv   string
	Port     string
	LogLevel string

	DatabaseURL string
	RedisURL    string

	JWTAccessSecret     string
	JWTRefreshSecret    string
	JWTAccessExpiresIn  time.Duration
	JWTRefreshExpiresIn time.Duration

	WorkerPort       string
	WorkerJobTimeout time.Duration

	SessionCleanupInterval  time.Duration
	SessionCleanupRetention time.Duration
	SessionCleanupBatchSize int
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

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
