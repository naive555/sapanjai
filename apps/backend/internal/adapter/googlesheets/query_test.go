package googlesheets

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- shared test fixtures ----

// rowSource is a sheetsAPI-shaped data generator for query_test.go: it
// synthesizes rows on the fly from a header and a genRow callback rather
// than pre-building a giant slice, so tests that scan far more rows than
// any real cap (millions, conceptually) stay fast and cheap. It also
// records every a1Range it was asked for, which the "filter value never
// reaches upstream" test inspects directly.
type rowSource struct {
	t      *testing.T
	header []string
	genRow func(n int) []any // n is the 1-indexed data row (1 = first row after the header)
	// maxRow bounds the dataset to exactly maxRow data rows: a page
	// request reaching past it returns fewer rows than asked for, the same
	// signal a real short final page gives. Zero means unbounded — the
	// generator always returns a full page, used by tests that must never
	// let the scan reach a natural "end of sheet" on its own (budget,
	// retention-cap, and rate-limit tests all want the scan to stop for a
	// specific other reason, not this one).
	maxRow int
	calls  []string
}

func (rs *rowSource) spreadsheetMeta(ctx context.Context, spreadsheetID string) (*Meta, error) {
	return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data"}}}, nil
}

func (rs *rowSource) values(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
	rs.calls = append(rs.calls, a1Range)
	row1, row2, err := parseA1RowRange(a1Range)
	if err != nil {
		rs.t.Fatalf("unparseable a1Range %q: %v", a1Range, err)
	}
	if row1 == 1 && row2 == 1 {
		headerRow := make([]any, len(rs.header))
		for i, h := range rs.header {
			headerRow[i] = h
		}
		return [][]any{headerRow}, nil
	}
	out := make([][]any, 0, row2-row1+1)
	for r := row1; r <= row2; r++ {
		n := r - 1
		if rs.maxRow > 0 && n > rs.maxRow {
			break
		}
		out = append(out, rs.genRow(n))
	}
	return out, nil
}

func newRowSourceAPI(rs *rowSource) sheetsAPI {
	return &mockSheetsAPI{
		t:               rs.t,
		spreadsheetMeta: rs.spreadsheetMeta,
		values:          rs.values,
	}
}

// parseA1RowRange reverses a1RowRange's "'Sheet'!row1:row2" shape (doubled
// internal quotes undone) for test assertions.
func parseA1RowRange(a1Range string) (row1, row2 int, err error) {
	bang := strings.LastIndex(a1Range, "!")
	if bang < 0 {
		return 0, 0, errors.New("no '!' in range")
	}
	rowsPart := a1Range[bang+1:]
	parts := strings.SplitN(rowsPart, ":", 2)
	if len(parts) != 2 {
		return 0, 0, errors.New("no ':' in row span")
	}
	row1, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	row2, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return row1, row2, nil
}

// fakeCharger is a RateCharger test double: allows the first allowUpTo
// charges, denies every one after.
type fakeCharger struct {
	allowUpTo int
	calls     int
}

func (c *fakeCharger) ChargeRateLimit(ctx context.Context, connectorID uuid.UUID, n int) (bool, time.Duration, error) {
	c.calls++
	if c.calls > c.allowUpTo {
		return false, 30 * time.Second, nil
	}
	return true, 0, nil
}

func idStatusCfg() *Config {
	return &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-1"}}}
}

// ---- ValidateQueryRowsInput ----

func TestValidateQueryRowsInput(t *testing.T) {
	base := QueryRowsInput{SpreadsheetID: "s1", SheetName: "Data", Limit: DefaultLimit, Offset: 0}

	if err := ValidateQueryRowsInput(base); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}

	missingID := base
	missingID.SpreadsheetID = ""
	if err := ValidateQueryRowsInput(missingID); err == nil {
		t.Error("want error for missing spreadsheet_id")
	}

	missingSheet := base
	missingSheet.SheetName = ""
	if err := ValidateQueryRowsInput(missingSheet); err == nil {
		t.Error("want error for missing sheet_name")
	}

	negativeOffset := base
	negativeOffset.Offset = -1
	if err := ValidateQueryRowsInput(negativeOffset); err == nil {
		t.Error("want error for negative offset")
	}

	badFilter := base
	badFilter.Filters = []Filter{{Column: "", Op: OpEq, Value: "x"}}
	if err := ValidateQueryRowsInput(badFilter); err == nil {
		t.Error("want error for an invalid filter")
	}
}

