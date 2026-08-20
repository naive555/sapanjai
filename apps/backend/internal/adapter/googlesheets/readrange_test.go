package googlesheets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// attemptReadRange mimics tools_sheets.go's registerReadRange handler:
// parse+validate first (networkless), only reaching readRange (and
// therefore api) if that succeeds. api's methods are guarded by
// mockSheetsAPI itself (an unset method calls t.Fatal), so any test that
// expects ValidateReadRangeInput to reject its input can pass an api with
// every field left nil and get an automatic, unambiguous failure if the
// code path ever reaches the network after all — this is how the
// "never touches Values" assertions below are enforced, not by manually
// inspecting a call log.
func attemptReadRange(t *testing.T, api sheetsAPI, spreadsheetID, rangeStr string) (*ReadRangeOutput, error) {
	t.Helper()
	in := ReadRangeInput{SpreadsheetID: spreadsheetID, RangeStr: rangeStr}
	parsed, err := ValidateReadRangeInput(in)
	if err != nil {
		return nil, err
	}
	return readRange(context.Background(), api, uuid.New(), nil, in, parsed)
}

// untouchedAPI is a mockSheetsAPI with every method left unset — any test
// using it asserts, by construction, that the code under test never
// reaches the network at all.
func untouchedAPI(t *testing.T) sheetsAPI {
	return &mockSheetsAPI{t: t}
}

// ---- ParseA1Range ----

func TestReadRange_ParseA1Range_QuotedSheetNameWithSpacesAndEmbeddedQuote(t *testing.T) {
	got, err := ParseA1Range(`'It''s Contract Data'!A1:D10`)
	if err != nil {
		t.Fatalf("ParseA1Range: %v", err)
	}
	if !got.HasSheetName || got.SheetName != "It's Contract Data" {
		t.Errorf("SheetName = %q (HasSheetName=%v), want %q (true)", got.SheetName, got.HasSheetName, "It's Contract Data")
	}
	if got.StartCol != 0 || got.EndCol != 3 || got.StartRow != 1 || got.EndRow != 10 {
		t.Errorf("bounds = col[%d,%d] row[%d,%d], want col[0,3] row[1,10]", got.StartCol, got.EndCol, got.StartRow, got.EndRow)
	}
}

func TestReadRange_ParseA1Range_SheetNameOmitted(t *testing.T) {
	got, err := ParseA1Range("A1:D10")
	if err != nil {
		t.Fatalf("ParseA1Range: %v", err)
	}
	if got.HasSheetName {
		t.Errorf("HasSheetName = true, want false for a range with no sheet prefix")
	}
	if got.SheetName != "" {
		t.Errorf("SheetName = %q, want empty until readRange resolves it", got.SheetName)
	}
}

func TestReadRange_ParseA1Range_LowercaseColumns(t *testing.T) {
	got, err := ParseA1Range("a1:d10")
	if err != nil {
		t.Fatalf("ParseA1Range: %v", err)
	}
	if got.StartCol != 0 || got.EndCol != 3 {
		t.Errorf("cols = [%d,%d], want [0,3] (lowercase a-d)", got.StartCol, got.EndCol)
	}
}

func TestReadRange_ParseA1Range_MultiLetterColumns(t *testing.T) {
	got, err := ParseA1Range("Data!AA1:AB10")
	if err != nil {
		t.Fatalf("ParseA1Range: %v", err)
	}
	// AA -> 26, AB -> 27 (0-based), matching columnLetter's own convention
	// (0="A", 25="Z", 26="AA").
	if got.StartCol != 26 || got.EndCol != 27 {
		t.Errorf("cols = [%d,%d], want [26,27] (AA, AB)", got.StartCol, got.EndCol)
	}
}

func TestReadRange_ParseA1Range_ReversedBoundsNormalized(t *testing.T) {
	got, err := ParseA1Range("Data!D10:A1")
	if err != nil {
		t.Fatalf("ParseA1Range: %v", err)
	}
	if got.StartCol != 0 || got.EndCol != 3 || got.StartRow != 1 || got.EndRow != 10 {
		t.Errorf("bounds = col[%d,%d] row[%d,%d], want the normalized ascending col[0,3] row[1,10]",
			got.StartCol, got.EndCol, got.StartRow, got.EndRow)
	}
}

