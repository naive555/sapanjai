// Package googlesheets is the connector adapter for type "google_sheets"
// (docs/06-sheets-adapter.md, docs/07-sheets-adapter-decisions.md step 5). It
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
	"slices"

	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
)

// ErrSpreadsheetNotAllowed is returned by every operation reading a
// spreadsheet absent from the connector's allowlist — the spec's single most
// important security boundary (docs/06 §3), enforced on every call against
// the freshly decrypted config, never a cached copy. Wrapped with the
// offending id, which is an allowlist entry rather than a credential and so
// is safe to surface to the model.
var ErrSpreadsheetNotAllowed = errors.New("googlesheets: spreadsheet not on connector allowlist")

// ErrCredentialAmbiguous is returned when a config names both credential
// variants or neither. A sentinel rather than a formatted string so the
// dashboard, which validates the same shape before submitting, can assert
// on it in a test without matching prose.
var ErrCredentialAmbiguous = errors.New("googlesheets: config must carry exactly one of oauth or service_account")

// Config is the parsed, validated shape of a "google_sheets" connector's
// decrypted config (docs/06-sheets-adapter.md §3):
//
//	{
//	  "service_account": {"key_json": "{\"type\":\"service_account\", ...}"},
//	  "scope": {
//	    "spreadsheet_ids": ["1AbC...", "1XyZ..."],
//	    "drive_folder_ids": ["0B1a..."],
//	    "header_rows": {"1AbC...": 3}
//	  }
//	}
//
// The credential half has two shapes. "service_account" is the default and
// the one the dashboard offers first; "oauth" is the original
// refresh-token flow, kept for customers whose Google Workspace admin
// forbids sharing a file outside the domain — which is the only thing that
// makes a service account unusable. Exactly one of the two is ever present:
//
//	{"oauth": {"refresh_token": "...", "client_id": "...", "client_secret": "..."}}
type Config struct {
	Credential Credential
	Scope      ScopeConfig
}

// Credential is a connector's upstream identity. Exactly one field is ever
// non-nil — ParseConfig rejects both and neither — so every reader branches
// on which one is set rather than on a separate discriminator that could
// disagree with the data beside it.
type Credential struct {
	OAuth          *OAuthConfig
	ServiceAccount *ServiceAccountConfig
}

// OAuthConfig is a customer-supplied OAuth client plus a refresh token
// obtained against it, pasted in by hand (docs/07-sheets-adapter-decisions.md
// §1 Decision 2 — no dashboard consent flow yet).
//
// Fragile by construction, which is why it is no longer the default: this
// adapter requests drive.readonly, a Google *restricted* scope, so a
// customer's own OAuth app cannot leave "Testing" publishing status without
// a paid CASA assessment — and Google expires every refresh token a Testing
// app issues after seven days. Prefer ServiceAccountConfig.
type OAuthConfig struct {
	RefreshToken string
	ClientID     string
	ClientSecret string
}

// ServiceAccountConfig is a Google service-account key. The customer shares
// each spreadsheet or folder with Email exactly as they would share it with
// a colleague: no consent screen, no verification, and no refresh token for
// Google to expire.
//
// It also composes with the allowlist rather than duplicating it. Google
// only lets a service account reach what was explicitly shared with it, and
// ScopeConfig independently rejects anything not named there, so a file must
// clear both to be readable.
type ServiceAccountConfig struct {
	// Email is the service account's address — the value a customer shares
	// their spreadsheet with. Derived from the key's client_email. Not a
	// credential: it is safe to display and safe to put in an error.
	Email string

	// jwt is the parsed key, built once by parseServiceAccount so that
	// NewTokenSource cannot fail. Unexported and never serialized — it holds
	// the private key, which must not reach a DTO, a log, or an error string.
	jwt *jwt.Config

	// keyJSON is the raw key material, retained for exactly one purpose:
	// oauth.go's fingerprint digests it so a rotated key evicts the cached
	// TokenSource built from the retired one. Never read for anything else.
	keyJSON string
}

// ScopeConfig is the allowlist: the adapter rejects any spreadsheet or
// folder not named here, even one the connector's credential could reach —
// whichever variant supplied it. Whatever a Google account has accumulated
// access to, or whatever has been shared with a service account, typically
// extends far past what a single connector should expose to an agent.
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
	return slices.Contains(c.Scope.SpreadsheetIDs, spreadsheetID)
}

