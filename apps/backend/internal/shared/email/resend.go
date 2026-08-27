package email

import (
	"context"
	"fmt"

	resend "github.com/resend/resend-go/v2"
)

// ResendSender delivers mail through the Resend API. It is the only Sender
// that talks to a third party, and it is deliberately thin: no retry, no
// backoff, no queueing. Retry policy belongs to the dispatch job, which owns
// the attempt count and the outbox row.
type ResendSender struct {
	client *resend.Client
	from   string
}

var _ Sender = (*ResendSender)(nil)

// NewResendSender builds a sender against the live Resend API. from must be a
// verified sending identity ("Name <addr@domain>"); an unverified domain
// makes every send fail with a 403 that surfaces in email_outbox.last_error.
func NewResendSender(apiKey, from string) *ResendSender {
	return NewResendSenderWithClient(resend.NewClient(apiKey), from)
}

// NewResendSenderWithClient builds a sender around an already-configured
// client. Production uses NewResendSender; this exists so a caller can supply
// a client with a custom BaseURL or http.Client — which is what the tests do
// to point it at an httptest server instead of api.resend.com.
func NewResendSenderWithClient(client *resend.Client, from string) *ResendSender {
	return &ResendSender{client: client, from: from}
}

// Send posts m to Resend. When m.IdempotencyKey is set it travels as the
// Idempotency-Key header, so a message the dispatch job re-sends after a
// crash is collapsed by the provider rather than delivered twice.
//
// A returned error must describe the failure without quoting m.HTML or
// m.Text: those bodies contain a live verification or reset token, and this
// error string ends up in email_outbox.last_error and the worker log.
func (s *ResendSender) Send(ctx context.Context, m Message) error {
	req := &resend.SendEmailRequest{
		From:    s.from,
		To:      []string{m.To},
		Subject: m.Subject,
		Html:    m.HTML,
		Text:    m.Text,
	}

	// Always pass a concrete, non-nil *SendEmailOptions: the SDK accepts an
	// Options interface, and a nil *SendEmailOptions boxed into that
	// interface is a non-nil interface value whose GetIdempotencyKey (a
	// value-receiver method) panics on the nil pointer receiver. An empty
	// IdempotencyKey is the "omit the header" case the library already
	// handles — only a POST with a non-empty key gets the header at all.
	options := &resend.SendEmailOptions{IdempotencyKey: m.IdempotencyKey}

	if _, err := s.client.Emails.SendWithOptions(ctx, req, options); err != nil {
		return fmt.Errorf("resend: send to %s: %w", m.To, err)
	}
	return nil
}
