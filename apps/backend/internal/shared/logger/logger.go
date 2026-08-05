// Package logger builds the application's structured logger.
//
// Sensitive attribute values (Authorization headers, passwords, tokens) are
// censored by every handler this package builds — see redact.go, which mirrors
// the pino redact config in the source app
// (src/infrastructure/logger/logger.ts).
package logger

import (
	"log/slog"
	"os"
)

// New builds a slog.Logger. In development it uses a human-readable text
// handler; otherwise structured JSON, matching the pino pretty-vs-json
// split in the source app.
func New(appEnv, logLevel string) *slog.Logger {
	level := parseLevel(logLevel)
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}

	var handler slog.Handler
	if appEnv == "development" || appEnv == "local" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// parseLevel maps the pino-style level names used by the source app's
// LOG_LEVEL env var onto slog levels. slog has no "trace" or "fatal" level,
// so trace maps to debug and fatal maps to error.
func parseLevel(raw string) slog.Level {
	switch raw {
	case "trace", "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error", "fatal":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