// IsFolderAllowed reports whether folderID is on the allowlist. Same
// per-request re-check requirement as IsSpreadsheetAllowed.
func (c *Config) IsFolderAllowed(folderID string) bool {
	return slices.Contains(c.Scope.DriveFolderIDs, folderID)
}

// HeaderRow returns the 1-indexed header row for spreadsheetID: the
// configured override if one exists, otherwise the default of row 1.
func (c *Config) HeaderRow(spreadsheetID string) int {
	if row, ok := c.Scope.HeaderRows[spreadsheetID]; ok && row > 0 {
		return row
	}
	return 1
}

// ParseConfig validates and converts a connector's decrypted config map
// into a Config. Errors name the missing/malformed field only — never a
// value — since a value in this map may be the OAuth client secret or
// refresh token and this error can end up in a log line
// (internal/module/connector.Service.CheckHealth logs the error a Checker
// returns).
func ParseConfig(raw map[string]any) (*Config, error) {
	oauthRaw, hasOAuth := raw["oauth"].(map[string]any)
	saRaw, hasServiceAccount := raw["service_account"].(map[string]any)
	if hasOAuth == hasServiceAccount {
		// Both, or neither. A config naming two credentials is as broken as
		// one naming none: there would be no principled way to pick, and
		// picking silently is how a connector ends up authenticating as
		// something other than what its owner last configured.
		return nil, ErrCredentialAmbiguous
	}

	scopeRaw, ok := raw["scope"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("googlesheets: config.scope is required")
	}

	var cred Credential
	if hasOAuth {
		oauthCfg, err := parseOAuth(oauthRaw)
		if err != nil {
			return nil, err
		}
		cred.OAuth = &oauthCfg
	} else {
		saCfg, err := parseServiceAccount(saRaw)
		if err != nil {
			return nil, err
		}
		cred.ServiceAccount = saCfg
	}

	scopeCfg, err := parseScope(scopeRaw)
	if err != nil {
		return nil, err
	}

	return &Config{Credential: cred, Scope: scopeCfg}, nil
}

// parseServiceAccount validates config.service_account and parses the key
// eagerly, so a malformed key is a configuration error surfaced by the
// health check rather than a token-exchange failure surfaced mid-tool-call.
func parseServiceAccount(raw map[string]any) (*ServiceAccountConfig, error) {
	keyJSON, err := requiredString(raw, "service_account", "key_json")
	if err != nil {
		return nil, err
	}

	jwtCfg, err := google.JWTConfigFromJSON([]byte(keyJSON), scopes...)
	if err != nil {
		// err is deliberately dropped, not wrapped. It can quote fragments
		// of the input it failed to parse, and the input is a private key —
		// and this error reaches a log line via
		// connector.Service.CheckHealth. The field name is the whole of
		// what a caller may safely learn.
		return nil, fmt.Errorf("googlesheets: config.service_account.key_json is not a valid Google service-account key")
	}
	if jwtCfg.Email == "" {
		return nil, fmt.Errorf("googlesheets: config.service_account.key_json is missing client_email")
	}

	return &ServiceAccountConfig{Email: jwtCfg.Email, jwt: jwtCfg, keyJSON: keyJSON}, nil
}

func parseOAuth(raw map[string]any) (OAuthConfig, error) {
	refreshToken, err := requiredString(raw, "oauth", "refresh_token")
	if err != nil {
		return OAuthConfig{}, err
	}
	clientID, err := requiredString(raw, "oauth", "client_id")
	if err != nil {
		return OAuthConfig{}, err
	}
	clientSecret, err := requiredString(raw, "oauth", "client_secret")
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

// requiredString reads a mandatory string field out of one config block.
// section names the block for the error message ("oauth",
// "service_account") — the error must be able to say which credential
// variant was malformed, and must still never quote the value, which in
// every one of these fields is a live customer credential.
func requiredString(raw map[string]any, section, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", fmt.Errorf("googlesheets: config.%s.%s is required", section, key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("googlesheets: config.%s.%s must be a non-empty string", section, key)
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
