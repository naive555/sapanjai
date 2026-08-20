package googlesheets

import (
	"context"
	"errors"
	"testing"
)

func TestDescribeSpreadsheet_RejectsNonAllowlistedIDWithoutTouchingTheNetwork(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"1AbCAllowed"}}}

	// ts is nil: if DescribeSpreadsheet reached newClient before the
	// allowlist check, this would panic (oauth2.NewClient dereferencing a
	// nil TokenSource) rather than return ErrSpreadsheetNotAllowed — the
	// panic itself would be the signal that the check ran too late.
	_, err := DescribeSpreadsheet(context.Background(), nil, cfg, "1XyZNotAllowed", 0)
	if !errors.Is(err, ErrSpreadsheetNotAllowed) {
		t.Fatalf("err = %v, want ErrSpreadsheetNotAllowed", err)
	}
}

func TestDescribeSpreadsheet_AllowedIDPasses(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"1AbCAllowed"}}}
	if !cfg.IsSpreadsheetAllowed("1AbCAllowed") {
		t.Fatal("sanity check: fixture id should be allowlisted")
	}
}

func TestDescribeSpreadsheet_HeaderAndColumns(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-1"}}}
	var gotRange string
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{
				SpreadsheetID: spreadsheetID,
				Title:         "ระบบสัญญา 2568",
				Sheets: []SheetMeta{
					{Title: "Contracts", RowCount: 12450, ColumnCount: 5},
				},
			}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			gotRange = a1Range
			return [][]any{{"contract_id", "partner_name"}}, nil
		},
	}

	out, err := describeSpreadsheet(context.Background(), api, cfg, "sheet-1", 0)
	if err != nil {
		t.Fatalf("describeSpreadsheet: %v", err)
	}
	if gotRange != "'Contracts'!1:1" {
		t.Errorf("header range = %q, want %q", gotRange, "'Contracts'!1:1")
	}
	if out.Title != "ระบบสัญญา 2568" {
		t.Errorf("title = %q", out.Title)
	}
	if len(out.Sheets) != 1 {
		t.Fatalf("Sheets = %d, want 1", len(out.Sheets))
	}
	sh := out.Sheets[0]
	if sh.Name != "Contracts" || sh.RowCount != 12450 || sh.ColumnCount != 5 {
		t.Errorf("sheet = %+v, want Contracts/12450/5", sh)
	}
	if len(sh.Columns) != 2 {
		t.Fatalf("Columns = %d, want 2", len(sh.Columns))
	}
	if sh.Columns[0] != (ColumnDescription{Index: 0, Letter: "A", Header: "contract_id"}) {
		t.Errorf("Columns[0] = %+v", sh.Columns[0])
	}
	if sh.Columns[1] != (ColumnDescription{Index: 1, Letter: "B", Header: "partner_name"}) {
		t.Errorf("Columns[1] = %+v", sh.Columns[1])
	}
	if sh.SampleRows != nil {
		t.Errorf("SampleRows = %v, want nil when include_sample_rows is 0", sh.SampleRows)
	}
}

func TestDescribeSpreadsheet_SampleRowsRangeAndCount(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-1"}}}
	var ranges []string
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Data"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			ranges = append(ranges, a1Range)
			if a1Range == "'Data'!1:1" {
				return [][]any{{"id", "name"}}, nil
			}
			return [][]any{{"C-1", "Alice"}, {"C-2", "Bob"}}, nil
		},
	}

	out, err := describeSpreadsheet(context.Background(), api, cfg, "sheet-1", 3)
	if err != nil {
		t.Fatalf("describeSpreadsheet: %v", err)
	}
	if len(ranges) != 2 || ranges[1] != "'Data'!2:4" {
		t.Fatalf("ranges = %v, want header then sample range '%s'", ranges, "'Data'!2:4")
	}
	sh := out.Sheets[0]
	if len(sh.SampleRows) != 2 {
		t.Fatalf("SampleRows = %d, want 2 (whatever the mock returned)", len(sh.SampleRows))
	}
	if sh.SampleRows[0][0] != "C-1" || sh.SampleRows[0][1] != "Alice" {
		t.Errorf("SampleRows[0] = %v", sh.SampleRows[0])
	}
}

