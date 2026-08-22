package googlesheets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// sheets_read_range: the A1-range escape hatch for what sheets_query_rows'
// filter DSL cannot express.
//
// A range string is agent-supplied input, not a trusted identifier, so
// nothing here hands one to Google unexamined. ParseA1Range decomposes it
// into a sheet name and numeric bounds first — the same discipline filter.go
// applies to filter values, applied to a range instead.

const (
	// Pre-fetch bounds on a parsed range's own dimensions, checked before any
	// Values.Get. These catch an oversized *request*; maxResultBytes'
	// post-fetch cap catches an oversized response. A fully bounded range is
	// checked before any network call; a row-only range is re-checked once
	// readRange resolves its column span, still before the data fetch.
	MaxReadRangeRows  = 1000
	MaxReadRangeCells = 20000
)

// ErrUnboundedRange is returned for a range with no explicit numeric row
// bound on both ends — a bare sheet name, a column-only range ("A:D").  At
// the spec's 87GB target scale that is the load-the-whole-sheet failure mode
// docs/06 §6 rules out, so the parser rejects it before Values.Get. A
// row-only range ("1:100") is bounded in this sense and does not hit here.
var ErrUnboundedRange = errors.New("googlesheets: range has no explicit row bounds")

// ErrRangeTooLarge is returned when a parsed range exceeds MaxReadRangeRows
// or MaxReadRangeCells — the pre-fetch guardrail, distinct from
// ErrResultTooLarge's post-fetch byte cap.
var ErrRangeTooLarge = errors.New("googlesheets: range exceeds the pre-fetch size cap")

// RateLimitedError is returned when read_range's Values.Get charge finds the
// bucket empty. Unlike query_rows' scan, which degrades to ScanComplete:
// false, a read_range call has no partial result — the range was read or it
// wasn't — so this is a distinct type tools_sheets.go maps via errors.As.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("googlesheets: rate limit exceeded, retry after %s", e.RetryAfter)
}

// ParsedA1Range is an agent-supplied range decomposed into a structured
// form, never an opaque string handed to Google. StartCol/EndCol are 0-based;
// -1 on both means a row-only reference ("1:100") whose column span is known
// only once the sheet's dimensions are fetched. readRange resolves that,
// keeping ParseA1Range networkless so it can run before config decryption.
type ParsedA1Range struct {
	// SheetName is "" when the input omitted a sheet prefix; readRange
	// resolves it to the first tab and echoes the resolved name back, so the
	// caller always knows what was read.
	SheetName    string
	HasSheetName bool
	StartCol     int
	EndCol       int
	StartRow     int
	EndRow       int
}

// RowSpan is always known immediately after parsing — unlike column span,
// a range's row bounds are never left implicit (ErrUnboundedRange rejects
// anything that would leave them so).
func (r ParsedA1Range) RowSpan() int {
	return r.EndRow - r.StartRow + 1
}

// ColSpan reports the parsed range's column span, or -1 when it is not yet
// known (a row-only range before its columns have been resolved against
// the sheet's actual width).
func (r ParsedA1Range) ColSpan() int {
	if r.StartCol < 0 {
		return -1
	}
	return r.EndCol - r.StartCol + 1
}

// CellCount reports RowSpan*ColSpan, or -1 when ColSpan is not yet known
// (see ColSpan) — checkRangeSize treats a negative cell count as "skip
// this half of the check for now," since it will be re-run once the
// column span is resolved.
func (r ParsedA1Range) CellCount() int {
	cols := r.ColSpan()
	if cols < 0 {
		return -1
	}
	return r.RowSpan() * cols
}

