// Package googlesheets is the connector adapter for type "google_sheets"
// (docs/06-sheets-adapter.md, docs/07-sheets-adapter-plan.md step 5). It
// imports internal/module/connector for the Checker interface; connector
// does not import this package back — the registry that wires them together
// is assembled in internal/server/server.go, so there is no import cycle.
//
// The one invariant every file here must hold: a connector's decrypted
// config (an OAuth client secret and refresh token — a live customer
// credential) never leaves this package. It is never logged, never
// embedded in an error string returned to a caller, and never retained
// past the call that needed it.
package googlesheets

import (
	"errors"
	"fmt"
)

// ErrSpreadsheetNotAllowed is returned by every adapter operation that reads
// a spreadsheet whose id is absent from the connector's own allowlist
// (config.scope.spreadsheet_ids) — "the spec's single most important
// security boundary" (docs/06-sheets-adapter.md §3), enforced on every call
// against the freshly decrypted config, never a value cached from
// connector-creation time. Wrapped with the offending id via fmt.Errorf's
// %w, so a caller can both errors.Is against this sentinel and read the id
// out of Error() — the id is an allowlist entry, not a credential, so it is
// safe to surface to the model (docs/06-sheets-adapter.md §8
// SPREADSHEET_NOT_ALLOWED).
var ErrSpreadsheetNotAllowed = errors.New("googlesheets: spreadsheet not on connector allowlist")

// Config is the parsed, validated shape of a "google_sheets" connector's
// decrypted config (docs/06-sheets-adapter.md §3):
//
//	{
//	  "oauth": {"refresh_token": "...", "client_id": "...", "client_secret": "..."},
//	  "scope": {
//	    "spreadsheet_ids": ["1AbC...", "1XyZ..."],
//	    "drive_folder_ids": ["0B1a..."],
//	    "header_rows": {"1AbC...": 3}
//	  }
//	}
type Config struct {
	OAuth OAuthConfig
	Scope ScopeConfig
}

// OAuthConfig is the credential a customer pastes in for the MVP's
// manual-credential-paste onboarding (docs/07-sheets-adapter-plan.md §1
// Decision 2 — no dashboard consent flow yet).
type OAuthConfig struct {
	RefreshToken string
	ClientID     string
	ClientSecret string
}

// ScopeConfig is the allowlist — "the spec's single most important security
// boundary" (docs/06-sheets-adapter.md §3): the adapter must reject any
// spreadsheet or folder not named here, even though the OAuth token itself
// may be able to reach it (a Google account's own sharing typically extends
// far past what one connector should expose to an MCP agent).
type ScopeConfig struct {
	SpreadsheetIDs []string
	DriveFolderIDs []string

	// HeaderRows overrides the default header row (1) per spreadsheet id —
	// e.g. {"1AbC...": 3} for a sheet with a two-row title banner above its
	// real header. Optional; spec open question 3, resolved in step 5.
	HeaderRows map[string]int
}

// IsSpreadsheetAllowed reports whether spreadsheetID is on the allowlist.
// Every tool must call this on every request against the stored config —
// never against a value cached from connector-creation time — so a
// narrowed allowlist takes effect on the very next call.
func (c *Config) IsSpreadsheetAllowed(spreadsheetID string) bool {
	return contains(c.Scope.SpreadsheetIDs, spreadsheetID)
}

// IsFolderAllowed reports whether folderID is on the allowlist. Same
// per-request re-check requirement as IsSpreadsheetAllowed.
func (c *Config) IsFolderAllowed(folderID string) bool {
	return contains(c.Scope.DriveFolderIDs, folderID)
}

// HeaderRow returns the 1-indexed header row for spreadsheetID: the
// configured override if one exists, otherwise the default of row 1.
func (c *Config) HeaderRow(spreadsheetID string) int {
	if row, ok := c.Scope.HeaderRows[spreadsheetID]; ok && row > 0 {
		return row
	}
	return 1
}

func contains(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// ParseConfig validates and converts a connector's decrypted config map
// into a Config. Errors name the missing/malformed field only — never a
// value — since a value in this map may be the OAuth client secret or
// refresh token and this error can end up in a log line
// (internal/module/connector.Service.CheckHealth logs the error a Checker
// returns).
func ParseConfig(raw map[string]any) (*Config, error) {
	oauthRaw, ok := raw["oauth"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("googlesheets: config.oauth is required")
	}
	scopeRaw, ok := raw["scope"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("googlesheets: config.scope is required")
	}

	oauthCfg, err := parseOAuth(oauthRaw)
	if err != nil {
		return nil, err
	}
	scopeCfg, err := parseScope(scopeRaw)
	if err != nil {
		return nil, err
	}

	return &Config{OAuth: oauthCfg, Scope: scopeCfg}, nil
}

func parseOAuth(raw map[string]any) (OAuthConfig, error) {
	refreshToken, err := requiredString(raw, "refresh_token")
	if err != nil {
		return OAuthConfig{}, err
	}
	clientID, err := requiredString(raw, "client_id")
	if err != nil {
		return OAuthConfig{}, err
	}
	clientSecret, err := requiredString(raw, "client_secret")
	if err != nil {
		return OAuthConfig{}, err
	}
	return OAuthConfig{RefreshToken: refreshToken, ClientID: clientID, ClientSecret: clientSecret}, nil
}

func parseScope(raw map[string]any) (ScopeConfig, error) {
	spreadsheetIDs, err := optionalStringSlice(raw, "spreadsheet_ids")
	if err != nil {
		return ScopeConfig{}, err
	}
	folderIDs, err := optionalStringSlice(raw, "drive_folder_ids")
	if err != nil {
		return ScopeConfig{}, err
	}
	if len(spreadsheetIDs) == 0 && len(folderIDs) == 0 {
		return ScopeConfig{}, fmt.Errorf("googlesheets: config.scope must allowlist at least one spreadsheet or folder")
	}

	headerRows, err := optionalHeaderRows(raw, "header_rows")
	if err != nil {
		return ScopeConfig{}, err
	}

	return ScopeConfig{SpreadsheetIDs: spreadsheetIDs, DriveFolderIDs: folderIDs, HeaderRows: headerRows}, nil
}

func requiredString(raw map[string]any, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", fmt.Errorf("googlesheets: config.oauth.%s is required", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("googlesheets: config.oauth.%s must be a non-empty string", key)
	}
	return s, nil
}

func optionalStringSlice(raw map[string]any, key string) ([]string, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("googlesheets: config.scope.%s must be an array of strings", key)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("googlesheets: config.scope.%s must contain only non-empty strings", key)
		}
		out = append(out, s)
	}
	return out, nil
}

// optionalHeaderRows parses config.scope.header_rows, a
// spreadsheet-id -> row-number map. JSON numbers decode to float64 through
// the map[string]any produced by json.Unmarshal (the connector service's
// openConfig), so values are accepted as float64 or json.Number and
// truncated to int.
func optionalHeaderRows(raw map[string]any, key string) (map[string]int, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("googlesheets: config.scope.%s must be an object", key)
	}
	out := make(map[string]int, len(items))
	for id, raw := range items {
		row, ok := toInt(raw)
		if !ok || row <= 0 {
			return nil, fmt.Errorf("googlesheets: config.scope.%s.%s must be a positive integer", key, id)
		}
		out[id] = row
	}
	return out, nil
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}
