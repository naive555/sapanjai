package logger

import (
	"log/slog"
	"net/url"
	"strings"
)

// Censor replaces the value of any redacted attribute or query parameter,
// matching the censor string used by the pino redact config in the source app
// (src/infrastructure/logger/logger.ts).
const Censor = "[REDACTED]"

// sensitiveKeys are log attribute keys (and URL query parameter names) whose
// values never reach the log output. It is a superset of the pino redact paths
// in the source app (req.headers.authorization, body.password, body.token):
// slog is flat rather than path-addressed, so matching happens on the leaf key
// wherever it appears.
//
// Keys are compared case-insensitively with `-` and `_` stripped, so
// "refresh_token", "refreshToken", "REFRESH-TOKEN" all collapse to the same
// entry.
var sensitiveKeys = map[string]struct{}{
	"authorization":   {},
	"cookie":          {},
	"setcookie":       {},
	"password":        {},
	"newpassword":     {},
	"token":           {},
	"accesstoken":     {},
	"refreshtoken":    {},
	"secret":          {},
	"apikey":          {},
	"connectorconfig": {},
	"encryptedconfig": {},
	"masterkey":       {},
	"datakey":         {},
}

// IsSensitive reports whether a log attribute or query parameter with this key
// must have its value censored.
func IsSensitive(key string) bool {
	_, ok := sensitiveKeys[normalizeKey(key)]
	return ok
}

func normalizeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		switch {
		case r == '-' || r == '_':
			// skip: refresh_token and refreshToken normalize alike
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// redactAttr is the slog.HandlerOptions.ReplaceAttr hook. It censors the value
// of any attribute whose key is sensitive, regardless of which call site logged
// it or which group it sits in — the slog equivalent of pino's redact.paths.
//
// Limitation: slog only exposes the leaf key, so this cannot reach inside a
// struct logged wholesale (slog.Any("body", req) keeps req.Password). Log
// individual fields, not whole request structs.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if IsSensitive(a.Key) {
		return slog.String(a.Key, Censor)
	}
	return a
}

// SanitizeURI censors the values of sensitive query parameters in a request
// URI so an access token passed as ?token=... never lands in the request log.
// The path and all non-sensitive parameters are preserved verbatim; a URI that
// fails to parse is returned with its query string dropped entirely rather
// than risk logging a secret.
func SanitizeURI(uri string) string {
	path, rawQuery, found := strings.Cut(uri, "?")
	if !found || rawQuery == "" {
		return uri
	}

	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return path
	}

	redacted := false
	for key := range values {
		if !IsSensitive(key) {
			continue
		}
		for i := range values[key] {
			values[key][i] = Censor
		}
		redacted = true
	}
	if !redacted {
		return uri
	}

	return path + "?" + values.Encode()
}
