package email

import (
	"context"
	"errors"
	"log/slog"
)

// LogSender is the Sender used when no RESEND_API_KEY is configured. It
// exists so `make worker` works with no third-party account: in development
// it writes the whole rendered body to the log, which is how a developer gets
// at the verification link without a mailbox.
//
// In production it does the opposite. Logging a live token to a production
// log is not acceptable, and neither is silently succeeding: a Send that
// returns nil would mark the outbox row 'sent' when nothing was sent, hiding
// a real outage behind a green row. So production logs recipient and subject
// only and returns an error, which retries, exhausts the attempt budget, and
// leaves "no RESEND_API_KEY configured" in email_outbox.last_error where an
// operator will actually find it.
type LogSender struct {
	log        *slog.Logger
	production bool
}

var _ Sender = (*LogSender)(nil)

// NewLogSender builds a LogSender. appEnv is the raw APP_ENV value; anything
// other than "production" is treated as development. Compared exactly, not
// case-folded: a typo like "Production" must not accidentally turn off the
// one thing that makes this useful in dev.
func NewLogSender(log *slog.Logger, appEnv string) *LogSender {
	return &LogSender{log: log, production: appEnv == "production"}
}

// Send logs m instead of delivering it. See the type comment for why the
// production and development branches differ in both what they log and what
// they return.
func (s *LogSender) Send(ctx context.Context, m Message) error {
	if s.production {
		s.log.WarnContext(ctx, "email not sent: no RESEND_API_KEY configured",
			"to", m.To, "subject", m.Subject)
		return errors.New("email not sent: no RESEND_API_KEY configured")
	}

	s.log.InfoContext(ctx, "email (dev, not actually sent)",
		"to", m.To, "subject", m.Subject, "html", m.HTML, "text", m.Text)
	return nil
}
