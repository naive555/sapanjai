package googlesheets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// This file is docs/07-sheets-adapter-plan.md step 7: sheets_query_rows,
// "the workhorse — the tool the product actually sells." The whole point of
// this file is the scan loop below (queryRows) — see its doc comment for
// the memory-bound argument that makes it load-bearing against the spec's
// 87GB spreadsheet.

const (
	// DefaultPageSize is how many rows QueryRows asks Values.Get for per
	// upstream call — docs/07 step 7: "5,000 rows per Values.Get, tune
	// later." queryRows itself takes pageSize as a parameter so
	// query_test.go can drive the paging/budget logic with a small page
	// size instead of allocating real 5,000-row fixtures.
	DefaultPageSize = 5000

	// DefaultScanBudget is the hard ceiling on rows evaluated in one
	// sheets_query_rows call, independent of matches found or sheet size —
	// docs/07 step 7: "the scan budget (50,000 rows, configurable)."
	DefaultScanBudget = 50000

	// MaxLimit and DefaultLimit are sheets_query_rows' limit bounds
	// (docs/06-sheets-adapter.md §4.2: "int, 1-200, default 50").
	MaxLimit     = 200
	DefaultLimit = 50

	// maxResultBytes is the response body cap (docs/06 §6, docs/07 step 7:
	// "~256KB"). Checked against the JSON-encoded size of the rows actually
	// being returned (post offset/limit windowing) — the retained scan
	// buffer itself is already bounded far below this by the retention cap,
	// so this catches wide rows (many/large columns), not deep ones.
	maxResultBytes = 256 * 1024
)

// ErrSheetNotFound is returned when sheet_name names a tab absent from the
// spreadsheet's own tab list — docs/06-sheets-adapter.md §8
// SHEET_NOT_FOUND, "เรียก sheets_describe_spreadsheet ก่อน". Wrapped with
// the offending name via %w; a tab name is not a credential.
var ErrSheetNotFound = errors.New("googlesheets: sheet not found in spreadsheet")

// ErrResultTooLarge is returned when the rows sheets_query_rows would
// return exceed maxResultBytes once JSON-encoded — docs/06 §8
// RESULT_TOO_LARGE, "แนะนำให้ใส่ columns projection หรือลด limit". Never
// wraps any row data: the error carries only a byte count.
var ErrResultTooLarge = errors.New("googlesheets: result exceeds the response size cap")

// RateCharger is the narrow seam queryRows uses to charge the connector's
// upstream-request rate-limit bucket once per page fetched — docs/07 step
// 7: "not one per tool call." It is satisfied by *mcp.Service.ChargeRateLimit
// as-is (same method name and signature); declaring it here rather than
// importing internal/module/mcp keeps this package's existing one-way
// dependency (mcp imports googlesheets, never the reverse). A nil
// RateCharger is never charged and never denies — QueryRows' caller
// (tools_sheets.go) always supplies the real *mcp.Service, but query_test.go
// can pass nil for tests that don't care about rate limiting at all, or a
// mock for tests that do.
type RateCharger interface {
	ChargeRateLimit(ctx context.Context, connectorID uuid.UUID, n int) (allowed bool, retryAfter time.Duration, err error)
}

// QueryRowsInput is sheets_query_rows' parsed, bounds-checked input.
// Limit/Offset default and bound checking happens in
// ValidateQueryRowsInput; callers (tools_sheets.go) apply DefaultLimit
// before calling it, matching the house style set by
// sheets_describe_spreadsheet's include_sample_rows bound check —
// checked before any config decryption or network call.
type QueryRowsInput struct {
	SpreadsheetID string
	SheetName     string
	Filters       []Filter
	// Columns is the projection: header names to return per row, in the
	// order given. Empty means every header column, in header order.
	Columns []string
	Limit   int
	Offset  int
}