// TestValidateQueryRowsInput_LimitBounds is the plan's required test:
// limit=201 must be rejected. Also checks the boundaries around it.
func TestValidateQueryRowsInput_LimitBounds(t *testing.T) {
	cases := []struct {
		limit   int
		wantErr bool
	}{
		{0, true},
		{-1, true},
		{1, false},
		{MaxLimit, false},
		{MaxLimit + 1, true}, // limit=201
		{500, true},
	}
	for _, c := range cases {
		in := QueryRowsInput{SpreadsheetID: "s1", SheetName: "Data", Limit: c.limit}
		err := ValidateQueryRowsInput(in)
		if (err != nil) != c.wantErr {
			t.Errorf("limit=%d: err = %v, wantErr %v", c.limit, err, c.wantErr)
		}
	}
}

// ---- allowlist / not-found errors ----

func TestQueryRows_RejectsNonAllowlistedIDWithoutTouchingTheNetwork(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"1AbCAllowed"}}}
	in := QueryRowsInput{SpreadsheetID: "1XyZNotAllowed", SheetName: "Data", Limit: DefaultLimit}
	// ts is nil: reaching newClient before the allowlist check would panic.
	_, err := QueryRows(context.Background(), nil, cfg, uuid.New(), nil, in)
	if !errors.Is(err, ErrSpreadsheetNotAllowed) {
		t.Fatalf("err = %v, want ErrSpreadsheetNotAllowed", err)
	}
}

func TestQueryRows_UnknownSheetName(t *testing.T) {
	cfg := idStatusCfg()
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Contracts"}}}, nil
		},
	}
	in := QueryRowsInput{SpreadsheetID: "sheet-1", SheetName: "NoSuchTab", Limit: DefaultLimit}
	_, err := queryRows(context.Background(), api, cfg, uuid.New(), nil, in, DefaultPageSize, DefaultScanBudget)
	if !errors.Is(err, ErrSheetNotFound) {
		t.Fatalf("err = %v, want ErrSheetNotFound", err)
	}
}

// TestQueryRows_UnknownColumn is the plan's required test: a filter or
// projection column absent from the header row must fail with
// ErrColumnNotFound before any data page is ever fetched.
func TestQueryRows_UnknownColumn(t *testing.T) {
	t.Run("in a filter", func(t *testing.T) {
		rs := &rowSource{t: t, header: []string{"id", "status"}}
		api := newRowSourceAPI(rs)
		in := QueryRowsInput{
			SpreadsheetID: "sheet-1", SheetName: "Data", Limit: DefaultLimit,
			Filters: []Filter{{Column: "does_not_exist", Op: OpEq, Value: "x"}},
		}
		_, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), nil, in, 10, DefaultScanBudget)
		if !errors.Is(err, ErrColumnNotFound) {
			t.Fatalf("err = %v, want ErrColumnNotFound", err)
		}
		for _, c := range rs.calls {
			if c != "'Data'!1:1" {
				t.Errorf("unexpected data page fetched before the column check failed: %q", c)
			}
		}
	})

	t.Run("in a projection", func(t *testing.T) {
		rs := &rowSource{t: t, header: []string{"id", "status"}}
		api := newRowSourceAPI(rs)
		in := QueryRowsInput{
			SpreadsheetID: "sheet-1", SheetName: "Data", Limit: DefaultLimit,
			Columns: []string{"id", "not_a_real_column"},
		}
		_, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), nil, in, 10, DefaultScanBudget)
		if !errors.Is(err, ErrColumnNotFound) {
			t.Fatalf("err = %v, want ErrColumnNotFound", err)
		}
	})
}

