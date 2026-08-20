package googlesheets

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
)

// ColumnDescription is one header column sheets_describe_spreadsheet
// reports for a tab.
type ColumnDescription struct {
	// Index is the 0-based column position.
	Index int
	// Letter is the A1-notation column letter ("A", "B", ..., "AA", ...).
	Letter string
	// Header is the header row's cell text for this column, "" if blank.
	Header string
}

// SheetDescription describes one tab within a spreadsheet.
type SheetDescription struct {
	Name        string
	RowCount    int
	ColumnCount int
	// Columns is derived from the header row (row 1 by default, or
	// config.scope.header_rows' override for this spreadsheet), one entry
	// per non-empty leading cell Google returns for that row. Empty when
	// the header row itself is blank.
	Columns []ColumnDescription
	// SampleRows holds up to includeSampleRows rows immediately following
	// the header, each cell stringified positionally (index i corresponds
	// to Columns[i], when present). Empty unless the caller asked for
	// samples.
	SampleRows [][]string
}

// DescribeSpreadsheetOutput is sheets_describe_spreadsheet's result — the
// schema-discovery shape docs/06-sheets-adapter.md §4.1 calls non-negotiable:
// without it an agent has no way to learn a tab's name or a column's header
// short of guessing.
type DescribeSpreadsheetOutput struct {
	SpreadsheetID string
	Title         string
	Sheets        []SheetDescription
}

// DescribeSpreadsheet describes spreadsheetID's tabs, headers, and
// (optionally) a few sample rows per tab. It checks cfg's allowlist itself
// — every call, against the cfg the caller just decrypted, never a cached
// value (docs/07-sheets-adapter-plan.md step 5's design point) — so this is
// the one place that check is guaranteed to run regardless of what the
// caller remembered to do first. A spreadsheetID absent from
// cfg.Scope.SpreadsheetIDs returns an error wrapping ErrSpreadsheetNotAllowed
// and makes no upstream call at all.
func DescribeSpreadsheet(ctx context.Context, ts oauth2.TokenSource, cfg *Config, spreadsheetID string, includeSampleRows int) (*DescribeSpreadsheetOutput, error) {
	if !cfg.IsSpreadsheetAllowed(spreadsheetID) {
		return nil, fmt.Errorf("%w: %s", ErrSpreadsheetNotAllowed, spreadsheetID)
	}

	api, err := newClient(ctx, ts, "")
	if err != nil {
		return nil, err
	}
	return describeSpreadsheet(ctx, api, cfg, spreadsheetID, includeSampleRows)
}

// describeSpreadsheet is DescribeSpreadsheet minus client construction and
// the allowlist check (already done by the caller), so describe_test.go can
// drive it against a mocked sheetsAPI instead of a real Google client and
// exercise the header/sample-row shaping directly.
func describeSpreadsheet(ctx context.Context, api sheetsAPI, cfg *Config, spreadsheetID string, includeSampleRows int) (*DescribeSpreadsheetOutput, error) {
	meta, err := api.SpreadsheetMeta(ctx, spreadsheetID)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: describe spreadsheet: %w", err)
	}

	headerRow := cfg.HeaderRow(spreadsheetID)

	out := &DescribeSpreadsheetOutput{SpreadsheetID: meta.SpreadsheetID, Title: meta.Title}
	if out.SpreadsheetID == "" {
		out.SpreadsheetID = spreadsheetID
	}

	for _, sh := range meta.Sheets {
		desc := SheetDescription{Name: sh.Title, RowCount: sh.RowCount, ColumnCount: sh.ColumnCount}

		headerValues, err := api.Values(ctx, spreadsheetID, a1RowRange(sh.Title, headerRow, headerRow))
		if err != nil {
			return nil, fmt.Errorf("googlesheets: read header row for sheet %q: %w", sh.Title, err)
		}
		if len(headerValues) > 0 {
			for i, cell := range headerValues[0] {
				desc.Columns = append(desc.Columns, ColumnDescription{
					Index:  i,
					Letter: columnLetter(i),
					Header: cellString(cell),
				})
			}
		}

		if includeSampleRows > 0 {
			sampleValues, err := api.Values(ctx, spreadsheetID, a1RowRange(sh.Title, headerRow+1, headerRow+includeSampleRows))
			if err != nil {
				return nil, fmt.Errorf("googlesheets: read sample rows for sheet %q: %w", sh.Title, err)
			}
			for _, row := range sampleValues {
				strRow := make([]string, len(row))
				for i, cell := range row {
					strRow[i] = cellString(cell)
				}
				desc.SampleRows = append(desc.SampleRows, strRow)
			}
		}

		out.Sheets = append(out.Sheets, desc)
	}

	return out, nil
}

// a1RowRange builds an A1-notation range spanning whole rows row1..row2 of
// sheetName, e.g. "'Contracts'!2:6". The sheet name is always single-quoted
// (valid even when unnecessary) with internal quotes doubled per Google's
// escaping convention, since header/tab names come from the spreadsheet
// itself and may contain spaces or other characters that are only safe
// inside quotes.
func a1RowRange(sheetName string, row1, row2 int) string {
	return fmt.Sprintf("%s!%d:%d", quoteSheetName(sheetName), row1, row2)
}

func quoteSheetName(name string) string {
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// cellString stringifies one cell value as the Sheets API returns it
// (string, float64, bool, or nil after JSON decoding) for display in a
// tool result. Never used for anything Google evaluates — this is one-way,
// read-only formatting.
func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// columnLetter converts a 0-based column index into its A1-notation letter:
// 0 -> "A", 25 -> "Z", 26 -> "AA", and so on.
func columnLetter(index int) string {
	if index < 0 {
		return ""
	}
	var letters []byte
	n := index
	for {
		letters = append([]byte{byte('A' + n%26)}, letters...)
		n = n/26 - 1
		if n < 0 {
			break
		}
	}
	return string(letters)
}
