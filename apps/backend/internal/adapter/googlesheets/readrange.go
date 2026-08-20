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

// This file is docs/07-sheets-adapter-plan.md step 8: sheets_read_range,
// "the A1-range escape hatch" for whatever sheets_query_rows' structured
// filter DSL cannot express. The step's entire point, per the plan, is
// that "the A1 range must be parsed and re-validated against the allowlist
// — a range string is agent-supplied input, not a trusted identifier."
// Nothing in this file ever hands an agent-supplied range string to Google
// unexamined: ParseA1Range below decomposes it into a sheet name and
// numeric column/row bounds in our own code first, the same discipline
// filter.go applies to filter values (never interpolated, always compared
// as literal data) applied here to a range instead.

const (
	// MaxReadRangeRows and MaxReadRangeCells are sheets_read_range's
	// pre-fetch bounds (docs/07 step 8: "reject a range spanning more than
	// 1,000 rows or more than 20,000 cells"). Checked against the parsed
	// range's own dimensions before any Values.Get call — the first line
	// of defence behind maxResultBytes' post-fetch 256KB body cap
	// (checkEncodedSize, shared with sheets_query_rows below), which
	// catches an oversized *request* rather than only an oversized
	// response. A fully col+row-bounded range is checked at
	// ValidateReadRangeInput time, before any network call at all; a
	// row-only range ("1:100", column-unbounded until the sheet's real
	// width is known) gets that same check re-run once readRange has
	// resolved its column span against SpreadsheetMeta — still before the
	// data-bearing Values.Get.
	MaxReadRangeRows  = 1000
	MaxReadRangeCells = 20000
)

// ErrUnboundedRange is returned when a range has no explicit numeric row
// bound on both ends — a bare sheet name, a column-only range ("A:D"), or
// anything else that would mean reading the sheet's entire row extent.
// Docs/07 step 8 calls this out by name: at the spec's 87GB target scale,
// an unbounded row range is exactly the load-the-whole-sheet failure mode
// docs/06 §6 rules out, so it is rejected by the parser rather than ever
// reaching Values.Get. A row-only range ("1:100") is *not* unbounded in
// this sense — its rows are explicitly bounded even though its columns
// resolve against the sheet's actual width — so it does not hit this path.
var ErrUnboundedRange = errors.New("googlesheets: range has no explicit row bounds")

// ErrRangeTooLarge is returned when a parsed (and, for a row-only range,
// column-resolved) range spans more rows or cells than MaxReadRangeRows /
// MaxReadRangeCells allow — the pre-fetch guardrail docs/07 step 8 asks
// for, distinct from ErrResultTooLarge's post-fetch byte cap: this catches
// an oversized *request* before a single upstream byte is spent.
var ErrRangeTooLarge = errors.New("googlesheets: range exceeds the pre-fetch size cap")

// RateLimitedError is returned when sheets_read_range's single Values.Get
// charge (see ReadRange's doc comment) finds the connector's rate-limit
// bucket empty. Unlike sheets_query_rows' scan — which has a well-defined
// "partial answer" for a mid-scan bucket exhaustion (ScanComplete: false)
// — a read_range call has no partial result to fall back to: either the
// range was read or it wasn't. So this is a distinct error type, mapped by
// tools_sheets.go via errors.As to the same RateLimited(retryAfter) tool
// result sheets_query_rows' dispatch-time floor already uses, rather than
// silently returning nothing.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("googlesheets: rate limit exceeded, retry after %s", e.RetryAfter)
}