var (
	// fullCellRefRe matches a single A1 cell reference: one or more
	// (case-insensitive) column letters followed by one or more digits —
	// "A1", "aa100", "D10".
	fullCellRefRe = regexp.MustCompile(`^[A-Za-z]+[0-9]+$`)
	// splitCellRefRe captures fullCellRefRe's two halves once it has
	// already matched, so the letters/digits don't need re-deriving with
	// index arithmetic.
	splitCellRefRe = regexp.MustCompile(`^([A-Za-z]+)([0-9]+)$`)
	// rowOnlyRefRe matches a bare row number — "1", "100" — one half of a
	// row-only range like "1:100".
	rowOnlyRefRe = regexp.MustCompile(`^[0-9]+$`)
	// colOnlyRefRe matches bare column letters — "A", "aa" — half of a
	// column-only range like "A:D". Also what a bare sheet name parses as;
	// both mean "no explicit row bound", so both are rejected together.
	colOnlyRefRe = regexp.MustCompile(`^[A-Za-z]+$`)
)

// a1RefKind is which of the three reference shapes a colon-separated half
// of a range matched: a full cell ("A1"), a bare row ("1"), or bare column
// letters ("A"). ParseA1Range requires both halves of a range to match the
// same kind — "A1:100" (cell on one side, row on the other) is rejected as
// malformed rather than guessed at.
type a1RefKind int

const (
	a1RefInvalid a1RefKind = iota
	a1RefFull
	a1RefRowOnly
	a1RefColOnly
)

// classifyA1Ref parses one colon-separated half of a range into its kind and
// numeric values. Anything else — a smuggled formula, a comma, a second
// "!" — matches no regex here and falls to the default, which is why callers
// need no separate injection check: a half that isn't exactly
// "[letters][digits]", "[digits]", or "[letters]" simply never parses.
func classifyA1Ref(s string) (kind a1RefKind, col, row int, err error) {
	switch {
	case fullCellRefRe.MatchString(s):
		m := splitCellRefRe.FindStringSubmatch(s)
		col, err = columnIndex(m[1])
		if err != nil {
			return a1RefInvalid, 0, 0, err
		}
		row, err = strconv.Atoi(m[2])
		if err != nil || row < 1 {
			return a1RefInvalid, 0, 0, fmt.Errorf("googlesheets: row %q must be a positive integer", m[2])
		}
		return a1RefFull, col, row, nil
	case rowOnlyRefRe.MatchString(s):
		row, err = strconv.Atoi(s)
		if err != nil || row < 1 {
			return a1RefInvalid, 0, 0, fmt.Errorf("googlesheets: row %q must be a positive integer", s)
		}
		return a1RefRowOnly, 0, row, nil
	case colOnlyRefRe.MatchString(s):
		// Index deliberately not computed: ParseA1Range rejects column-only
		// refs anyway, and running a bare sheet name like "Contracts"
		// (syntactically just column letters) through columnIndex would
		// report "past the last column" instead of "needs row bounds".
		return a1RefColOnly, 0, 0, nil
	default:
		return a1RefInvalid, 0, 0, fmt.Errorf("googlesheets: %q is not a valid A1 cell, row, or column reference", s)
	}
}

// maxColumnIndex is the 0-based index of "ZZZ", Sheets' ceiling of 18,278
// columns. Bounding here matters twice: a range like "AAAAAAAAAAAAA1:...B2"
// spans only two columns, so neither the row nor the cell cap would stop it
// spending a token on a Values.Get Google answers with an opaque 400; and it
// removes the only way CellCount's RowSpan*ColSpan could overflow negative
// and slip past checkRangeSize.
const maxColumnIndex = 18277

// columnIndex converts A1 column letters to a 0-based index — the inverse of
// describe.go's columnLetter. Case-insensitive, and bounded by
// maxColumnIndex rather than trusting agent-supplied letters to be sane.
func columnIndex(letters string) (int, error) {
	letters = strings.ToUpper(letters)
	idx := 0
	for i := 0; i < len(letters); i++ {
		c := letters[i]
		if c < 'A' || c > 'Z' {
			return 0, fmt.Errorf("googlesheets: %q is not a valid column reference", letters)
		}
		idx = idx*26 + int(c-'A') + 1
		if idx-1 > maxColumnIndex {
			return 0, fmt.Errorf(
				"googlesheets: column %q is past the last column a sheet can have (%q) — check the range",
				letters, columnLetter(maxColumnIndex))
		}
	}
	return idx - 1, nil
}

