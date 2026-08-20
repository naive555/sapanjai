package googlesheets

import (
	"context"

	"golang.org/x/oauth2"
)

// SpreadsheetSummary is one entry sheets_list_spreadsheets returns — enough
// for the model to recognize a spreadsheet among several similarly named
// ones and pass its id to sheets_describe_spreadsheet next, but not the
// full tab/column shape describe returns.
type SpreadsheetSummary struct {
	// SpreadsheetID is the allowlisted id itself, echoed back so the model
	// doesn't have to cross-reference anything.
	SpreadsheetID string
	// Title is the spreadsheet's display name, empty when Accessible is
	// false.
	Title string
	// Accessible is false when this id is still allowlisted in the
	// connector's config but the OAuth token can no longer read it (a
	// revoked share, a deleted file). Reported per-item rather than failing
	// the whole call — one stale allowlist entry should not hide visibility
	// into every other spreadsheet the connector can still reach.
	Accessible bool
}

// ListSpreadsheetsOutput is sheets_list_spreadsheets' result.
type ListSpreadsheetsOutput struct {
	Spreadsheets []SpreadsheetSummary
}

// ListSpreadsheets returns one summary per spreadsheet in cfg's own
// allowlist (config.scope.spreadsheet_ids) — the allowlist itself is the
// list; this only adds each one's title. There is no separate "is it
// allowed" check here, unlike DescribeSpreadsheet: every id iterated over
// already came from the allowlist, so there is nothing to reject.
func ListSpreadsheets(ctx context.Context, ts oauth2.TokenSource, cfg *Config) (*ListSpreadsheetsOutput, error) {
	api, err := newClient(ctx, ts, "")
	if err != nil {
		return nil, err
	}
	return listSpreadsheets(ctx, api, cfg)
}

// listSpreadsheets is ListSpreadsheets minus client construction, so
// list_test.go can drive it against a mocked sheetsAPI instead of a real
// Google client.
func listSpreadsheets(ctx context.Context, api sheetsAPI, cfg *Config) (*ListSpreadsheetsOutput, error) {
	out := &ListSpreadsheetsOutput{}
	for _, id := range cfg.Scope.SpreadsheetIDs {
		meta, err := api.SpreadsheetMeta(ctx, id)
		if err != nil {
			out.Spreadsheets = append(out.Spreadsheets, SpreadsheetSummary{SpreadsheetID: id, Accessible: false})
			continue
		}
		out.Spreadsheets = append(out.Spreadsheets, SpreadsheetSummary{
			SpreadsheetID: id,
			Title:         meta.Title,
			Accessible:    true,
		})
	}
	return out, nil
}
