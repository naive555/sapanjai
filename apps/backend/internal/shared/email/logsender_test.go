package email

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// The token that must never reach a production log. Distinctive enough that
// a substring search for it is meaningful.
const secretToken = "8f14e45fceea167a5a36dedd4bea2543b1e3b3c8a2f5d9e0c7b4a1968d2e5f70"

func testMessage() Message {
	link := "https://app.sapanjai.io/verify-email?token=" + secretToken
	return Message{
		To:      "person@example.com",
		Subject: SubjectVerifyEmail,
		HTML:    `<p>Hi Ada</p><a href="` + link + `">Verify</a>`,
		Text:    "Hi Ada\n\nVerify your email: " + link + "\n",
	}
}

// newCapturingLogSender returns a LogSender writing into a buffer the test
// can inspect.
func newCapturingLogSender(appEnv string) (*LogSender, *bytes.Buffer) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewLogSender(log, appEnv), &buf
}

// In development the whole point of LogSender is to hand the developer the
// link, since there is no mailbox to check.
func TestLogSender_DevelopmentLogsTheLink(t *testing.T) {
	s, buf := newCapturingLogSender("development")

	if err := s.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send returned %v, want nil in development", err)
	}
	if !strings.Contains(buf.String(), secretToken) {
		t.Errorf("development log does not contain the link.\nlog:\n%s", buf.String())
	}
}

// Anything that is not exactly "production" is development. A typo in
// APP_ENV must not silently turn logging off.
func TestLogSender_UnknownEnvIsTreatedAsDevelopment(t *testing.T) {
	for _, appEnv := range []string{"", "dev", "staging", "Production", "PRODUCTION"} {
		t.Run(appEnv, func(t *testing.T) {
			s, buf := newCapturingLogSender(appEnv)
			if err := s.Send(context.Background(), testMessage()); err != nil {
				t.Fatalf("Send returned %v, want nil for APP_ENV=%q", err, appEnv)
			}
			if !strings.Contains(buf.String(), secretToken) {
				t.Errorf("APP_ENV=%q did not log the link.\nlog:\n%s", appEnv, buf.String())
			}
		})
	}
}

// The core safety property: a live bearer token must never reach a
// production log, in either body part.
func TestLogSender_ProductionNeverLogsTheBody(t *testing.T) {
	s, buf := newCapturingLogSender("production")

	msg := testMessage()
	_ = s.Send(context.Background(), msg)

	logged := buf.String()
	if strings.Contains(logged, secretToken) {
		t.Errorf("production log leaked the token.\nlog:\n%s", logged)
	}
	if strings.Contains(logged, msg.HTML) {
		t.Errorf("production log leaked the HTML body.\nlog:\n%s", logged)
	}
	if strings.Contains(logged, msg.Text) {
		t.Errorf("production log leaked the text body.\nlog:\n%s", logged)
	}
}

// Recipient and subject are safe and are what an operator needs to correlate
// a complaint with a row, so they must still be logged.
func TestLogSender_ProductionLogsRecipientAndSubject(t *testing.T) {
	s, buf := newCapturingLogSender("production")

	_ = s.Send(context.Background(), testMessage())

	logged := buf.String()
	if !strings.Contains(logged, "person@example.com") {
		t.Errorf("production log omits the recipient.\nlog:\n%s", logged)
	}
	if !strings.Contains(logged, SubjectVerifyEmail) {
		t.Errorf("production log omits the subject.\nlog:\n%s", logged)
	}
}

// Returning nil here would mark the outbox row 'sent' when nothing was sent,
// turning a production misconfiguration into a silently green table. The
// error is what puts "no RESEND_API_KEY" into email_outbox.last_error.
func TestLogSender_ProductionReturnsError(t *testing.T) {
	s, _ := newCapturingLogSender("production")

	err := s.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send returned nil in production; want an error so the outbox row does not read as sent")
	}
	if !strings.Contains(err.Error(), "RESEND_API_KEY") {
		t.Errorf("error %q does not name RESEND_API_KEY, so an operator cannot act on it", err)
	}
}

// That error string lands in email_outbox.last_error, which is a database
// column an operator reads. It must not carry the token either.
func TestLogSender_ProductionErrorDoesNotLeakTheBody(t *testing.T) {
	s, _ := newCapturingLogSender("production")

	err := s.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send returned nil in production")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("error string leaked the token: %q", err)
	}
}
