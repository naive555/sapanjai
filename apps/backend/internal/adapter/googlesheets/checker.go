package googlesheets

import (
	"context"
	"fmt"

	"github.com/sapanjai/backend/internal/module/connector"
)

// Checker implements connector.Checker for type "google_sheets": refresh
// the OAuth token and do one cheap metadata read against the connector's
// allowlist, proving both credential and scope resolve to something real.
//
// Stateless by necessity — Check receives a decrypted config but never a
// connector id, so there is no key to cache a TokenSource under here.
type Checker struct{}

var _ connector.Checker = (*Checker)(nil)

// NewChecker builds a Checker.
func NewChecker() *Checker {
	return &Checker{}
}

// Type implements connector.Checker.
func (c *Checker) Type() connector.Type {
	return connector.TypeGoogleSheets
}

// Check probes the cheapest allowlisted resource: metadata for the first
// allowlisted spreadsheet, or a listing of the first allowlisted Drive
// folder. ParseConfig guarantees at least one exists, so one branch runs.
//
// Per the Checker contract, config is the caller's only copy of a customer
// credential and is never retained past this call. Errors are safe to log:
// the token travels in a header, so no client error carries it in a URL.
func (c *Checker) Check(ctx context.Context, config map[string]any) error {
	cfg, err := ParseConfig(config)
	if err != nil {
		return err
	}

	ts := NewTokenSource(ctx, cfg.OAuth)
	api, err := newClient(ctx, ts, "")
	if err != nil {
		return err
	}

	return probe(ctx, api, cfg)
}

// probe runs the health-check read against api. Split out from Check so
// checker_test.go can drive it against a mocked sheetsAPI instead of a real
// Google client — this package's unit tests never touch the network.
func probe(ctx context.Context, api sheetsAPI, cfg *Config) error {
	if len(cfg.Scope.SpreadsheetIDs) > 0 {
		if _, err := api.SpreadsheetMeta(ctx, cfg.Scope.SpreadsheetIDs[0]); err != nil {
			return fmt.Errorf("googlesheets: health check spreadsheet read: %w", err)
		}
		return nil
	}

	// ParseConfig requires at least one of the two lists to be non-empty,
	// so reaching here means DriveFolderIDs is.
	if _, err := api.ListFiles(ctx, cfg.Scope.DriveFolderIDs[0], ""); err != nil {
		return fmt.Errorf("googlesheets: health check folder list: %w", err)
	}
	return nil
}
