package email

import (
	"html/template"
	"strings"
	"testing"
)

// A realistic verification link: one query parameter, 64 hex characters of
// token. Single-parameter is the shape the auth service actually builds, so
// most assertions below can compare against it verbatim.
const testVerifyURL = "https://app.sapanjai.io/verify-email?token=" +
	"8f14e45fceea167a5a36dedd4bea2543b1e3b3c8a2f5d9e0c7b4a1968d2e5f70"

const testResetURL = "https://app.sapanjai.io/reset-password?token=" +
	"c4ca4238a0b923820dcc509a6f75849b3f2a1d8e7c6b5a49382716f0e5d4c3b2"

func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return r
}

func TestNewRenderer_ParsesEmbeddedTemplates(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if r == nil {
		t.Fatal("NewRenderer returned a nil Renderer with no error")
	}
}

func TestRenderer_VerifyEmail_SetsEnvelopeFields(t *testing.T) {
	r := newTestRenderer(t)

	msg, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		DisplayName: "Ada",
		VerifyURL:   testVerifyURL,
		ExpiresIn:   VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	if msg.To != "person@example.com" {
		t.Errorf("To = %q, want %q", msg.To, "person@example.com")
	}
	if msg.Subject != SubjectVerifyEmail {
		t.Errorf("Subject = %q, want %q", msg.Subject, SubjectVerifyEmail)
	}
	// The renderer does not invent an idempotency key; the dispatch job owns
	// that, because only it knows the outbox row id.
	if msg.IdempotencyKey != "" {
		t.Errorf("IdempotencyKey = %q, want empty", msg.IdempotencyKey)
	}
}

// Both parts must always be populated. An HTML-only mail scores badly with
// spam filters and is unreadable in a plain-text client.
func TestRenderer_VerifyEmail_PopulatesBothParts(t *testing.T) {
	r := newTestRenderer(t)

	msg, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		DisplayName: "Ada",
		VerifyURL:   testVerifyURL,
		ExpiresIn:   VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	if strings.TrimSpace(msg.HTML) == "" {
		t.Error("HTML part is empty")
	}
	if strings.TrimSpace(msg.Text) == "" {
		t.Error("Text part is empty")
	}
}

// The link is the entire point of the mail. It must appear in the HTML as a
// real href, and in the text part as a bare URL the reader can copy.
func TestRenderer_VerifyEmail_CarriesTheLinkInBothParts(t *testing.T) {
	r := newTestRenderer(t)

	msg, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		DisplayName: "Ada",
		VerifyURL:   testVerifyURL,
		ExpiresIn:   VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	if !strings.Contains(msg.HTML, `href="`+testVerifyURL+`"`) {
		t.Errorf("HTML part has no href to the verify URL.\nHTML:\n%s", msg.HTML)
	}
	if !strings.Contains(msg.Text, testVerifyURL) {
		t.Errorf("Text part does not contain the verify URL.\nText:\n%s", msg.Text)
	}
}

func TestRenderer_PasswordReset_CarriesTheLinkInBothParts(t *testing.T) {
	r := newTestRenderer(t)

	msg, err := r.PasswordReset("person@example.com", PasswordResetData{
		DisplayName: "Ada",
		ResetURL:    testResetURL,
		ExpiresIn:   PasswordResetExpiresIn,
	})
	if err != nil {
		t.Fatalf("PasswordReset: %v", err)
	}

	if msg.To != "person@example.com" {
		t.Errorf("To = %q, want %q", msg.To, "person@example.com")
	}
	if msg.Subject != SubjectPasswordReset {
		t.Errorf("Subject = %q, want %q", msg.Subject, SubjectPasswordReset)
	}
	if !strings.Contains(msg.HTML, `href="`+testResetURL+`"`) {
		t.Errorf("HTML part has no href to the reset URL.\nHTML:\n%s", msg.HTML)
	}
	if !strings.Contains(msg.Text, testResetURL) {
		t.Errorf("Text part does not contain the reset URL.\nText:\n%s", msg.Text)
	}
}

// The two mails must not be interchangeable: a reset mail that says "verify
// your email" is worse than no mail. Assert each body names its own action.
func TestRenderer_BodiesAreNotInterchangeable(t *testing.T) {
	r := newTestRenderer(t)

	verify, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		VerifyURL: testVerifyURL, ExpiresIn: VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	reset, err := r.PasswordReset("person@example.com", PasswordResetData{
		ResetURL: testResetURL, ExpiresIn: PasswordResetExpiresIn,
	})
	if err != nil {
		t.Fatalf("PasswordReset: %v", err)
	}

	// Compare the prose with the link removed. The verification URL itself
	// contains the word "verify", so checking the raw body would pass even if
	// the copy said nothing about confirming an address at all.
	verifyProse := strings.ToLower(strings.ReplaceAll(verify.Text, testVerifyURL, ""))
	resetProse := strings.ToLower(strings.ReplaceAll(reset.Text, testResetURL, ""))

	if !strings.Contains(verifyProse, "email address") {
		t.Errorf("verification copy never mentions the email address it is about.\nText:\n%s", verify.Text)
	}
	if strings.Contains(verifyProse, "password") {
		t.Errorf("verification copy mentions a password.\nText:\n%s", verify.Text)
	}
	if !strings.Contains(resetProse, "password") {
		t.Errorf("reset copy never mentions a password.\nText:\n%s", reset.Text)
	}
	if verify.Subject == reset.Subject {
		t.Errorf("both mails share the subject %q", verify.Subject)
	}
	if strings.Contains(reset.HTML, testVerifyURL) {
		t.Error("reset HTML contains the verification URL")
	}
}