// ParsedA1Range is an agent-supplied A1 range string decomposed into our
// own structured representation — the parser docs/07 step 8 requires,
// never an opaque string handed to Google. StartCol/EndCol are 0-based
// column indexes; -1 on both means the input was a row-only reference
// ("1:100", every column across those rows) whose actual column span is
// only known once the sheet's real dimensions are fetched — readRange
// resolves it there, not here, since ParseA1Range must stay networkless to
// run before config decryption (the same ordering discipline
// ValidateQueryRowsInput/registerQueryRows already follow).
type ParsedA1Range struct {
	// SheetName is "" when the input omitted a sheet prefix (HasSheetName
	// false); readRange resolves it to the spreadsheet's first tab once
	// SpreadsheetMeta is available, and echoes the resolved name back in
	// ReadRangeOutput so the caller always knows exactly what was read.
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
	// colOnlyRefRe matches bare column letters — "A", "aa" — one half of a
	// column-only range like "A:D", which ErrUnboundedRange rejects. Also,
	// not coincidentally, what a bare sheet name with no "!" parses as
	// (e.g. the range "Contracts" alone): both cases are "no explicit row
	// bound," so both are rejected by the same path for the same reason.
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

// classifyA1Ref parses one colon-separated half of a range ("A1", "100",
// or "D") into its kind and numeric value(s). Every other string —
// including anything that could smuggle a formula ("=IMPORTRANGE(...)") or
// a second range reference (a comma, a second "!") — fails every regex
// here and falls through to the default case, so ParseA1Range's caller
// never needs a separate formula/injection check: a range that isn't
// exactly "[letters][digits]", "[digits]", or "[letters]" on both sides of
// its (at most one) colon simply never parses at all.
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
		// The column index is deliberately not computed here: ParseA1Range
		// rejects a column-only reference outright (ErrUnboundedRange), so
		// the value would be discarded — and running it through
		// columnIndex would let a bare sheet name like "Contracts" (which
		// is, syntactically, just column letters) fail with "past the last
		// column" instead of the accurate "specify explicit row bounds".
		return a1RefColOnly, 0, 0, nil
	default:
		return a1RefInvalid, 0, 0, fmt.Errorf("googlesheets: %q is not a valid A1 cell, row, or column reference", s)
	}
}

// maxColumnIndex is the 0-based index of "ZZZ", Google Sheets' own
// hard ceiling of 18,278 columns per sheet. columnIndex rejects anything
// past it rather than letting a long letter run through: a range like
// "AAAAAAAAAAAAA1:AAAAAAAAAAAAB2" spans only two columns, so neither the
// row cap nor the cell cap would stop it, and it would spend a rate-limit
// token on a Values.Get that Google can only answer with an opaque 400.
// Bounding it here turns that into an immediate, self-explanatory
// rejection, and — since a column index can no longer exceed 18,277 —
// also removes the only way ParsedA1Range.CellCount's RowSpan*ColSpan
// could overflow into a negative number and slip past checkRangeSize.
const maxColumnIndex = 18277

// columnIndex converts A1-notation column letters into a 0-based index —
// the inverse of describe.go's columnLetter ("A" -> 0, "Z" -> 25,
// "AA" -> 26, ...). Case-insensitive, per docs/07 step 8's required test
// coverage for lowercase column letters. Bounded by maxColumnIndex: a
// range string is agent-supplied input (docs/07 step 8), so its column
// reference is checked against what a sheet can actually have rather than
// being trusted to be sane.
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

// splitSheetAndRange peels an optional leading "SheetName!" or
// "'Quoted Sheet Name'!" off s, returning the sheet name (unquoted,
// doubled internal quotes collapsed to one), whether one was present, and
// whatever remains as the range part. A quoted sheet name follows Google's
// own escaping convention (mirrors quoteSheetName's writing side): a
// literal "'" inside the name is written "”". No sheet name may itself
// ever hide a second "!" or range reference this way — for the quoted
// case, only an unescaped closing "'" ends the name, and the unquoted case
// stops at the first "!" — so a smuggled second range (e.g.
// "Contracts!A1:B2,Other!A1:B2") still leaves everything past the first
// "!" as one opaque range part, which then fails to parse as a single
// range below rather than silently addressing a second sheet.
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

