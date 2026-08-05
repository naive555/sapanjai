package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestIsSensitive(t *testing.T) {
	sensitive := []string{
		"authorization", "Authorization", "AUTHORIZATION",
		"password", "newPassword", "new_password",
		"token", "accessToken", "access_token", "refreshToken", "refresh-token",
		"cookie", "Set-Cookie", "secret", "apiKey", "api_key",
	}
	for _, key := range sensitive {
		if !IsSensitive(key) {
			t.Errorf("IsSensitive(%q) = false, want true", key)
		}
	}

	safe := []string{"userId", "email", "action", "status", "uri", "method", "request_id", "tokenCount"}
	for _, key := range safe {
		if IsSensitive(key) {
			t.Errorf("IsSensitive(%q) = true, want false", key)
		}
	}
}

// TestNewRedactsSensitiveAttrs is the core guarantee: whatever a call site
// passes, a sensitive key never reaches the output.
func TestNewRedactsSensitiveAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: redactAttr}))

	log.Info("request",
		"authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.secret",
		"refreshToken", "rt_supersecret",
		"password", "hunter2",
		"email", "user@example.com",
	)

	out := buf.String()
	for _, secret := range []string{"eyJhbGciOiJIUzI1NiJ9", "rt_supersecret", "hunter2"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaked %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "user@example.com") {
		t.Errorf("output dropped non-sensitive attr: %s", out)
	}

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, key := range []string{"authorization", "refreshToken", "password"} {
		if record[key] != Censor {
			t.Errorf("record[%q] = %v, want %q", key, record[key], Censor)
		}
	}
	if record["msg"] != "request" || record["level"] != "INFO" {
		t.Errorf("built-in attrs mangled by redaction: %v", record)
	}
}

func TestNewRedactsInsideGroupsAndWith(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: redactAttr})).
		With("token", "leaked-via-with")

	log.Info("grouped", slog.Group("req", "authorization", "Bearer abc123"))

	out := buf.String()
	if strings.Contains(out, "leaked-via-with") || strings.Contains(out, "abc123") {
		t.Errorf("output leaked a secret: %s", out)
	}
}

func TestSanitizeURI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no query", "/audit-logs", "/audit-logs"},
		{"empty query", "/audit-logs?", "/audit-logs?"},
		{"no sensitive params", "/audit-logs?userId=abc&limit=50", "/audit-logs?userId=abc&limit=50"},
		{"token censored", "/auth/refresh?token=secret", "/auth/refresh?token=" + enc(Censor)},
		{"mixed params", "/x?access_token=secret&userId=abc", "/x?access_token=" + enc(Censor) + "&userId=abc"},
		{"repeated param", "/x?token=a&token=b", "/x?token=" + enc(Censor) + "&token=" + enc(Censor)},
		{"unparseable query drops it", "/x?token=%zz", "/x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeURI(tt.in); got != tt.want {
				t.Errorf("SanitizeURI(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// enc percent-encodes a value the way url.Values.Encode does, so the expected
// strings above stay readable.
func enc(v string) string {
	return strings.NewReplacer("[", "%5B", "]", "%5D").Replace(v)
}