func TestReadRange_ParseA1Range_RowOnlyAllowed(t *testing.T) {
	// docs/07 step 8: "1:100 is bounded in rows so it's fine" — row-only
	// ranges are accepted at parse time; their column span is resolved
	// later, against the sheet's real width, by readRange.
	got, err := ParseA1Range("Data!1:100")
	if err != nil {
		t.Fatalf("ParseA1Range: %v", err)
	}
	if got.StartCol != -1 || got.EndCol != -1 {
		t.Errorf("cols = [%d,%d], want [-1,-1] (unresolved) for a row-only range", got.StartCol, got.EndCol)
	}
	if got.StartRow != 1 || got.EndRow != 100 {
		t.Errorf("rows = [%d,%d], want [1,100]", got.StartRow, got.EndRow)
	}
	if got.ColSpan() != -1 || got.CellCount() != -1 {
		t.Errorf("ColSpan/CellCount = %d/%d, want -1/-1 before resolution", got.ColSpan(), got.CellCount())
	}
}

// TestReadRange_ParseA1Range_UnboundedRejected is the plan's required
// test: a bare sheet name and a column-only range are both rejected as
// ErrUnboundedRange, before touching any api.
func TestReadRange_ParseA1Range_UnboundedRejected(t *testing.T) {
	cases := []string{"Contracts", "Contracts!A:D", "A:D"}
	for _, rangeStr := range cases {
		t.Run(rangeStr, func(t *testing.T) {
			_, err := ParseA1Range(rangeStr)
			if !errors.Is(err, ErrUnboundedRange) {
				t.Fatalf("ParseA1Range(%q) err = %v, want ErrUnboundedRange", rangeStr, err)
			}
			if !strings.Contains(err.Error(), "row bounds") {
				t.Errorf("error message = %q, want it to name the fix (explicit row bounds)", err.Error())
			}
		})
	}
}

// ---- ValidateReadRangeInput / pre-fetch caps ----

// TestReadRange_OverCapRejected is the plan's required test: both a row
// span over MaxReadRangeRows and a cell count over MaxReadRangeCells are
// rejected before any network call — untouchedAPI enforces the "never
// touches the network" half of that claim.
func TestReadRange_OverCapRejected(t *testing.T) {
	t.Run("row span", func(t *testing.T) {
		// 1001 rows, 1 column: over MaxReadRangeRows, comfortably under
		// MaxReadRangeCells — isolates the row-span check.
		_, err := attemptReadRange(t, untouchedAPI(t), "sheet-1", "Data!A1:A1001")
		if !errors.Is(err, ErrRangeTooLarge) {
			t.Fatalf("err = %v, want ErrRangeTooLarge", err)
		}
		if !strings.Contains(err.Error(), "narrow the range") {
			t.Errorf("error message = %q, want it to name the fix", err.Error())
		}
	})

	t.Run("cell count", func(t *testing.T) {
		// 500 rows * 50 columns = 25,000 cells: over MaxReadRangeCells,
		// under MaxReadRangeRows — isolates the cell-count check.
		_, err := attemptReadRange(t, untouchedAPI(t), "sheet-1", "Data!A1:AX500")
		if !errors.Is(err, ErrRangeTooLarge) {
			t.Fatalf("err = %v, want ErrRangeTooLarge", err)
		}
	})
}

// TestReadRange_FormulaAndSecondRangeSmuggling_RejectedByParser is the
// plan's required guardrail test: a range string that tries to smuggle a
// formula or a second spreadsheet/sheet reference must be rejected by
// ParseA1Range (via ValidateReadRangeInput) before it ever reaches an api
// call — untouchedAPI's t.Fatal-on-any-call guard is the assertion that
// Values (and SpreadsheetMeta) were never invoked.
func TestReadRange_FormulaAndSecondRangeSmuggling_RejectedByParser(t *testing.T) {
	cases := []string{
		`=IMPORTRANGE("other","A1")`,
		"Contracts!A1:B2,Other!A1:B2",
		"Contracts!A1:B2,Other!A1:B2!A1:B2",
	}
	for _, rangeStr := range cases {
		t.Run(rangeStr, func(t *testing.T) {
			_, err := attemptReadRange(t, untouchedAPI(t), "sheet-1", rangeStr)
			if err == nil {
				t.Fatalf("attemptReadRange(%q): want an error, got success", rangeStr)
			}
		})
	}
}

// ---- allowlist ----

func TestReadRange_RejectsNonAllowlistedIDWithoutTouchingTheNetwork(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"1AbCAllowed"}}}
	parsed, err := ParseA1Range("Data!A1:D10")
	if err != nil {
		t.Fatalf("ParseA1Range: %v", err)
	}
	in := ReadRangeInput{SpreadsheetID: "1XyZNotAllowed", RangeStr: "Data!A1:D10"}
	// ts is nil: reaching newClient before the allowlist check would panic.
	_, err = ReadRange(context.Background(), nil, cfg, uuid.New(), nil, in, parsed)
	if !errors.Is(err, ErrSpreadsheetNotAllowed) {
		t.Fatalf("err = %v, want ErrSpreadsheetNotAllowed", err)
	}
}