// splitSheetAndRange peels an optional leading "SheetName!" or "'Quoted
// Name'!" off s, following Google's escaping convention (a literal "'"
// inside a quoted name is doubled — mirrors quoteSheetName).
//
// A sheet name cannot hide a second range this way: the quoted form ends
// only at an unescaped "'", the unquoted form at the first "!", so
// "Contracts!A1:B2,Other!A1:B2" leaves everything past the first "!" as one
// opaque range part that then fails to parse, rather than quietly addressing
// a second sheet.
func splitSheetAndRange(s string) (sheetName string, hasSheet bool, rangePart string, err error) {
	if strings.HasPrefix(s, "'") {
		var sb strings.Builder
		i := 1
		closed := false
		for i < len(s) {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					sb.WriteByte('\'')
					i += 2
					continue
				}
				i++
				closed = true
				break
			}
			sb.WriteByte(s[i])
			i++
		}
		if !closed {
			return "", false, "", fmt.Errorf("googlesheets: %q has an unterminated quoted sheet name", s)
		}
		if i >= len(s) || s[i] != '!' {
			return "", false, "", fmt.Errorf("googlesheets: %q is missing \"!\" after the quoted sheet name", s)
		}
		return sb.String(), true, s[i+1:], nil
	}
	if idx := strings.Index(s, "!"); idx >= 0 {
		return s[:idx], true, s[idx+1:], nil
	}
	return "", false, s, nil
}

// splitRangeParts splits a range part on its first ":" into two halves. A
// range with no colon (a single cell reference, e.g. "A1") is treated as
// referring to the same cell twice — ParseA1Range's kind/bounds logic then
// degenerates cleanly into a 1x1 range without a separate code path. A
// range with a *second* colon ("A1:B2:C3") is deliberately not specially
// detected here: the second half becomes "B2:C3", which matches none of
// classifyA1Ref's three patterns and so fails to parse, exactly like any
// other malformed range.
func splitRangeParts(rangePart string) (left, right string) {
	if idx := strings.Index(rangePart, ":"); idx >= 0 {
		return rangePart[:idx], rangePart[idx+1:]
	}
	return rangePart, rangePart
}

// ParseA1Range decomposes an agent-supplied A1 range, or rejects it. Makes
// no network call and touches no config, so it can run before config
// decryption. Three outcomes:
//
//   - Malformed syntax (a formula, a smuggled second range) returns a plain
//     error from classifyA1Ref.
//   - No explicit row bound on both ends returns ErrUnboundedRange.
//   - Otherwise well-formed, either fully bounded or row-only. Reversed
//     bounds ("D10:A1") are normalized ascending rather than rejected — the
//     intent is unambiguous, so there is nothing to make the agent retry for.
func ParseA1Range(s string) (ParsedA1Range, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ParsedA1Range{}, errors.New("googlesheets: range is required")
	}

	sheetName, hasSheet, rangePart, err := splitSheetAndRange(trimmed)
	if err != nil {
		return ParsedA1Range{}, err
	}
	if rangePart == "" {
		return ParsedA1Range{}, fmt.Errorf("googlesheets: %q has no range after the sheet name", trimmed)
	}

	left, right := splitRangeParts(rangePart)
	leftKind, leftCol, leftRow, err := classifyA1Ref(left)
	if err != nil {
		return ParsedA1Range{}, fmt.Errorf("googlesheets: %q is not a valid A1 range: %w", trimmed, err)
	}
	rightKind, rightCol, rightRow, err := classifyA1Ref(right)
	if err != nil {
		return ParsedA1Range{}, fmt.Errorf("googlesheets: %q is not a valid A1 range: %w", trimmed, err)
	}
	if leftKind != rightKind {
		return ParsedA1Range{}, fmt.Errorf(
			"googlesheets: %q mixes cell, row, and column references across \":\" — use the same kind of reference on both sides", trimmed)
	}

	switch leftKind {
	case a1RefColOnly:
		return ParsedA1Range{}, fmt.Errorf(
			"%w: %q — specify explicit row bounds, e.g. \"Contracts!A1:D100\"", ErrUnboundedRange, trimmed)
	case a1RefRowOnly:
		startRow, endRow := leftRow, rightRow
		if startRow > endRow {
			startRow, endRow = endRow, startRow
		}
		return ParsedA1Range{
			SheetName: sheetName, HasSheetName: hasSheet,
			StartCol: -1, EndCol: -1,
			StartRow: startRow, EndRow: endRow,
		}, nil
	default: // a1RefFull
		startCol, endCol := leftCol, rightCol
		if startCol > endCol {
			startCol, endCol = endCol, startCol
		}
		startRow, endRow := leftRow, rightRow
		if startRow > endRow {
			startRow, endRow = endRow, startRow
		}
		return ParsedA1Range{
			SheetName: sheetName, HasSheetName: hasSheet,
			StartCol: startCol, EndCol: endCol,
			StartRow: startRow, EndRow: endRow,
		}, nil
	}
}