// ValidateQueryRowsInput checks in's shape without touching the network or
// a sheet's actual header row (column names are validated later, once
// queryRows has fetched the real header — a column name is
// spreadsheet-specific and this function must stay networkless so it can
// run before any config decryption, exactly like
// sheets_describe_spreadsheet's include_sample_rows check).
func ValidateQueryRowsInput(in QueryRowsInput) error {
	if in.SpreadsheetID == "" {
		return errors.New("googlesheets: spreadsheet_id is required")
	}
	if in.SheetName == "" {
		return errors.New("googlesheets: sheet_name is required")
	}
	if in.Limit < 1 || in.Limit > MaxLimit {
		return fmt.Errorf("googlesheets: limit must be between 1 and %d (got %d)", MaxLimit, in.Limit)
	}
	if in.Offset < 0 {
		return fmt.Errorf("googlesheets: offset must be >= 0 (got %d)", in.Offset)
	}
	for _, f := range in.Filters {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// QueryRowsOutput is sheets_query_rows' result shape — docs/07 step 7's
// Decision 4, the bounded-scan replacement for spec §4.2's unconditional
// (and, at scale, unbuildable) "total". Total is nil unless ScanComplete is
// true, in which case it is exact: the scan reached the real end of the
// sheet without ever needing to stop early for the match cap, the scan
// budget, or an exhausted rate-limit bucket.
type QueryRowsOutput struct {
	// Columns is the effective projection, in display order: Columns as
	// given, or the sheet's full header order when no projection was
	// requested. Used to render the markdown table; also returned so a
	// json-format caller sees the same order without recomputing it.
	Columns      []string
	Rows         []map[string]string
	Count        int
	Offset       int
	HasMore      bool
	NextOffset   int
	ScannedRows  int
	ScanComplete bool
	// Total is set only when ScanComplete is true.
	Total *int
}

// QueryRows checks cfg's allowlist, builds a client, and runs the bounded
// scan (queryRows) against connectorID's live rate-limit bucket via
// charger. This is the entrypoint tools_sheets.go calls; queryRows below is
// the testable core against a mocked sheetsAPI and a configurable
// page/budget size.
func QueryRows(ctx context.Context, ts oauth2.TokenSource, cfg *Config, connectorID uuid.UUID, charger RateCharger, in QueryRowsInput) (*QueryRowsOutput, error) {
	if !cfg.IsSpreadsheetAllowed(in.SpreadsheetID) {
		return nil, fmt.Errorf("%w: %s", ErrSpreadsheetNotAllowed, in.SpreadsheetID)
	}
	api, err := newClient(ctx, ts, "")
	if err != nil {
		return nil, err
	}
	return queryRows(ctx, api, cfg, connectorID, charger, in, DefaultPageSize, DefaultScanBudget)
}

// retainedPeakHook, when non-nil, is invoked with the retained-match
// buffer's length every time a matched row is appended to it. Test-only
// instrumentation: query_test.go uses it to empirically measure the
// buffer's peak size across a scan spanning many pages and many more
// matches than the retention cap, rather than asserting the cap holds by
// reading this file's code. Always nil in production; nothing outside this
// package can reach it.
var retainedPeakHook func(n int)

// queryRows is sheets_query_rows' core: the bounded scan loop docs/07 step
// 7 specifies, verbatim —
//
//  1. Fetch a bounded page of rows (pageSize per Values.Get) via the
//     sheetsAPI seam.
//  2. Charge charger one unit per page fetched (never per tool call). An
//     empty bucket ends the scan early and cleanly with ScanComplete:
//     false — never as an error; mid-scan exhaustion is a partial answer,
//     not a failure.
//  3. Evaluate filters in-process, retaining at most offset+limit+1
//     matched rows — the +1 is what sets HasMore without a second pass.
//  4. Stop at whichever comes first: enough matches, end of sheet, the
//     scan budget, or an empty bucket.
//
// Peak memory is one fetched page plus the retained window — both bounded
// independent of the sheet's actual size, which is the entire reason this
// loop exists: the spec's 87GB spreadsheet defeats anything that tries to
// hold a whole sheet, or even every match, in memory.
func queryRows(ctx context.Context, api sheetsAPI, cfg *Config, connectorID uuid.UUID, charger RateCharger, in QueryRowsInput, pageSize, scanBudget int) (*QueryRowsOutput, error) {
	meta, err := api.SpreadsheetMeta(ctx, in.SpreadsheetID)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: query rows: %w", err)
	}
	var sheetFound bool
	for _, sh := range meta.Sheets {
		if sh.Title == in.SheetName {
			sheetFound = true
			break
		}
	}
	if !sheetFound {
		return nil, fmt.Errorf("%w: %s", ErrSheetNotFound, in.SheetName)
	}

	headerRow := cfg.HeaderRow(in.SpreadsheetID)
	headerValues, err := api.Values(ctx, in.SpreadsheetID, a1RowRange(in.SheetName, headerRow, headerRow))
	if err != nil {
		return nil, fmt.Errorf("googlesheets: read header row for sheet %q: %w", in.SheetName, err)
	}
	var headers []string
	headerIndex := map[string]int{}
	if len(headerValues) > 0 {
		for i, cell := range headerValues[0] {
			h := cellString(cell)
			headers = append(headers, h)
			if h != "" {
				headerIndex[h] = i
			}
		}
	}

	for _, f := range in.Filters {
		if _, ok := headerIndex[f.Column]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrColumnNotFound, f.Column)
		}
	}
	projection := in.Columns
	if len(projection) == 0 {
		projection = headers
	} else {
		for _, c := range projection {
			if _, ok := headerIndex[c]; !ok {
				return nil, fmt.Errorf("%w: %s", ErrColumnNotFound, c)
			}
		}
	}

	// retainCap is the "+1" trick: retaining one row past the requested
	// window is what lets HasMore be computed for free, without a second
	// scan pass just to check "is there one more."
	retainCap := in.Offset + in.Limit + 1

	var retained []map[string]string
	matchCount := 0
	scannedRows := 0
	stopReason := ""

	currentRow := headerRow + 1
