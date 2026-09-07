package googlesheets

import (
	"errors"
	"strings"
	"testing"
)

func validRawConfig() map[string]any {
	return map[string]any{
		"oauth": map[string]any{
			"refresh_token": "1//0g-refresh",
			"client_id":     "abc.apps.googleusercontent.com",
			"client_secret": "shh",
		},
		"scope": map[string]any{
			"spreadsheet_ids":  []any{"sheet-a", "sheet-b"},
			"drive_folder_ids": []any{"folder-a"},
			"header_rows":      map[string]any{"sheet-a": float64(3)},
		},
	}
}

// testPrivateKey is deliberately not a real RSA key. google.JWTConfigFromJSON
// unmarshals the key file and checks its "type" but does not parse the PEM —
// that happens at token-exchange time — so a placeholder is enough to drive
// every parse-level test here, and no test in this package needs a genuine
// private key committed to the repo.
//
// The corollary is worth stating: ParseConfig cannot tell a well-formed key
// file from one whose private key is garbage. The health check is what
// catches that, which is the same division of labour the OAuth variant has
// always had.
const testPrivateKey = "-----BEGIN PRIVATE KEY-----\nnot-a-real-key\n-----END PRIVATE KEY-----\n"

const testServiceAccountEmail = "sapanjai-bot@example-project.iam.gserviceaccount.com"

func serviceAccountKeyJSON() string {
	return `{"type":"service_account","project_id":"example-project",` +
		`"client_email":"` + testServiceAccountEmail + `",` +
		`"private_key":"` + testPrivateKey + `",` +
		`"token_uri":"https://oauth2.googleapis.com/token"}`
}

func validServiceAccountRawConfig() map[string]any {
	raw := validRawConfig()
	delete(raw, "oauth")
	raw["service_account"] = map[string]any{"key_json": serviceAccountKeyJSON()}
	return raw
}

func TestParseConfig_Valid(t *testing.T) {
	cfg, err := ParseConfig(validRawConfig())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Credential.ServiceAccount != nil {
		t.Fatal("an oauth config must not produce a service-account credential")
	}
	if cfg.Credential.OAuth == nil {
		t.Fatal("oauth credential is nil")
	}
	oauth := cfg.Credential.OAuth
	if oauth.RefreshToken != "1//0g-refresh" || oauth.ClientID != "abc.apps.googleusercontent.com" || oauth.ClientSecret != "shh" {
		t.Fatalf("oauth = %+v", oauth)
	}
	if len(cfg.Scope.SpreadsheetIDs) != 2 || len(cfg.Scope.DriveFolderIDs) != 1 {
		t.Fatalf("scope = %+v", cfg.Scope)
	}
}

func TestParseConfig_ServiceAccountValid(t *testing.T) {
	cfg, err := ParseConfig(validServiceAccountRawConfig())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Credential.OAuth != nil {
		t.Fatal("a service-account config must not produce an oauth credential")
	}
	sa := cfg.Credential.ServiceAccount
	if sa == nil {
		t.Fatal("service-account credential is nil")
	}
	if sa.Email != testServiceAccountEmail {
		t.Fatalf("Email = %q, want %q", sa.Email, testServiceAccountEmail)
	}
	// jwt is what makes NewTokenSource infallible, and keyJSON is what
	// oauth.go fingerprints so a rotated key evicts its cached TokenSource.
	// Both are unexported, so this test is the only place their presence
	// can be asserted at all.
	if sa.jwt == nil {
		t.Error("jwt config was not parsed at ParseConfig time")
	}
	if sa.keyJSON != serviceAccountKeyJSON() {
		t.Error("keyJSON was not retained for fingerprinting")
	}
	// The allowlist is credential-agnostic: it must behave identically
	// whichever variant supplied the identity.
	if !cfg.IsSpreadsheetAllowed("sheet-a") || cfg.IsSpreadsheetAllowed("sheet-not-on-allowlist") {
		t.Error("allowlist behaves differently under a service-account credential")
	}
}

func TestParseConfig_ExactlyOneCredentialRequired(t *testing.T) {
	t.Run("both", func(t *testing.T) {
		raw := validServiceAccountRawConfig()
		raw["oauth"] = validRawConfig()["oauth"]
		_, err := ParseConfig(raw)
		if !errors.Is(err, ErrCredentialAmbiguous) {
			t.Fatalf("err = %v, want ErrCredentialAmbiguous", err)
		}
	})
	t.Run("neither", func(t *testing.T) {
		raw := validRawConfig()
		delete(raw, "oauth")
		_, err := ParseConfig(raw)
		if !errors.Is(err, ErrCredentialAmbiguous) {
			t.Fatalf("err = %v, want ErrCredentialAmbiguous", err)
		}
	})
}

