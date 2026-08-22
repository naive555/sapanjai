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

// sheets_query_rows, the workhorse tool. The scan loop below (queryRows) is
// the point of the file — see its doc comment for the memory-bound argument
// that makes it hold against the spec's 87GB spreadsheet.

const (
	// DefaultPageSize is how many rows to ask Values.Get for per upstream
	// call. queryRows takes pageSize as a parameter so tests can drive the
	// paging logic without allocating real 5,000-row fixtures.
	DefaultPageSize = 5000

	// DefaultScanBudget caps rows evaluated in one call, independent of
	// matches found or sheet size.
	DefaultScanBudget = 50000

	// MaxLimit and DefaultLimit bound the limit argument (docs/06 §4.2).
	MaxLimit     = 200
	DefaultLimit = 50

	// maxResultBytes caps the response body. Checked against the encoded size
	// of the rows actually returned, post-windowing — the scan buffer is
	// already bounded far below this, so it catches wide rows, not deep ones.
	maxResultBytes = 256 * 1024
)

// ErrSheetNotFound is returned when sheet_name names a tab the spreadsheet
// does not have. Wrapped with the offending name; a tab name is not a
// credential.
var ErrSheetNotFound = errors.New("googlesheets: sheet not found in spreadsheet")

// ErrResultTooLarge is returned when the rows to return exceed
// maxResultBytes once encoded. Carries a byte count and no row data.
var ErrResultTooLarge = errors.New("googlesheets: result exceeds the response size cap")

// RateCharger is the seam queryRows charges the connector's bucket through,
// once per page fetched rather than once per tool call. Declared here rather
// than imported so this package keeps its one-way dependency: mcp imports
// googlesheets, never the reverse. *mcp.Service satisfies it as-is. A nil
// RateCharger never charges and never denies.
type RateCharger interface {
	ChargeRateLimit(ctx context.Context, connectorID uuid.UUID, n int) (allowed bool, retryAfter time.Duration, err error)
}

// chargeOne charges a single upstream call against connectorID's bucket,
// returning RateLimitedError when the bucket is empty. op names the calling
// operation for the wrapped infra error. A nil charger is a no-op.
//
// queryRows deliberately does not use this: a mid-scan empty bucket there
// degrades to a partial result rather than failing the call.
func chargeOne(ctx context.Context, charger RateCharger, connectorID uuid.UUID, op string) error {
	if charger == nil {
		return nil
	}
	allowed, retryAfter, err := charger.ChargeRateLimit(ctx, connectorID, 1)
	if err != nil {
		return fmt.Errorf("googlesheets: %s: charge rate limit: %w", op, err)
	}
	if !allowed {
		return &RateLimitedError{RetryAfter: retryAfter}
	}
	return nil
}

// QueryRowsInput is sheets_query_rows' parsed input. Callers apply
// DefaultLimit before ValidateQueryRowsInput bounds-checks it.
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

// ValidateQueryRowsInput checks shape without touching the network, so it
// can run before config decryption. Column names are spreadsheet-specific
// and validated later, once queryRows has the real header row.
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

// QueryRowsOutput is sheets_query_rows' result. It deliberately has no
// unconditional "total" — at 87GB scale that is unbuildable. Total is nil
// unless ScanComplete, in which case it is exact: the scan reached the real
// end of the sheet without stopping early for the match cap, the scan
// budget, or an empty bucket.
type QueryRowsOutput struct {
	// Columns is the effective projection in display order — as given, or
	// full header order when none was requested. Returned so a json-format
	// caller sees the same order the markdown table uses.
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
// scan. queryRows below is the testable core, taking a mocked sheetsAPI and
// configurable page/budget sizes.
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

// retainedPeakHook, when non-nil, receives the retained-match buffer's
// length on every append. Test-only instrumentation, so query_test.go can
// measure the buffer's peak empirically rather than by reading this code.
// Always nil in production.
var retainedPeakHook func(n int)

// queryRows is the bounded scan loop: fetch a page, charge one unit for it
// (never one per tool call), filter in-process retaining at most
// offset+limit+1 matches — the +1 is what sets HasMore without a second
// pass — and stop at whichever comes first of enough matches, end of sheet,
// the scan budget, or an empty bucket.
//
// Peak memory is one page plus the retained window, both bounded
// independent of sheet size. That is the whole reason the loop exists: the
// spec's 87GB spreadsheet defeats anything holding a whole sheet, or even
// every match, in memory. A mid-scan empty bucket ends the scan cleanly with
// ScanComplete: false — a partial answer, never an error.
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
				// Enough matches — the highest-priority stop. Breaks
				// mid-page; whether this was also the sheet's last page is
				// left unknown rather than guessed, so ScanComplete (and
				// therefore Total) is never set on this path.
				stopReason = "cap"
				break pageLoop
			}
		}

		if len(page) < pageSize {
			// A short page is Google's own end-of-sheet signal — the true
			// end, regardless of the scan budget.
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

	if err := checkEncodedSize(out.Rows); err != nil {
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
			// Unreachable: queryRows validates every filter column before
			// scanning starts. Not matching is the safe default anyway.
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
// cell for display. A column absent from a ragged row reads as "", matching
// Google's convention for a row with no trailing cells.
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

// checkEncodedSize enforces the shared ~256KB body cap against the encoded
// size of v. v is whatever the caller has already decided is "the result
// actually returned" — query_rows' windowed Rows, read_range's padded Rows —
// never a larger structure that would count bytes the cap isn't policing.
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