// checkRangeSize enforces the pre-fetch caps, naming the fix in the message.
// cellCount < 0 (a row-only range, columns not yet resolved) skips the cell
// half; readRange re-runs this once columns resolve, so the cell check still
// always happens before Values.Get.
func checkRangeSize(rowSpan, cellCount int) error {
	if rowSpan > MaxReadRangeRows {
		return fmt.Errorf("%w: spans %d rows, max %d — narrow the range", ErrRangeTooLarge, rowSpan, MaxReadRangeRows)
	}
	if cellCount >= 0 && cellCount > MaxReadRangeCells {
		return fmt.Errorf("%w: spans %d cells, max %d — narrow the range", ErrRangeTooLarge, cellCount, MaxReadRangeCells)
	}
	return nil
}

// ReadRangeInput is the model's own strings, unparsed.
type ReadRangeInput struct {
	SpreadsheetID string
	RangeStr      string
}

// ValidateReadRangeInput checks shape and parses the range without touching
// the network or config, so a malformed, unbounded, or oversized range never
// spends the connector's credential or rate-limit budget. The parsed range
// is threaded through to ReadRange so it is only ever parsed once.
func ValidateReadRangeInput(in ReadRangeInput) (ParsedA1Range, error) {
	if in.SpreadsheetID == "" {
		return ParsedA1Range{}, errors.New("googlesheets: spreadsheet_id is required")
	}
	parsed, err := ParseA1Range(in.RangeStr)
	if err != nil {
		return ParsedA1Range{}, err
	}
	if err := checkRangeSize(parsed.RowSpan(), parsed.CellCount()); err != nil {
		return ParsedA1Range{}, err
	}
	return parsed, nil
}

// ReadRangeOutput is sheets_read_range's result. Range is always the fully
// resolved range actually read, so the agent sees what happened even when
// its request left the sheet name or columns implicit. Rows[i][j] came from
// column Columns[j], padded to a rectangle len(Columns) wide.
type ReadRangeOutput struct {
	SpreadsheetID string
	Range         string
	SheetName     string
	Columns       []string
	Rows          [][]string
	RowCount      int
	ColumnCount   int
}

// ReadRange checks cfg's allowlist, builds a client, and reads an
// already-validated range. The allowlist check runs first and
// unconditionally, against the cfg the caller just decrypted — never a
// cached value.
func ReadRange(ctx context.Context, ts oauth2.TokenSource, cfg *Config, connectorID uuid.UUID, charger RateCharger, in ReadRangeInput, parsed ParsedA1Range) (*ReadRangeOutput, error) {
	if !cfg.IsSpreadsheetAllowed(in.SpreadsheetID) {
		return nil, fmt.Errorf("%w: %s", ErrSpreadsheetNotAllowed, in.SpreadsheetID)
	}
	api, err := newClient(ctx, ts, "")
	if err != nil {
		return nil, err
	}
	return readRange(ctx, api, connectorID, charger, in, parsed)
}