// ---- sheet resolution / not-found ----

func TestReadRange_UnknownSheetName(t *testing.T) {
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Contracts"}}}, nil
		},
	}
	_, err := attemptReadRange(t, api, "sheet-1", "NoSuchTab!A1:D10")
	if !errors.Is(err, ErrSheetNotFound) {
		t.Fatalf("err = %v, want ErrSheetNotFound", err)
	}
}

func TestReadRange_SheetNameOmitted_ResolvesToFirstTab(t *testing.T) {
	var gotRange string
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Contracts", ColumnCount: 4}, {Title: "Archive"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			gotRange = a1Range
			return [][]any{{"a", "b"}}, nil
		},
	}
	out, err := attemptReadRange(t, api, "sheet-1", "A1:B2")
	if err != nil {
		t.Fatalf("attemptReadRange: %v", err)
	}
	if out.SheetName != "Contracts" {
		t.Errorf("SheetName = %q, want %q (the spreadsheet's first tab)", out.SheetName, "Contracts")
	}
	if out.Range != "'Contracts'!A1:B2" {
		t.Errorf("Range = %q, want the resolved range with the sheet name filled in", out.Range)
	}
	if gotRange != "'Contracts'!A1:B2" {
		t.Errorf("Values called with %q, want %q", gotRange, "'Contracts'!A1:B2")
	}
}

// ---- row-only range column resolution ----

func TestReadRange_RowOnlyRange_ResolvesColumnsAgainstSheetWidth(t *testing.T) {
	var gotRange string
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data", ColumnCount: 5}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			gotRange = a1Range
			return [][]any{{"a", "b", "c", "d", "e"}}, nil
		},
	}
	out, err := attemptReadRange(t, api, "sheet-1", "Data!1:1")
	if err != nil {
		t.Fatalf("attemptReadRange: %v", err)
	}
	if out.ColumnCount != 5 {
		t.Errorf("ColumnCount = %d, want 5 (the sheet's reported width)", out.ColumnCount)
	}
	wantColumns := []string{"A", "B", "C", "D", "E"}
	if len(out.Columns) != len(wantColumns) {
		t.Fatalf("Columns = %v, want %v", out.Columns, wantColumns)
	}
	for i, c := range wantColumns {
		if out.Columns[i] != c {
			t.Errorf("Columns[%d] = %q, want %q", i, out.Columns[i], c)
		}
	}
	if gotRange != "'Data'!A1:E1" {
		t.Errorf("Values called with %q, want %q", gotRange, "'Data'!A1:E1")
	}
}

// TestReadRange_RowOnlyRange_ResolvedOverCap proves the pre-fetch cap is
// re-checked once a row-only range's column span is known: 500 rows across
// a 100-column-wide sheet is 50,000 cells, over MaxReadRangeCells, even
// though the row span alone (500) is well under MaxReadRangeRows and would
// have passed ValidateReadRangeInput's earlier, column-unaware check.
func TestReadRange_RowOnlyRange_ResolvedOverCap(t *testing.T) {
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data", ColumnCount: 100}}}, nil
		},
	}
	_, err := attemptReadRange(t, api, "sheet-1", "Data!1:500")
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("err = %v, want ErrRangeTooLarge", err)
	}
}

// ---- ragged rows ----

// TestReadRange_RaggedRowsPadded is the plan's required test: Google
// returns short rows (trailing empty cells simply omitted), never
// nil-padded ones, and readRange must pad every row out to the requested
// column span rather than leave Rows ragged (see readRange's doc comment
// for why).
func TestReadRange_RaggedRowsPadded(t *testing.T) {
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			return [][]any{
				{"row1-a"},                     // ragged: only 1 of 3 cells
				{"row2-a", "row2-b"},           // ragged: only 2 of 3 cells
				{"row3-a", "row3-b", "row3-c"}, // full width
			}, nil
		},
	}
	out, err := attemptReadRange(t, api, "sheet-1", "Data!A1:C3")
	if err != nil {
		t.Fatalf("attemptReadRange: %v", err)
	}
	if out.ColumnCount != 3 {
		t.Fatalf("ColumnCount = %d, want 3", out.ColumnCount)
	}
	for i, row := range out.Rows {
		if len(row) != 3 {
			t.Fatalf("Rows[%d] = %v, want length 3 (padded rectangle)", i, row)
		}
	}
	if out.Rows[0][1] != "" || out.Rows[0][2] != "" {
		t.Errorf("Rows[0] = %v, want the missing trailing cells padded to \"\"", out.Rows[0])
	}
	if out.Rows[1][2] != "" {
		t.Errorf("Rows[1] = %v, want the missing trailing cell padded to \"\"", out.Rows[1])
	}
	if out.Rows[2][0] != "row3-a" || out.Rows[2][1] != "row3-b" || out.Rows[2][2] != "row3-c" {
		t.Errorf("Rows[2] = %v, want the full row unchanged", out.Rows[2])
	}
}