// ---- pagination math ----

// TestQueryRows_HasMoreAndNextOffset_PageBoundary is the plan's required
// test. A 7-row matching dataset (no filters — every row matches
// trivially) is queried at three offsets to exercise has_more/next_offset
// at, before, and after the true end of the matched set.
func TestQueryRows_HasMoreAndNextOffset_PageBoundary(t *testing.T) {
	rs := &rowSource{
		t: t, header: []string{"id"}, maxRow: 7,
		genRow: func(n int) []any { return []any{"row-" + strconv.Itoa(n)} },
	}
	api := newRowSourceAPI(rs)
	cfg := idStatusCfg()

	run := func(offset, limit int) *QueryRowsOutput {
		in := QueryRowsInput{SpreadsheetID: "sheet-1", SheetName: "Data", Limit: limit, Offset: offset}
		out, err := queryRows(context.Background(), api, cfg, uuid.New(), nil, in, 100, DefaultScanBudget)
		if err != nil {
			t.Fatalf("offset=%d limit=%d: %v", offset, limit, err)
		}
		return out
	}

	// Only 7 matching rows exist upstream, but the generator never
	// terminates on its own — the scan must stop by exhausting matches
	// within retainCap at offset=0, not by "end of sheet", because 7 total
	// matches (no filters, so every fetched row is a "match") exceeds
	// retainCap of 4. This is intentional: it proves has_more/next_offset
	// come purely from the retained window, not from knowing the sheet's
	// true size.
	first := run(0, 3)
	if first.Count != 3 || !first.HasMore || first.NextOffset != 3 {
		t.Errorf("offset=0,limit=3: count=%d hasMore=%v nextOffset=%d, want 3/true/3", first.Count, first.HasMore, first.NextOffset)
	}
	if first.ScanComplete {
		t.Error("offset=0,limit=3: ScanComplete = true, want false (stopped by the match cap, not the sheet's end)")
	}
	if first.Total != nil {
		t.Errorf("offset=0,limit=3: Total = %v, want nil when ScanComplete is false", first.Total)
	}

	mid := run(3, 3)
	if mid.Count != 3 || !mid.HasMore || mid.NextOffset != 6 {
		t.Errorf("offset=3,limit=3: count=%d hasMore=%v nextOffset=%d, want 3/true/6", mid.Count, mid.HasMore, mid.NextOffset)
	}

	last := run(6, 3)
	if last.Count != 1 || last.HasMore {
		t.Errorf("offset=6,limit=3: count=%d hasMore=%v, want 1/false", last.Count, last.HasMore)
	}
	if !last.ScanComplete {
		t.Error("offset=6,limit=3: ScanComplete = false, want true — retainCap(10) exceeds the 7 actual matches, so the scan should run to the sheet's real end")
	}
	if last.Total == nil || *last.Total != 7 {
		t.Errorf("offset=6,limit=3: Total = %v, want *7", last.Total)
	}
}

// ---- scan budget / rate limit / retention ----

// TestQueryRows_ScanBudgetExceeded_NoTotal is the plan's required test: a
// sheet larger than the scan budget must stop with ScanComplete: false and
// no Total, never an error.
func TestQueryRows_ScanBudgetExceeded_NoTotal(t *testing.T) {
	const pageSize = 5
	const scanBudget = 20

	rs := &rowSource{t: t, header: []string{"id"}, genRow: func(n int) []any { return []any{"row-" + strconv.Itoa(n)} }}
	api := newRowSourceAPI(rs)

	// retainCap (1001) is deliberately far above scanBudget so the scan
	// stops for "budget", not "enough matches" — isolating the behavior
	// this test is actually about.
	in := QueryRowsInput{SpreadsheetID: "sheet-1", SheetName: "Data", Limit: 1000, Offset: 0}
	out, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), nil, in, pageSize, scanBudget)
	if err != nil {
		t.Fatalf("queryRows: %v", err)
	}
	if out.ScanComplete {
		t.Error("ScanComplete = true, want false: the generator never runs out of rows on its own")
	}
	if out.Total != nil {
		t.Errorf("Total = %v, want nil when the scan budget was hit before the sheet's real end", out.Total)
	}
	if out.ScannedRows < scanBudget {
		t.Errorf("ScannedRows = %d, want >= %d", out.ScannedRows, scanBudget)
	}
}