func TestDescribeSpreadsheet_HeaderRowOverride(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{
		SpreadsheetIDs: []string{"sheet-1"},
		HeaderRows:     map[string]int{"sheet-1": 3},
	}}
	var headerRange, sampleRange string
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Sheet1"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			if headerRange == "" {
				headerRange = a1Range
				return [][]any{{"h"}}, nil
			}
			sampleRange = a1Range
			return [][]any{{"v"}}, nil
		},
	}

	if _, err := describeSpreadsheet(context.Background(), api, cfg, "sheet-1", 1); err != nil {
		t.Fatalf("describeSpreadsheet: %v", err)
	}
	if headerRange != "'Sheet1'!3:3" {
		t.Errorf("header range = %q, want row 3 per the override", headerRange)
	}
	if sampleRange != "'Sheet1'!4:4" {
		t.Errorf("sample range = %q, want row 4 (immediately after the overridden header)", sampleRange)
	}
}

func TestDescribeSpreadsheet_MultipleSheets(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-1"}}}
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{
				SpreadsheetID: spreadsheetID,
				Sheets: []SheetMeta{
					{Title: "Tab A"}, {Title: "Tab B"},
				},
			}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			return [][]any{{"col"}}, nil
		},
	}

	out, err := describeSpreadsheet(context.Background(), api, cfg, "sheet-1", 0)
	if err != nil {
		t.Fatalf("describeSpreadsheet: %v", err)
	}
	if len(out.Sheets) != 2 || out.Sheets[0].Name != "Tab A" || out.Sheets[1].Name != "Tab B" {
		t.Fatalf("Sheets = %+v, want [Tab A, Tab B] in order", out.Sheets)
	}
}

func TestDescribeSpreadsheet_BlankHeaderRowYieldsNoColumns(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-1"}}}
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "Empty"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			return [][]any{}, nil
		},
	}

	out, err := describeSpreadsheet(context.Background(), api, cfg, "sheet-1", 0)
	if err != nil {
		t.Fatalf("describeSpreadsheet: %v", err)
	}
	if len(out.Sheets[0].Columns) != 0 {
		t.Errorf("Columns = %v, want empty for a blank header row", out.Sheets[0].Columns)
	}
}

func TestDescribeSpreadsheet_PropagatesMetaError(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-1"}}}
	upstreamErr := errors.New("googleapi: Error 404: not found")
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return nil, upstreamErr
		},
	}

	_, err := describeSpreadsheet(context.Background(), api, cfg, "sheet-1", 0)
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("err = %v, want it to wrap the upstream error", err)
	}
}

func TestDescribeSpreadsheet_PropagatesValuesError(t *testing.T) {
	cfg := &Config{Scope: ScopeConfig{SpreadsheetIDs: []string{"sheet-1"}}}
	upstreamErr := errors.New("googleapi: Error 429: quota exceeded")
	api := &mockSheetsAPI{
		t: t,
		spreadsheetMeta: func(ctx context.Context, spreadsheetID string) (*Meta, error) {
			return &Meta{SpreadsheetID: spreadsheetID, Sheets: []SheetMeta{{Title: "S"}}}, nil
		},
		values: func(ctx context.Context, spreadsheetID, a1Range string) ([][]any, error) {
			return nil, upstreamErr
		},
	}

	_, err := describeSpreadsheet(context.Background(), api, cfg, "sheet-1", 0)
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("err = %v, want it to wrap the upstream error", err)
	}
}

func TestColumnLetter(t *testing.T) {
	cases := map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"}
	for idx, want := range cases {
		if got := columnLetter(idx); got != want {
			t.Errorf("columnLetter(%d) = %q, want %q", idx, got, want)
		}
	}
}

func TestQuoteSheetName_EscapesInternalQuotes(t *testing.T) {
	if got := quoteSheetName("Q1's Report"); got != "'Q1''s Report'" {
		t.Errorf("quoteSheetName = %q, want doubled internal quote", got)
	}
}

func TestCellString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"hi", "hi"},
		{true, "true"},
		{false, "false"},
		{float64(42), "42"},
		{float64(3.5), "3.5"},
	}
	for _, tc := range cases {
		if got := cellString(tc.in); got != tc.want {
			t.Errorf("cellString(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