// ParseA1Range decomposes an agent-supplied A1 range string into a
// ParsedA1Range, or rejects it outright — docs/07 step 8's central
// requirement, "the A1 range must be parsed and re-validated ... a range
// string is agent-supplied input, not a trusted identifier." This function
// makes no network call and touches no config, so ValidateReadRangeInput
// can run it before config decryption, matching ValidateQueryRowsInput's
// ordering discipline. Three outcomes:
//
//   - Malformed syntax (including a formula like "=IMPORTRANGE(...)" or a
//     second range reference smuggled in after a comma) fails to match
//     classifyA1Ref on one or both halves and returns a plain error.
//   - A range with no explicit row bound on both ends (a bare sheet name,
//     a column-only range like "A:D") returns ErrUnboundedRange.
//   - Otherwise, the range is well-formed: either fully bounded (explicit
//     columns and rows) or row-only (explicit rows, columns resolved later
//     against the sheet's real width). Reversed bounds ("D10:A1") are
//     normalized into ascending order rather than rejected — the agent's
//     intent is unambiguous either way, and normalizing is friendlier than
//     making it retry for a cosmetic mistake.
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

// checkRangeSize enforces MaxReadRangeRows/MaxReadRangeCells against a
// parsed range's dimensions, naming the fix per docs/06 §8's "must state a
// fix" convention. cellCount < 0 (a row-only range whose column span isn't
// resolved yet) skips the cell check — ValidateReadRangeInput calls this
// once with the row bound alone locally computable, and readRange calls it
// again once a row-only range's columns are resolved against the sheet's
// real width, so the cell check still always runs before Values.Get either
// way.
func checkRangeSize(rowSpan, cellCount int) error {
	if rowSpan > MaxReadRangeRows {
		return fmt.Errorf("%w: spans %d rows, max %d — narrow the range", ErrRangeTooLarge, rowSpan, MaxReadRangeRows)
	}
	if cellCount >= 0 && cellCount > MaxReadRangeCells {
		return fmt.Errorf("%w: spans %d cells, max %d — narrow the range", ErrRangeTooLarge, cellCount, MaxReadRangeCells)
	}
	return nil
}

// ReadRangeInput is sheets_read_range's raw input: exactly the model's own
// strings, unparsed. RangeStr is parsed by ValidateReadRangeInput before
// any config decryption or network call — the same ordering discipline
// registerQueryRows/ValidateQueryRowsInput and
// registerDescribeSpreadsheet's include_sample_rows check both follow.
type ReadRangeInput struct {
	SpreadsheetID string
	RangeStr      string
}

// ValidateReadRangeInput checks in's shape and parses in.RangeStr, without
// touching the network or config — tools_sheets.go calls this before
// svc.openGoogleSheetsConfig, so a malformed, unbounded, or oversized
// range never spends a byte of the connector's OAuth credential or
// rate-limit budget (docs/07 step 8's "Ordering" requirement). The
// returned ParsedA1Range is threaded through to ReadRange so the range is
// only ever parsed once.
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
// resolved range actually read — sheet name filled in when the input
// omitted one, column bounds resolved when the input was row-only — so the
// agent can see exactly what was read even when its own request left
// something implicit. Columns holds the A1 column letter for each column
// in Rows' order (Rows[i][j] came from column Columns[j]); Rows is padded
// to a rectangle len(Columns) wide per row — see readRange's doc comment
// for why padding, not ragged, was chosen.
type ReadRangeOutput struct {
	SpreadsheetID string
	Range         string
	SheetName     string
	Columns       []string
	Rows          [][]string
	RowCount      int
	ColumnCount   int
}

// ReadRange checks cfg's allowlist, builds a client, and reads parsed (an
// already-validated ParsedA1Range from ValidateReadRangeInput) against
// connectorID's live rate-limit bucket via charger. Mirrors QueryRows'
// shape exactly: the allowlist check runs first and unconditionally,
// against the cfg the caller just decrypted, never a cached value — the
// same "checked fresh on every call" invariant DescribeSpreadsheet's doc
// comment describes.
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