// TestQueryRows_ManyMatches_RetentionBounded is the plan's required
// memory-bound test, and it is an actual measurement, not an assertion
// about the code: retainedPeakHook is invoked with the retained buffer's
// real length every time a row is appended, and this test drives a scan
// whose *total* available matches (conceptually unbounded — the generator
// never terminates) vastly exceeds offset+limit+1, then asserts the peak
// observed length never exceeded that cap.
func TestQueryRows_ManyMatches_RetentionBounded(t *testing.T) {
	rs := &rowSource{t: t, header: []string{"id"}, genRow: func(n int) []any { return []any{"row-" + strconv.Itoa(n)} }}
	api := newRowSourceAPI(rs)

	const offset, limit = 0, 10
	wantCap := offset + limit + 1

	peak := 0
	retainedPeakHook = func(n int) {
		if n > peak {
			peak = n
		}
	}
	t.Cleanup(func() { retainedPeakHook = nil })

	in := QueryRowsInput{SpreadsheetID: "sheet-1", SheetName: "Data", Limit: limit, Offset: offset}
	out, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), nil, in, 50, DefaultScanBudget)
	if err != nil {
		t.Fatalf("queryRows: %v", err)
	}
	if peak != wantCap {
		t.Errorf("peak retained rows measured = %d, want exactly %d (offset+limit+1)", peak, wantCap)
	}
	if !out.HasMore {
		t.Error("HasMore = false, want true: far more matches exist than the retention cap")
	}
	if out.ScanComplete {
		t.Error("ScanComplete = true, want false: the scan stopped at the match cap, not the sheet's real end")
	}
}

// TestQueryRows_RateLimitExhaustedMidScan_EndsCleanly is the plan's
// required test: an empty rate-limit bucket mid-scan must end the scan with
// ScanComplete: false and no error — never IsError, never a Go error.
func TestQueryRows_RateLimitExhaustedMidScan_EndsCleanly(t *testing.T) {
	const pageSize = 5

	rs := &rowSource{t: t, header: []string{"id"}, genRow: func(n int) []any { return []any{"row-" + strconv.Itoa(n)} }}
	api := newRowSourceAPI(rs)
	charger := &fakeCharger{allowUpTo: 3} // 3 pages' worth before the bucket runs dry

	in := QueryRowsInput{SpreadsheetID: "sheet-1", SheetName: "Data", Limit: 1000, Offset: 0}
	out, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), charger, in, pageSize, DefaultScanBudget)
	if err != nil {
		t.Fatalf("queryRows returned an error for a mid-scan rate-limit exhaustion, want a clean partial result: %v", err)
	}
	if out.ScanComplete {
		t.Error("ScanComplete = true, want false: the bucket ran dry before the sheet's real end")
	}
	if out.Total != nil {
		t.Errorf("Total = %v, want nil", out.Total)
	}
	if charger.calls != 4 {
		// 3 allowed charges (pages actually fetched) plus the 4th charge
		// attempt that was denied and stopped the scan before a 4th fetch.
		t.Errorf("charger.calls = %d, want 4 (3 allowed + 1 denied)", charger.calls)
	}
	if got := len(rs.calls); got != 4 {
		// 1 header fetch + 3 allowed data pages; the 4th (denied) charge
		// must never have reached a Values() call at all.
		t.Errorf("rs.calls (upstream Values fetches) = %d, want 4 (1 header + 3 data pages)", got)
	}
	if wantScanned := pageSize * 3; out.ScannedRows != wantScanned {
		t.Errorf("ScannedRows = %d, want %d (only the 3 fetched pages)", out.ScannedRows, wantScanned)
	}
}