// readRange is ReadRange minus client construction and the allowlist check,
// so tests can drive it against a mocked sheetsAPI.
//
// Two ordering rules carry weight. checkRangeSize is re-run here, after a
// row-only range's columns resolve, but still before Values.Get — an
// oversized range is rejected before any data-bearing call, not after. And
// only the Values.Get is charged: SpreadsheetMeta rides the dispatch-time
// floor enforce() already charges, as queryRows' header fetch does.
func readRange(ctx context.Context, api sheetsAPI, connectorID uuid.UUID, charger RateCharger, in ReadRangeInput, parsed ParsedA1Range) (*ReadRangeOutput, error) {
	meta, err := api.SpreadsheetMeta(ctx, in.SpreadsheetID)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: read range: %w", err)
	}

	sheetName := parsed.SheetName
	if !parsed.HasSheetName {
		if len(meta.Sheets) == 0 {
			return nil, fmt.Errorf("googlesheets: read range: spreadsheet %q has no tabs", in.SpreadsheetID)
		}
		sheetName = meta.Sheets[0].Title
	}

	var sheetMeta *SheetMeta
	for i := range meta.Sheets {
		if meta.Sheets[i].Title == sheetName {
			sheetMeta = &meta.Sheets[i]
			break
		}
	}
	if sheetMeta == nil {
		return nil, fmt.Errorf("%w: %s", ErrSheetNotFound, sheetName)
	}

	startCol, endCol := parsed.StartCol, parsed.EndCol
	if startCol < 0 {
		// Row-only range ("1:100"): resolved against the sheet's own reported
		// width, the only source of truth for what "every column" means here.
		startCol = 0
		endCol = sheetMeta.ColumnCount - 1
		if endCol < startCol {
			endCol = startCol
		}
	}
	colSpan := endCol - startCol + 1
	rowSpan := parsed.RowSpan()
	if err := checkRangeSize(rowSpan, rowSpan*colSpan); err != nil {
		return nil, err
	}

	resolvedRange := formatA1Range(sheetName, startCol, endCol, parsed.StartRow, parsed.EndRow)

	if err := chargeOne(ctx, charger, connectorID, "read range"); err != nil {
		return nil, err
	}

	values, err := api.Values(ctx, in.SpreadsheetID, resolvedRange)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: read range %q: %w", resolvedRange, err)
	}

	columns := make([]string, colSpan)
	for i := range columns {
		columns[i] = columnLetter(startCol + i)
	}

	// Padded to colSpan-wide rectangles, not left ragged: Google drops a
	// row's trailing empty cells rather than returning "", so a ragged Rows
	// could not say whether column D was absent or merely empty, and would
	// desynchronize rows from Columns' fixed width. Padding buys a caller
	// Rows[i][j] against Columns[j] with no bounds check.
	rows := make([][]string, len(values))
	for i, row := range values {
		strRow := make([]string, colSpan)
		for j := 0; j < colSpan; j++ {
			if j < len(row) {
				strRow[j] = cellString(row[j])
			}
		}
		rows[i] = strRow
	}

	out := &ReadRangeOutput{
		SpreadsheetID: in.SpreadsheetID,
		Range:         resolvedRange,
		SheetName:     sheetName,
		Columns:       columns,
		Rows:          rows,
		RowCount:      len(rows),
		ColumnCount:   colSpan,
	}
	if err := checkEncodedSize(out.Rows); err != nil {
		return nil, err
	}
	return out, nil
}

// formatA1Range builds the fully-resolved range readRange sends upstream and
// echoes back. Sheet name always quoted, bounds always explicit numbers, so
// a caller who omitted either still learns exactly what was read.
func formatA1Range(sheetName string, startCol, endCol, startRow, endRow int) string {
	return fmt.Sprintf("%s!%s%d:%s%d", quoteSheetName(sheetName), columnLetter(startCol), startRow, columnLetter(endCol), endRow)
}