func TestParseConfig_ServiceAccountRejectsMalformedKey(t *testing.T) {
	cases := map[string]string{
		"not json":              "nonsense",
		"wrong credential type": `{"type":"authorized_user","client_email":"x@y.z","private_key":"k"}`,
		"missing client_email":  `{"type":"service_account","private_key":"k"}`,
	}
	for name, keyJSON := range cases {
		t.Run(name, func(t *testing.T) {
			raw := validServiceAccountRawConfig()
			raw["service_account"] = map[string]any{"key_json": keyJSON}
			if _, err := ParseConfig(raw); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseConfig_ServiceAccountMissingKeyJSON(t *testing.T) {
	raw := validServiceAccountRawConfig()
	raw["service_account"] = map[string]any{}
	err := errParse(t, raw)
	if !strings.Contains(err.Error(), "config.service_account.key_json") {
		t.Fatalf("error should name the missing field, got: %v", err)
	}
}

// TestParseConfig_ServiceAccountErrorNeverEmbedsKeyBytes is the reason
// parseServiceAccount drops google.JWTConfigFromJSON's error instead of
// wrapping it: that error quotes fragments of the input it failed on, and
// the input is a private key. This error reaches a log line through
// connector.Service.CheckHealth.
func TestParseConfig_ServiceAccountErrorNeverEmbedsKeyBytes(t *testing.T) {
	const marker = "SUPER-SECRET-KEY-MATERIAL"
	raw := validServiceAccountRawConfig()
	raw["service_account"] = map[string]any{
		"key_json": `{"type":"authorized_user","private_key":"` + marker + `"}`,
	}
	err := errParse(t, raw)
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error text embeds key material: %v", err)
	}
	if strings.Contains(err.Error(), "authorized_user") {
		t.Fatalf("error text embeds a value copied out of the key file: %v", err)
	}
}

func errParse(t *testing.T, raw map[string]any) error {
	t.Helper()
	_, err := ParseConfig(raw)
	if err == nil {
		t.Fatal("expected error")
	}
	return err
}

func TestParseConfig_MissingScope(t *testing.T) {
	raw := validRawConfig()
	delete(raw, "scope")
	if _, err := ParseConfig(raw); err == nil {
		t.Fatal("expected error for missing scope block")
	}
}

func TestParseConfig_MissingOAuthField(t *testing.T) {
	for _, field := range []string{"refresh_token", "client_id", "client_secret"} {
		t.Run(field, func(t *testing.T) {
			raw := validRawConfig()
			oauth := raw["oauth"].(map[string]any)
			delete(oauth, field)
			if _, err := ParseConfig(raw); err == nil {
				t.Fatalf("expected error for missing oauth.%s", field)
			}
		})
	}
}

func TestParseConfig_EmptyAllowlistRejected(t *testing.T) {
	raw := validRawConfig()
	raw["scope"] = map[string]any{}
	if _, err := ParseConfig(raw); err == nil {
		t.Fatal("expected error for an allowlist naming neither a spreadsheet nor a folder")
	}
}

func TestParseConfig_ErrorNeverEmbedsSecretValues(t *testing.T) {
	raw := validRawConfig()
	delete(raw["oauth"].(map[string]any), "client_secret")
	_, err := ParseConfig(raw)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "shh") || strings.Contains(err.Error(), "1//0g-refresh") {
		t.Fatalf("error text embeds a credential value: %v", err)
	}
}

func TestConfig_IsSpreadsheetAllowed(t *testing.T) {
	cfg, err := ParseConfig(validRawConfig())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	if !cfg.IsSpreadsheetAllowed("sheet-a") {
		t.Error("sheet-a should be allowed")
	}
	if !cfg.IsSpreadsheetAllowed("sheet-b") {
		t.Error("sheet-b should be allowed")
	}
	// The negative case the plan calls out explicitly: a spreadsheet id
	// the OAuth token could well be able to reach (nothing here asserts
	// otherwise) but that the allowlist simply does not name must be
	// rejected regardless.
	if cfg.IsSpreadsheetAllowed("sheet-not-on-allowlist") {
		t.Error("a spreadsheet id absent from the allowlist must be rejected")
	}
}

func TestConfig_IsFolderAllowed(t *testing.T) {
	cfg, err := ParseConfig(validRawConfig())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	if !cfg.IsFolderAllowed("folder-a") {
		t.Error("folder-a should be allowed")
	}
	if cfg.IsFolderAllowed("folder-not-on-allowlist") {
		t.Error("a folder id absent from the allowlist must be rejected")
	}
}

func TestConfig_HeaderRow(t *testing.T) {
	cfg, err := ParseConfig(validRawConfig())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	if got := cfg.HeaderRow("sheet-a"); got != 3 {
		t.Errorf("HeaderRow(sheet-a) = %d, want 3 (override)", got)
	}
	if got := cfg.HeaderRow("sheet-b"); got != 1 {
		t.Errorf("HeaderRow(sheet-b) = %d, want 1 (default)", got)
	}
	if got := cfg.HeaderRow("sheet-unknown"); got != 1 {
		t.Errorf("HeaderRow(sheet-unknown) = %d, want 1 (default)", got)
	}
}