// ---- result size cap ----

func TestReadRange_ResultTooLarge(t *testing.T) {
	big := strings.Repeat("x", 2000)
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			rows := make([][]any, 200)
			for i := range rows {
				rows[i] = []any{big, big}
			}
			return rows, nil
		},
	}
	// 200 rows x 2 columns = 400 cells (under the 20,000 pre-fetch cap),
	// but ~2KB per cell comfortably exceeds the 256KB post-fetch cap.
	_, err := attemptReadRange(t, api, "sheet-1", "Data!A1:B200")
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("err = %v, want ErrResultTooLarge", err)
	}
}

// ---- rate limiting ----

// TestReadRange_RateLimitDenied_ChargesExactlyOnce is the plan's required
// test: an exhausted rate-limit bucket surfaces as RateLimitedError (never
// a partial result — read_range has none to offer), and the charge happens
// exactly once, before Values is ever called (values is left unset on the
// mock, so a call there fails the test automatically).
func TestReadRange_RateLimitDenied_ChargesExactlyOnce(t *testing.T) {
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data"}}}, nil
		},
	}
	charger := &fakeCharger{allowUpTo: 0}

	in := ReadRangeInput{SpreadsheetID: "sheet-1", RangeStr: "Data!A1:D10"}
	parsed, err := ValidateReadRangeInput(in)
	if err != nil {
		t.Fatalf("ValidateReadRangeInput: %v", err)
	}
	_, err = readRange(context.Background(), api, uuid.New(), charger, in, parsed)

	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("err = %v, want *RateLimitedError", err)
	}
	if rlErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want > 0", rlErr.RetryAfter)
	}
	if charger.calls != 1 {
		t.Errorf("charger.calls = %d, want exactly 1", charger.calls)
	}
}

// TestReadRange_RateLimitAllowed_ChargesExactlyOnce is the positive
// counterpart: a successful read charges the bucket exactly once (for the
// single Values.Get call), never once per SpreadsheetMeta call too.
func TestReadRange_RateLimitAllowed_ChargesExactlyOnce(t *testing.T) {
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			return [][]any{{"x"}}, nil
		},
	}
	charger := &fakeCharger{allowUpTo: 10}

	in := ReadRangeInput{SpreadsheetID: "sheet-1", RangeStr: "Data!A1:A1"}
	parsed, err := ValidateReadRangeInput(in)
	if err != nil {
		t.Fatalf("ValidateReadRangeInput: %v", err)
	}
	_, err = readRange(context.Background(), api, uuid.New(), charger, in, parsed)
	if err != nil {
		t.Fatalf("readRange: %v", err)
	}
	if charger.calls != 1 {
		t.Errorf("charger.calls = %d, want exactly 1", charger.calls)
	}
}

// TestReadRange_ColumnPastSheetMaximumRejected covers the one range shape
// that slips past both pre-fetch caps while still being unanswerable: a
// narrow span (two columns, well under MaxReadRangeCells) whose column
// letters run past "ZZZ", Google Sheets' own 18,278-column ceiling. Without
// columnIndex's maxColumnIndex bound this parses happily and spends a
// rate-limit token on a Values.Get Google can only reject, so the assertion
// is both that it errors and that it never reaches the network.
func TestReadRange_ColumnPastSheetMaximumRejected(t *testing.T) {
	for _, rangeStr := range []string{
		"Contracts!AAAAAAAAAAAAA1:AAAAAAAAAAAAB2",
		"Contracts!A1:AAAA2",
	} {
		t.Run(rangeStr, func(t *testing.T) {
			_, err := attemptReadRange(t, untouchedAPI(t), "sheet-1", rangeStr)
			if err == nil {
				t.Fatalf("attemptReadRange(%q): want an error, got success", rangeStr)
			}
			if !strings.Contains(err.Error(), "ZZZ") {
				t.Errorf("err = %v, want it to name the last valid column (%q)", err, "ZZZ")
			}
		})
	}

	// The bound is exact: "ZZZ" itself is the last column a sheet can have
	// and must still parse.
	p, err := ParseA1Range("Contracts!ZZZ1:ZZZ2")
	if err != nil {
		t.Fatalf("ParseA1Range(ZZZ): %v", err)
	}
	if p.StartCol != maxColumnIndex {
		t.Errorf("StartCol = %d, want %d", p.StartCol, maxColumnIndex)
	}
}
