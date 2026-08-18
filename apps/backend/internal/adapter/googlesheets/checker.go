package googlesheets

import (
	"context"
	"fmt"

	"github.com/sapanjai/backend/internal/module/connector"
)

// Checker implements connector.Checker for type "google_sheets"
// (internal/module/connector/health.go). It turns the 501
// HEALTH_CHECK_UNSUPPORTED stub into a real probe: refresh the OAuth token
// and perform one cheap metadata read against the connector's own
// allowlist, proving both the credential and the scope actually resolve to
// something reachable.
//
// Checker carries no shared state: connector.Checker.Check receives only
// the connector's decrypted config, never a connector id, so there is no id
// to key a cache by here — see oauth.go's TokenSourceCache doc comment,
// which is the cache step 6+'s per-session tool calls (which do have a
// connector id) are expected to use instead.
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

// Check implements connector.Checker. It parses config, builds a
// single-use OAuth token source and Google API client, and probes the
// cheapest available upstream resource: metadata for the first allowlisted
// spreadsheet, or — if no spreadsheet is allowlisted — a listing of the
// first allowlisted Drive folder. ParseConfig guarantees the allowlist
// names at least one of the two, so exactly one branch runs.
//
// Per the Checker contract: config is the caller's only copy of a customer
// credential and is never retained past this call, and any error returned
// is safe to log (Google API client errors carry no request URL with the
// token in it — the token travels in a header, not the URL).
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
