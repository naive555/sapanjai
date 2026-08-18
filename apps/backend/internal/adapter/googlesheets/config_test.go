package googlesheets

import (
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

func TestParseConfig_Valid(t *testing.T) {
	cfg, err := ParseConfig(validRawConfig())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.OAuth.RefreshToken != "1//0g-refresh" || cfg.OAuth.ClientID != "abc.apps.googleusercontent.com" || cfg.OAuth.ClientSecret != "shh" {
		t.Fatalf("oauth = %+v", cfg.OAuth)
	}
	if len(cfg.Scope.SpreadsheetIDs) != 2 || len(cfg.Scope.DriveFolderIDs) != 1 {
		t.Fatalf("scope = %+v", cfg.Scope)
	}
}

func TestParseConfig_MissingOAuth(t *testing.T) {
	raw := validRawConfig()
	delete(raw, "oauth")
	if _, err := ParseConfig(raw); err == nil {
		t.Fatal("expected error for missing oauth block")
	}
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