pageLoop:
	for {
		if charger != nil {
			allowed, _, chargeErr := charger.ChargeRateLimit(ctx, connectorID, 1)
			if chargeErr != nil {
				return nil, fmt.Errorf("googlesheets: query rows: charge rate limit: %w", chargeErr)
			}
			if !allowed {
				stopReason = "bucket"
				break
			}
		}

		endRow := currentRow + pageSize - 1
		page, err := api.Values(ctx, in.SpreadsheetID, a1RowRange(in.SheetName, currentRow, endRow))
		if err != nil {
			return nil, fmt.Errorf("googlesheets: fetch rows %d-%d for sheet %q: %w", currentRow, endRow, in.SheetName, err)
		}

		for _, row := range page {
			scannedRows++
			if !matchRow(row, headerIndex, in.Filters) {
				continue
			}
			matchCount++
			if len(retained) < retainCap {
				retained = append(retained, projectRow(row, headerIndex, projection))
				if retainedPeakHook != nil {
					retainedPeakHook(len(retained))
				}
			}
			if matchCount >= retainCap {
				// Enough matches found — the highest-priority stop
				// condition (docs/07 step 7: "enough matches, end of
				// sheet, the scan budget, or an empty bucket," in that
				// order). Stops immediately, mid-page if need be, without
				// evaluating this page's remaining rows: whether this also
				// happened to be the sheet's last page is deliberately
				// left unknown rather than guessed at, so ScanComplete
				// (and therefore Total) is never set on this path.
				stopReason = "cap"
				break pageLoop
			}
		}

		if len(page) < pageSize {
			// A short page is Google's own signal that this was the
			// sheet's last page — the true end, regardless of how it
			// compares to the scan budget.
			stopReason = "end"
			break
		}
		if scannedRows >= scanBudget {
			stopReason = "budget"
			break
		}
		currentRow += pageSize
	}

	scanComplete := stopReason == "end"

	windowEnd := in.Offset + in.Limit
	var rows []map[string]string
	if in.Offset < len(retained) {
		end := windowEnd
		if end > len(retained) {
			end = len(retained)
		}
		rows = retained[in.Offset:end]
	}
	hasMore := len(retained) > windowEnd

	out := &QueryRowsOutput{
		Columns:      projection,
		Rows:         rows,
		Count:        len(rows),
		Offset:       in.Offset,
		HasMore:      hasMore,
		NextOffset:   windowEnd,
		ScannedRows:  scannedRows,
		ScanComplete: scanComplete,
	}
	if scanComplete {
		total := matchCount
		out.Total = &total
	}

	if err := checkResultSize(out); err != nil {
		return nil, err
	}

	return out, nil
}

// matchRow reports whether row (raw, ragged, as Google returns it) matches
// every filter — filters AND together (docs/06 §4.2).
func matchRow(row []any, headerIndex map[string]int, filters []Filter) bool {
	for _, f := range filters {
		idx, ok := headerIndex[f.Column]
		if !ok {
			// Unreachable in production: queryRows validates every filter
			// column against headerIndex before scanning ever starts. Not
			// matching is the safe default if this is ever reached anyway.
			return false
		}
		var cell any
		if idx < len(row) {
			cell = row[idx]
		}
		if !f.Matches(cell) {
			return false
		}
	}
	return true
}

// projectRow extracts columns from row by header index, stringifying each
// cell with cellString — the same one-way, display-only formatting
// describe.go uses for sample rows. A column absent from a ragged row
// (shorter than the header) reads as "", matching Google's own convention
// for a row that simply has no trailing cells.
func projectRow(row []any, headerIndex map[string]int, columns []string) map[string]string {
	out := make(map[string]string, len(columns))
	for _, c := range columns {
		idx, ok := headerIndex[c]
		if !ok || idx >= len(row) {
			out[c] = ""
			continue
		}
		out[c] = cellString(row[idx])
	}
	return out
}

// checkResultSize enforces the ~256KB response body cap (docs/06 §6) against
// the rows actually being returned (post offset/limit windowing) — the
// scan's retained buffer is already far smaller than this by construction,
// so this guards against wide rows (many or large columns), not deep scans.
func checkResultSize(out *QueryRowsOutput) error {
	return checkEncodedSize(out.Rows)
}

// checkEncodedSize enforces the shared ~256KB response body cap (docs/06
// §6, docs/07 step 7) against the JSON-encoded size of v — generalized out
// of checkResultSize (docs/07 step 8: "generalize it ... rather than
// duplicating the constant") so sheets_read_range's readrange.go can reuse
// the exact same cap and constant instead of hand-rolling a second one.
// v is whatever the caller has already decided is "the result actually
// being returned" — sheets_query_rows' windowed Rows, sheets_read_range's
// padded Rows — never a larger structure that would double-count bytes the
// cap isn't meant to police.
func checkEncodedSize(v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("googlesheets: encode result: %w", err)
	}
	if len(encoded) > maxResultBytes {
		return fmt.Errorf("%w: %d bytes", ErrResultTooLarge, len(encoded))
	}
	return nil
}
