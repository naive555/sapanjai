package email

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	resend "github.com/resend/resend-go/v2"
)

const testFrom = "Sapanjai <noreply@sapanjai.io>"

// capturedRequest is what the fake Resend API saw.
type capturedRequest struct {
	mu      sync.Mutex
	method  string
	path    string
	headers http.Header
	body    map[string]any
	count   int
}

func (c *capturedRequest) snapshot() capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return capturedRequest{method: c.method, path: c.path, headers: c.headers, body: c.body, count: c.count}
}

// newFakeResend stands up an httptest server impersonating api.resend.com and
// returns a ResendSender pointed at it. status/response control what the fake
// API answers with.
func newFakeResend(t *testing.T, status int, response string) (*ResendSender, *capturedRequest) {
	t.Helper()

	captured := &capturedRequest{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		captured.mu.Lock()
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.headers = r.Header.Clone()
		captured.body = body
		captured.count++
		captured.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)

	client := resend.NewClient("re_test_key_123")
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client.BaseURL = base

	return NewResendSenderWithClient(client, testFrom), captured
}

func TestResendSender_PostsTheMessage(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	msg := testMessage()
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := captured.snapshot()
	if got.count != 1 {
		t.Fatalf("upstream saw %d requests, want 1", got.count)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if !strings.HasSuffix(got.path, "/emails") {
		t.Errorf("path = %q, want it to end in /emails", got.path)
	}
}

func TestResendSender_SendsTheConfiguredFromAddress(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if from, _ := captured.snapshot().body["from"].(string); from != testFrom {
		t.Errorf("from = %q, want %q", from, testFrom)
	}
}

// Resend takes "to" as an array even for a single recipient.
func TestResendSender_SendsRecipientAsArray(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	to, ok := captured.snapshot().body["to"].([]any)
	if !ok {
		t.Fatalf(`body["to"] is %T, want a JSON array`, captured.snapshot().body["to"])
	}
	if len(to) != 1 || to[0] != "person@example.com" {
		t.Errorf("to = %v, want [person@example.com]", to)
	}
}

// Both parts must survive to the wire; dropping one silently degrades every
// mail the product sends.
func TestResendSender_SendsBothBodyParts(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	msg := testMessage()
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := captured.snapshot().body
	if got, _ := body["subject"].(string); got != msg.Subject {
		t.Errorf("subject = %q, want %q", got, msg.Subject)
	}
	if got, _ := body["html"].(string); got != msg.HTML {
		t.Errorf("html = %q, want %q", got, msg.HTML)
	}
	if got, _ := body["text"].(string); got != msg.Text {
		t.Errorf("text = %q, want %q", got, msg.Text)
	}
}

// The idempotency key is what makes the outbox's at-least-once claim safe:
// a dispatch run that sends, then dies before recording the send, re-claims
// the row and sends again. Resend collapses the duplicate on this header.
func TestResendSender_SendsIdempotencyKeyHeader(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	msg := testMessage()
	msg.IdempotencyKey = "0f5e4d3c-2b1a-4098-8765-43210fedcba9"

	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := captured.snapshot().headers.Get("Idempotency-Key"); got != msg.IdempotencyKey {
		t.Errorf("Idempotency-Key header = %q, want %q", got, msg.IdempotencyKey)
	}
}

func TestResendSender_OmitsIdempotencyKeyWhenUnset(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := captured.snapshot().headers.Get("Idempotency-Key"); got != "" {
		t.Errorf("Idempotency-Key header = %q, want it absent", got)
	}
}

func TestResendSender_AuthenticatesWithTheAPIKey(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := captured.snapshot().headers.Get("Authorization"); got != "Bearer re_test_key_123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer re_test_key_123")
	}
}

// Every upstream failure has to come back as an error, or the dispatch job
// will mark an undelivered row as sent.
func TestResendSender_ErrorsOnUpstreamFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		response string
	}{
		{"unverified domain", http.StatusForbidden, `{"statusCode":403,"message":"The sapanjai.io domain is not verified"}`},
		{"bad api key", http.StatusUnauthorized, `{"statusCode":401,"message":"API key is invalid"}`},
		{"rate limited", http.StatusTooManyRequests, `{"statusCode":429,"message":"Too many requests"}`},
		{"upstream down", http.StatusInternalServerError, `{"statusCode":500,"message":"Internal server error"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sender, _ := newFakeResend(t, tc.status, tc.response)

			if err := sender.Send(context.Background(), testMessage()); err == nil {
				t.Fatalf("Send returned nil for a %d response; want an error", tc.status)
			}
		})
	}
}

// This error string is written to email_outbox.last_error and to the worker
// log. Neither is a place for a live token.
func TestResendSender_ErrorDoesNotLeakTheBody(t *testing.T) {
	sender, _ := newFakeResend(t, http.StatusForbidden, `{"statusCode":403,"message":"domain not verified"}`)

	msg := testMessage()
	err := sender.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("Send returned nil for a 403")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("error leaked the token: %q", err)
	}
	if strings.Contains(err.Error(), msg.HTML) {
		t.Errorf("error leaked the HTML body: %q", err)
	}
}

// The dispatch job runs under a per-run timeout; a hung upstream must abort
// with it rather than hold the job lock to the end of the interval.
func TestResendSender_RespectsContextCancellation(t *testing.T) {
	sender, captured := newFakeResend(t, http.StatusOK, `{"id":"re_abc123"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := sender.Send(ctx, testMessage()); err == nil {
		t.Fatal("Send returned nil for a cancelled context; want an error")
	}
	if got := captured.snapshot().count; got != 0 {
		t.Errorf("upstream saw %d requests despite a cancelled context, want 0", got)
	}
}

// NewResendSender is the production constructor; it must produce a usable
// Sender without reaching the network at construction time.
func TestNewResendSender_BuildsAWorkingSender(t *testing.T) {
	sender := NewResendSender("re_live_key", testFrom)
	if sender == nil {
		t.Fatal("NewResendSender returned nil")
	}
	var _ Sender = sender
}
