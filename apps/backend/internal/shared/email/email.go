// Package email renders and delivers transactional mail.
//
// Two concerns live here and nothing else: how a message is built (Renderer,
// from embedded templates) and how it leaves the process (Sender). Neither
// knows *why* the mail is being sent — that belongs to the caller
// (internal/module/auth) — and neither touches the email_outbox table, which
// is the dispatch job's concern (internal/job/emaildispatch).
//
// Rendered bodies carry live, single-use bearer tokens. Nothing here may log
// a Message's HTML or Text outside the explicit development-mode branch in
// LogSender, and callers must not either. The centralised redaction in
// internal/shared/logger/redact.go cannot help: it matches attribute *keys*,
// and the token is inside a value.
package email

import "context"

// Message is one outbound email, fully rendered and ready to send.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string

	// IdempotencyKey lets the provider collapse a duplicate delivery of the
	// same message into a single send. The dispatch job passes the outbox row
	// id: a run that sends successfully and then dies before recording that
	// fact re-claims the row once its lease expires, and this key is what
	// keeps the recipient from receiving the mail twice. Optional — an empty
	// value sends no key at all.
	IdempotencyKey string
}

// Sender delivers a rendered Message.
//
// Implementations must treat every returned error as retryable: the dispatch
// job backs off and retries on any error and gives up only after a configured
// attempt count. An error must never carry the Message's body — that body
// contains a live token.
type Sender interface {
	Send(ctx context.Context, m Message) error
}