// TestQueryRows_FilterValueNeverReachesUpstream is the plan's required
// test at the scan-loop level (filter_test.go already proves it at the
// single-comparison level): a formula-shaped filter value must never
// appear in any argument this package hands to the sheetsAPI seam — the
// only thing ever sent upstream is a1-notation row ranges built from the
// sheet name and row numbers.
func TestQueryRows_FilterValueNeverReachesUpstream(t *testing.T) {
	const formula = "=IMPORTRANGE(\"evil\", \"Sheet1!A1\")"

	rs := &rowSource{
		t:      t,
		header: []string{"id", "notes"},
		genRow: func(n int) []any {
			if n == 2 {
				return []any{"row-2", formula}
			}
			return []any{"row-" + strconv.Itoa(n), "ordinary text"}
		},
	}
	api := newRowSourceAPI(rs)

	in := QueryRowsInput{
		SpreadsheetID: "sheet-1", SheetName: "Data", Limit: DefaultLimit,
		Filters: []Filter{{Column: "notes", Op: OpEq, Value: formula}},
	}
	out, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), nil, in, 5, DefaultScanBudget)
	if err != nil {
		t.Fatalf("queryRows: %v", err)
	}
	if out.Count != 1 {
		t.Fatalf("Count = %d, want 1 (only row-2 carries the literal formula text)", out.Count)
	}
	if out.Rows[0]["id"] != "row-2" {
		t.Errorf("matched row = %v, want row-2", out.Rows[0])
	}
	for _, call := range rs.calls {
		if strings.Contains(call, "IMPORTRANGE") {
			t.Fatalf("the filter value reached an upstream call: %q", call)
		}
	}
}

// ---- projection ----

func TestQueryRows_ProjectionOrderAndRaggedRows(t *testing.T) {
	rs := &rowSource{
		t:      t,
		header: []string{"id", "status", "notes"},
		maxRow: 2,
		genRow: func(n int) []any {
			if n == 1 {
				return []any{"row-1", "draft"} // ragged: no third cell
			}
			return []any{"row-2", "draft", "hello"}
		},
	}
	api := newRowSourceAPI(rs)

	in := QueryRowsInput{
		SpreadsheetID: "sheet-1", SheetName: "Data", Limit: DefaultLimit,
		Columns: []string{"notes", "id"}, // deliberately reordered
	}
	out, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), nil, in, 5, 10)
	if err != nil {
		t.Fatalf("queryRows: %v", err)
	}
	if len(out.Columns) != 2 || out.Columns[0] != "notes" || out.Columns[1] != "id" {
		t.Errorf("Columns = %v, want [notes id] preserving the requested order", out.Columns)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("Rows = %d, want 2", len(out.Rows))
	}
	if out.Rows[0]["notes"] != "" || out.Rows[0]["id"] != "row-1" {
		t.Errorf("Rows[0] = %v, want notes empty (ragged row) and id row-1", out.Rows[0])
	}
	if out.Rows[1]["notes"] != "hello" || out.Rows[1]["id"] != "row-2" {
		t.Errorf("Rows[1] = %v, want notes=hello id=row-2", out.Rows[1])
	}
	// A projected-out column must not leak into the row map at all.
	if _, ok := out.Rows[0]["status"]; ok {
		t.Error("Rows[0] contains \"status\", which was not in the projection")
	}
}

// ---- result size cap ----

func TestQueryRows_ResultTooLarge(t *testing.T) {
	big := strings.Repeat("x", 2000)
	rs := &rowSource{
		t:      t,
		header: []string{"id", "blob"},
		genRow: func(n int) []any { return []any{"row-" + strconv.Itoa(n), big} },
	}
	api := newRowSourceAPI(rs)

	// 200 rows * ~2KB each comfortably exceeds the 256KB cap.
	in := QueryRowsInput{SpreadsheetID: "sheet-1", SheetName: "Data", Limit: MaxLimit, Offset: 0}
	_, err := queryRows(context.Background(), api, idStatusCfg(), uuid.New(), nil, in, 500, DefaultScanBudget)
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("err = %v, want ErrResultTooLarge", err)
	}
}