// DisplayName is user-controlled: whatever someone typed into the register
// form. It reaches an HTML document, so html/template's contextual escaping
// is the only thing between a chosen display name and script execution in
// the recipient's mail client.
func TestRenderer_EscapesDisplayNameInHTML(t *testing.T) {
	r := newTestRenderer(t)

	const payload = `<script>alert('xss')</script>`

	msg, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		DisplayName: payload,
		VerifyURL:   testVerifyURL,
		ExpiresIn:   VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	if strings.Contains(msg.HTML, "<script>") {
		t.Errorf("display name reached the HTML unescaped.\nHTML:\n%s", msg.HTML)
	}
	if !strings.Contains(msg.HTML, template.HTMLEscapeString(payload)) {
		t.Errorf("escaped display name is missing from the HTML.\nHTML:\n%s", msg.HTML)
	}
}

func TestRenderer_EscapesDisplayNameInResetHTML(t *testing.T) {
	r := newTestRenderer(t)

	msg, err := r.PasswordReset("person@example.com", PasswordResetData{
		DisplayName: `<img src=x onerror=alert(1)>`,
		ResetURL:    testResetURL,
		ExpiresIn:   PasswordResetExpiresIn,
	})
	if err != nil {
		t.Fatalf("PasswordReset: %v", err)
	}

	if strings.Contains(msg.HTML, "<img src=x") {
		t.Errorf("display name reached the reset HTML unescaped.\nHTML:\n%s", msg.HTML)
	}
}

// Most users register without a display name (it is optional in
// RegisterRequest). The greeting must degrade to something generic rather
// than rendering "Hi ," or a dangling comma.
func TestRenderer_EmptyDisplayNameLeavesNoArtifact(t *testing.T) {
	r := newTestRenderer(t)

	msg, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		DisplayName: "",
		VerifyURL:   testVerifyURL,
		ExpiresIn:   VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	for _, artifact := range []string{"Hi ,", "Hi ,", "Hello ,", " ,", "{{", "<no value>"} {
		if strings.Contains(msg.Text, artifact) {
			t.Errorf("text part contains the artifact %q for an empty display name.\nText:\n%s", artifact, msg.Text)
		}
		if strings.Contains(msg.HTML, artifact) {
			t.Errorf("HTML part contains the artifact %q for an empty display name.\nHTML:\n%s", artifact, msg.HTML)
		}
	}
}

// The expiry sentence is the difference between "this link is broken" and "I
// waited too long". It has to be in the body the reader actually sees, both
// of them.
func TestRenderer_StatesExpiryInBothParts(t *testing.T) {
	r := newTestRenderer(t)

	verify, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		VerifyURL: testVerifyURL, ExpiresIn: VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if !strings.Contains(verify.HTML, VerifyEmailExpiresIn) {
		t.Errorf("verification HTML omits the expiry %q", VerifyEmailExpiresIn)
	}
	if !strings.Contains(verify.Text, VerifyEmailExpiresIn) {
		t.Errorf("verification text omits the expiry %q", VerifyEmailExpiresIn)
	}

	reset, err := r.PasswordReset("person@example.com", PasswordResetData{
		ResetURL: testResetURL, ExpiresIn: PasswordResetExpiresIn,
	})
	if err != nil {
		t.Fatalf("PasswordReset: %v", err)
	}
	if !strings.Contains(reset.HTML, PasswordResetExpiresIn) {
		t.Errorf("reset HTML omits the expiry %q", PasswordResetExpiresIn)
	}
	if !strings.Contains(reset.Text, PasswordResetExpiresIn) {
		t.Errorf("reset text omits the expiry %q", PasswordResetExpiresIn)
	}
}

// A multi-parameter URL is not the shape the auth service builds today, but
// it costs nothing to be robust. html/template escapes "&" to "&amp;" inside
// an attribute, which is correct HTML and which every mail client unescapes
// before navigating — so accept either form there. The plain-text part goes
// through text/template and must carry the URL byte-for-byte.
func TestRenderer_MultiParamURLSurvivesEscaping(t *testing.T) {
	r := newTestRenderer(t)

	const url = "https://app.sapanjai.io/verify-email?token=abc123&redirect=%2Foverview"

	msg, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		VerifyURL: url, ExpiresIn: VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}

	escaped := strings.ReplaceAll(url, "&", "&amp;")
	if !strings.Contains(msg.HTML, url) && !strings.Contains(msg.HTML, escaped) {
		t.Errorf("HTML part carries the URL in neither raw nor &amp;-escaped form.\nHTML:\n%s", msg.HTML)
	}
	if !strings.Contains(msg.Text, url) {
		t.Errorf("text part mangled the URL; want it verbatim.\nText:\n%s", msg.Text)
	}
}

// html/template refuses to emit a URL it considers unsafe, substituting
// "#ZgotmplZ". If that ever appears the link is silently dead, which is the
// worst possible failure mode for this mail.
func TestRenderer_DoesNotFilterTheLinkAsUnsafe(t *testing.T) {
	r := newTestRenderer(t)

	msg, err := r.VerifyEmail("person@example.com", VerifyEmailData{
		VerifyURL: testVerifyURL, ExpiresIn: VerifyEmailExpiresIn,
	})
	if err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
	if strings.Contains(msg.HTML, "ZgotmplZ") {
		t.Errorf("html/template filtered the link as unsafe.\nHTML:\n%s", msg.HTML)
	}
}

// A renderer is built once at startup and shared across every request the
// API serves, so its methods must be safe to call concurrently.
func TestRenderer_ConcurrentUse(t *testing.T) {
	r := newTestRenderer(t)

	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := r.VerifyEmail("person@example.com", VerifyEmailData{
				DisplayName: "Ada", VerifyURL: testVerifyURL, ExpiresIn: VerifyEmailExpiresIn,
			})
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent VerifyEmail: %v", err)
		}
	}
}
