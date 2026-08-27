package email

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	texttemplate "text/template"
)

// templateFS holds the mail bodies. They are embedded rather than read from
// disk so the distroless production image — which has no filesystem to speak
// of — carries them inside the binary.
//
//go:embed templates/*.html templates/*.txt
var templateFS embed.FS

// Subject lines. Constants rather than literals at the call site so the
// service layer and the tests cannot drift apart.
const (
	SubjectVerifyEmail   = "Verify your email address"
	SubjectPasswordReset = "Reset your Sapanjai password"
)

// Expiry phrasing shown in the body. These are copy, not policy: the
// authoritative TTLs are the Redis key expiries set in internal/module/auth.
// Keep the two in sync by hand — a body promising 24 hours over a 1-hour key
// is a support ticket.
const (
	VerifyEmailExpiresIn   = "24 hours"
	PasswordResetExpiresIn = "1 hour"
)

// VerifyEmailData is the template input for the address-verification mail.
// DisplayName is optional and user-controlled — it MUST reach the HTML body
// through html/template's escaping, never through string concatenation.
type VerifyEmailData struct {
	DisplayName string
	VerifyURL   string
	ExpiresIn   string
}

// PasswordResetData is the template input for the password-reset mail. The
// same escaping obligation as VerifyEmailData applies to DisplayName.
type PasswordResetData struct {
	DisplayName string
	ResetURL    string
	ExpiresIn   string
}

// Renderer turns template data into a ready-to-send Message. Build one at
// startup and share it: parsing happens once, in NewRenderer, so a malformed
// template fails the process at boot rather than the first registration.
type Renderer struct {
	html *htmltemplate.Template
	text *texttemplate.Template
}

// NewRenderer parses the embedded templates. The HTML templates are parsed as
// one set so each body can share layout.html via {{define}}/{{template}}; the
// plain-text ones are a separate set because text/template deliberately does
// no escaping.
func NewRenderer() (*Renderer, error) {
	html, err := htmltemplate.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse html templates: %w", err)
	}

	text, err := texttemplate.ParseFS(templateFS, "templates/*.txt")
	if err != nil {
		return nil, fmt.Errorf("parse text templates: %w", err)
	}

	return &Renderer{html: html, text: text}, nil
}

// VerifyEmail renders the address-verification mail for to.
func (r *Renderer) VerifyEmail(to string, data VerifyEmailData) (Message, error) {
	html, text, err := r.render("verify_email.html", "verify_email.txt", data)
	if err != nil {
		return Message{}, err
	}
	return Message{To: to, Subject: SubjectVerifyEmail, HTML: html, Text: text}, nil
}

// PasswordReset renders the password-reset mail for to.
func (r *Renderer) PasswordReset(to string, data PasswordResetData) (Message, error) {
	html, text, err := r.render("password_reset.html", "password_reset.txt", data)
	if err != nil {
		return Message{}, err
	}
	return Message{To: to, Subject: SubjectPasswordReset, HTML: html, Text: text}, nil
}

// render executes the named HTML and text templates against data. Both
// template sets were parsed once in NewRenderer and are safe for concurrent
// Execute calls — html/template and text/template guarantee that as long as
// no template is mutated after parsing, which neither is here.
func (r *Renderer) render(htmlName, textName string, data any) (string, string, error) {
	var htmlBuf, textBuf bytes.Buffer

	if err := r.html.ExecuteTemplate(&htmlBuf, htmlName, data); err != nil {
		return "", "", fmt.Errorf("render %s: %w", htmlName, err)
	}
	if err := r.text.ExecuteTemplate(&textBuf, textName, data); err != nil {
		return "", "", fmt.Errorf("render %s: %w", textName, err)
	}

	return htmlBuf.String(), textBuf.String(), nil
}