// readRange is ReadRange minus client construction and the allowlist check
// (already done by the caller), so readrange_test.go can drive it against
// a mocked sheetsAPI. Order of operations, and why:
//
//  1. SpreadsheetMeta — needed both to resolve an omitted sheet name to
//     the spreadsheet's first tab and to validate an explicit one against
//     the spreadsheet's real tabs (ErrSheetNotFound), exactly like
//     queryRows. For a row-only range, the same call also supplies the
//     sheet's actual column count, which is what "every column" resolves
//     to.
//  2. checkRangeSize is re-run here (having already run once, for the row
//     bound alone, in ValidateReadRangeInput) now that a row-only range's
//     column span is known — still strictly before Values.Get, so an
//     oversized resolved range is rejected before any data-bearing
//     upstream call, not after.
//  3. One RateCharger charge for the Values.Get call — never for the
//     SpreadsheetMeta call above, which rides the dispatch-time floor
//     enforce() already charges every tools/call, exactly as queryRows'
//     header-row fetch does (docs/07 step 8: "charge one unit via
//     RateCharger for the Values fetch"). An empty bucket here has no
//     partial answer to fall back to (unlike queryRows' scan), so it
//     returns RateLimitedError rather than a degraded result.
//  4. Values.Get, then checkEncodedSize against the returned (padded)
//     rows — the same 256KB cap sheets_query_rows enforces, generalized
//     out of query.go's checkResultSize so the constant and the check live
//     in exactly one place.
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
		// Row-only range ("1:100"): unbounded at parse time, resolved now
		// against the sheet's own reported width — the only source of
		// truth for what "every column" means for this specific sheet.
		startCol = 0
		endCol = sheetMeta.ColumnCount - 1
		if endCol < startCol {
			endCol = startCol // a sheet reporting 0 columns still reads as 1 column wide, never negative
		}
	}
	colSpan := endCol - startCol + 1
	rowSpan := parsed.RowSpan()
	if err := checkRangeSize(rowSpan, rowSpan*colSpan); err != nil {
		return nil, err
	}

	resolvedRange := formatA1Range(sheetName, startCol, endCol, parsed.StartRow, parsed.EndRow)

	if charger != nil {
		allowed, retryAfter, chargeErr := charger.ChargeRateLimit(ctx, connectorID, 1)
		if chargeErr != nil {
			return nil, fmt.Errorf("googlesheets: read range: charge rate limit: %w", chargeErr)
		}
		if !allowed {
			return nil, &RateLimitedError{RetryAfter: retryAfter}
		}
	}

	values, err := api.Values(ctx, in.SpreadsheetID, resolvedRange)
	if err != nil {
		return nil, fmt.Errorf("googlesheets: read range %q: %w", resolvedRange, err)
	}

	columns := make([]string, colSpan)
	for i := range columns {
		columns[i] = columnLetter(startCol + i)
	}

	// Rows are padded to colSpan-wide rectangles, not left ragged: Google
	// drops a row's trailing empty cells entirely rather than returning
	// them as "" (describe.go's sheetsAPI doc comment notes the same for
	// Values generally), so a ragged Rows would make "how many columns did
	// row i actually have" ambiguous — was column D really absent, or just
	// empty? — and would desynchronize row i from Columns' fixed width.
	// Padding trades that ambiguity for a predictable rectangle the caller
	// can always index Rows[i][j] against Columns[j] without a bounds
	// check.
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

// formatA1Range builds the concrete, fully-resolved A1-notation range
// string readRange sends to Values.Get and echoes back in
// ReadRangeOutput.Range — sheet name always quoted (valid even when
// unnecessary, matching a1RowRange's convention) and column/row bounds
// always explicit numbers, never "every column" left implicit, so a caller
// that omitted the sheet name or asked for a row-only range still gets
// back the exact range that was actually read.
func formatA1Range(sheetName string, startCol, endCol, startRow, endRow int) string {
	return fmt.Sprintf("%s!%s%d:%s%d", quoteSheetName(sheetName), columnLetter(startCol), startRow, columnLetter(endCol), endRow)
}
