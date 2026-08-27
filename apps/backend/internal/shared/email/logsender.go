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
// Anywhere else it does the opposite. Logging a live token from a deployed
// environment is not acceptable, and neither is silently succeeding: a Send
// that returns nil would mark the outbox row 'sent' when nothing was sent,
// hiding a real outage behind a green row. So a deployed environment logs
// recipient and subject only and returns an error, which retries, exhausts
// the attempt budget, and leaves "no RESEND_API_KEY configured" in
// email_outbox.last_error where an operator will actually find it.
//
// "Deployed" is everything outside localEnvs below — staging and preview
// included. See that variable for why this is an allowlist.
type LogSender struct {
	log      *slog.Logger
	localEnv bool
}

var _ Sender = (*LogSender)(nil)

// localEnvs are the only APP_ENV values whose logs are allowed to carry a
// rendered body.
//
// This is an allowlist, not "anything that is not production". The negated
// form failed open: a staging or preview deployment with no RESEND_API_KEY
// took the development branch and wrote live account-takeover tokens into
// aggregated logs. An unrecognised value now gets the safe branch.
var localEnvs = map[string]bool{"development": true, "local": true, "test": true}

// NewLogSender builds a LogSender. appEnv is the raw APP_ENV value, compared
// exactly — a value that is not an exact match for a known local environment
// is treated as deployed.
func NewLogSender(log *slog.Logger, appEnv string) *LogSender {
	return &LogSender{log: log, localEnv: localEnvs[appEnv]}
}

// Send logs m instead of delivering it. See the type comment for why the
// production and development branches differ in both what they log and what
// they return.
func (s *LogSender) Send(ctx context.Context, m Message) error {
	if !s.localEnv {
		s.log.WarnContext(ctx, "email not sent: no RESEND_API_KEY configured",
			"to", m.To, "subject", m.Subject)
		return errors.New("email not sent: no RESEND_API_KEY configured")
	}

	s.log.InfoContext(ctx, "email (dev, not actually sent)",
		"to", m.To, "subject", m.Subject, "html", m.HTML, "text", m.Text)
	return nil
}
