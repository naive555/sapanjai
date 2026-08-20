// This file is docs/07-sheets-adapter-plan.md step 6: the first two tools
// that actually touch a customer's data, sheets_list_spreadsheets and
// sheets_describe_spreadsheet. Both are gated by connector.TypeGoogleSheets
// (Entry.ConnectorType) and PermissionSheetsRead, and both re-decrypt and
// re-parse their connector's config on every single invocation via
// Service.openGoogleSheetsConfig — never a value cached at connector-
// creation or session-start time — so a narrowed allowlist takes effect on
// the very next call (docs/07 step 5's design point, first exercised by a
// real tool here).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sapanjai/backend/internal/adapter/googlesheets"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/connector"
)

// PermissionSheetsRead is the RBAC action gating every sheets_* read tool
// (docs/06-sheets-adapter.md §5): "*" > "sheets:read" exact >
// "sheets:*" wildcard, the same ActionMatches semantics every other
// permission in this codebase uses. It has no dedicated permissions
// package to live in — connector's own PermissionRead/Write/Delete
// constants live in package connector because connector owns those REST
// routes, whereas sheets:read gates MCP tools, not a route, so it is
// declared here next to the tools it gates.
const PermissionSheetsRead = "sheets:read"

// maxSampleRows is sheets_describe_spreadsheet's include_sample_rows upper
// bound (docs/06-sheets-adapter.md §4.1: "int, 0-5, default 0").
const maxSampleRows = 5

var sheetsListSpreadsheetsEntry = Entry{
	Name:          "sheets_list_spreadsheets",
	Permission:    PermissionSheetsRead,
	Description:   sheetsListSpreadsheetsDescription,
	ConnectorType: connector.TypeGoogleSheets,
	Register:      registerListSpreadsheets,
}

var sheetsDescribeSpreadsheetEntry = Entry{
	Name:          "sheets_describe_spreadsheet",
	Permission:    PermissionSheetsRead,
	Description:   sheetsDescribeSpreadsheetDescription,
	ConnectorType: connector.TypeGoogleSheets,
	Register:      registerDescribeSpreadsheet,
}

var sheetsQueryRowsEntry = Entry{
	Name:          "sheets_query_rows",
	Permission:    PermissionSheetsRead,
	Description:   sheetsQueryRowsDescription,
	ConnectorType: connector.TypeGoogleSheets,
	Register:      registerQueryRows,
}

var sheetsReadRangeEntry = Entry{
	Name:          "sheets_read_range",
	Permission:    PermissionSheetsRead,
	Description:   sheetsReadRangeDescription,
	ConnectorType: connector.TypeGoogleSheets,
	Register:      registerReadRange,
}

// ---------------------------------------------------------------------------
// sheets_list_spreadsheets
// ---------------------------------------------------------------------------

const sheetsListSpreadsheetsDescription = "List every spreadsheet this connector's allowlist grants access to, " +
	"with each one's title. Call this first whenever you don't already know a spreadsheet's id: every other " +
	"sheets_* tool needs an id from this list. The Google account behind this connector may be able to reach " +
	"other spreadsheets too, but only the ones listed here can ever be read through this connector — everything " +
	"else is rejected, even if the OAuth token itself could technically open it."

type listSpreadsheetsOutput struct {
	Spreadsheets []spreadsheetSummaryOutput `json:"spreadsheets" jsonschema:"the spreadsheets this connector can read"`
}

type spreadsheetSummaryOutput struct {
	SpreadsheetID string `json:"spreadsheet_id" jsonschema:"the spreadsheet's Google id — pass this to sheets_describe_spreadsheet"`
	Title         string `json:"title" jsonschema:"the spreadsheet's display title; empty when accessible is false"`
	Accessible    bool   `json:"accessible" jsonschema:"false if this id is allowlisted but can no longer be read (a revoked share or a deleted file) — treat it as unusable rather than retrying"`
}

func registerListSpreadsheets(s *gomcp.Server, svc *Service, conn db.Connector) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "sheets_list_spreadsheets",
		Description: sheetsListSpreadsheetsDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, listSpreadsheetsOutput, error) {
		cfg, err := svc.openGoogleSheetsConfig(ctx, conn)
		if err != nil {
			return ErrorResult(err), listSpreadsheetsOutput{}, nil
		}

		ts := svc.sheetsTokens.Get(ctx, conn.ID, cfg.OAuth)
		result, err := googlesheets.ListSpreadsheets(ctx, ts, cfg)
		if err != nil {
			return ErrorResult(err), listSpreadsheetsOutput{}, nil
		}

		out := listSpreadsheetsOutput{Spreadsheets: make([]spreadsheetSummaryOutput, 0, len(result.Spreadsheets))}
		for _, sp := range result.Spreadsheets {
			out.Spreadsheets = append(out.Spreadsheets, spreadsheetSummaryOutput{
				SpreadsheetID: sp.SpreadsheetID,
				Title:         sp.Title,
				Accessible:    sp.Accessible,
			})
		}
		return nil, out, nil
	})
}

// ---------------------------------------------------------------------------
// sheets_describe_spreadsheet
// ---------------------------------------------------------------------------

const sheetsDescribeSpreadsheetDescription = "Describe one spreadsheet's structure: its title, every tab's name " +
	"and row count, and each tab's column headers, optionally with a few sample data rows. Google Sheets has no " +
	"schema, so call this before sheets_query_rows or sheets_read_range — guessing a tab name or column will " +
	"fail. spreadsheet_id must be one this connector's allowlist grants access to; call sheets_list_spreadsheets " +
	"first if you don't already have one."

type describeSpreadsheetInput struct {
	SpreadsheetID     string `json:"spreadsheet_id" jsonschema:"the spreadsheet id to describe, from sheets_list_spreadsheets; must be on this connector's allowlist"`
	IncludeSampleRows int    `json:"include_sample_rows,omitempty" jsonschema:"how many example data rows to include per tab, 0-5 (default 0); a few samples help you learn a column's actual format (date style, id prefix, ...) before writing a filter"`
}

type describeSpreadsheetOutput struct {
	SpreadsheetID string                   `json:"spreadsheet_id"`
	Title         string                   `json:"title"`
	Sheets        []sheetDescriptionOutput `json:"sheets"`
}

type sheetDescriptionOutput struct {
	Name        string                    `json:"name"`
	RowCount    int                       `json:"row_count"`
	ColumnCount int                       `json:"column_count"`
	Columns     []columnDescriptionOutput `json:"columns"`
	SampleRows  [][]string                `json:"sample_rows"`
}

type columnDescriptionOutput struct {
	Index  int    `json:"index"`
	Letter string `json:"letter"`
	Header string `json:"header"`
}

func registerDescribeSpreadsheet(s *gomcp.Server, svc *Service, conn db.Connector) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "sheets_describe_spreadsheet",
		Description: sheetsDescribeSpreadsheetDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *gomcp.CallToolRequest, in describeSpreadsheetInput) (*gomcp.CallToolResult, describeSpreadsheetOutput, error) {
		// Bounds- and presence-check the input before touching config
		// decryption or the network at all: a caller must get a clean,
		// immediate rejection for a malformed request regardless of
		// whether the connector's credentials even work.
		if in.IncludeSampleRows < 0 || in.IncludeSampleRows > maxSampleRows {
			return invalidSampleRowCount(in.IncludeSampleRows), describeSpreadsheetOutput{}, nil
		}
		if in.SpreadsheetID == "" {
			return missingSpreadsheetID(), describeSpreadsheetOutput{}, nil
		}

		cfg, err := svc.openGoogleSheetsConfig(ctx, conn)
		if err != nil {
			return ErrorResult(err), describeSpreadsheetOutput{}, nil
		}
		if !cfg.IsSpreadsheetAllowed(in.SpreadsheetID) {
			return SpreadsheetNotAllowed(in.SpreadsheetID), describeSpreadsheetOutput{}, nil
		}

		ts := svc.sheetsTokens.Get(ctx, conn.ID, cfg.OAuth)
		result, err := googlesheets.DescribeSpreadsheet(ctx, ts, cfg, in.SpreadsheetID, in.IncludeSampleRows)
		if err != nil {
			return ErrorResult(err), describeSpreadsheetOutput{}, nil
		}

		out := describeSpreadsheetOutput{
			SpreadsheetID: result.SpreadsheetID,
			Title:         result.Title,
			Sheets:        make([]sheetDescriptionOutput, 0, len(result.Sheets)),
		}
		for _, sh := range result.Sheets {
			columns := make([]columnDescriptionOutput, 0, len(sh.Columns))
			for _, c := range sh.Columns {
				columns = append(columns, columnDescriptionOutput{Index: c.Index, Letter: c.Letter, Header: c.Header})
			}
			out.Sheets = append(out.Sheets, sheetDescriptionOutput{
				Name:        sh.Name,
				RowCount:    sh.RowCount,
				ColumnCount: sh.ColumnCount,
				Columns:     columns,
				SampleRows:  sh.SampleRows,
			})
		}
		return nil, out, nil
	})
}

// ---------------------------------------------------------------------------
// sheets_query_rows
// ---------------------------------------------------------------------------

// sheetsQueryRowsDescription is deliberate prompt surface (docs/07-sheets-
// adapter-plan.md step 7's "budget review time for their wording"): it
// tells the model both how to use the filter DSL and how to read a partial
// answer, since docs/06-sheets-adapter.md §4.2's exact "total" is
// unbuildable at scale (Decision 4, docs/07 step 7) and an agent that
// doesn't know its count is a floor will confidently over-claim.
const sheetsQueryRowsDescription = "Filter and page through one sheet's data rows by column — the main way to answer " +
	"questions like \"which contracts have status=draft\" without reading the whole sheet. Always call " +
	"sheets_describe_spreadsheet first to learn the sheet's actual column headers; filters and columns here must " +
	"name headers exactly as it reports them. Filters AND together; supported operators are eq, neq, contains, gt, " +
	"lt, gte, lte, and in (in takes an array value) — there is no free-form query language, so a filter value is " +
	"always compared as plain text or a plain number, never evaluated as a formula or expression, even if it looks " +
	"like one. Use columns to return only the fields you need — this both reduces what you have to read and helps " +
	"avoid RESULT_TOO_LARGE on a wide sheet. limit is 1-200 (default 50); use offset with next_offset to page " +
	"through more results. IMPORTANT: this tool scans a bounded number of rows per call rather than the whole " +
	"sheet, so its counts can be a lower bound. Check scan_complete: when true, count/scanned_rows/total (if " +
	"present) are exact and cover the entire sheet. When false, the scan stopped early (enough matches already " +
	"found, the per-call scan budget, or the connector's rate-limit budget) — treat count as \"at least this many\" " +
	"rather than a final answer, there is no total field at all, and you should narrow the filter (or, if you " +
	"specifically need more of what's already matched, increase offset) rather than assume nothing else matches."

type queryRowsFilterInput struct {
	Column string `json:"column" jsonschema:"a column header name, exactly as sheets_describe_spreadsheet reports it"`
	Op     string `json:"op" jsonschema:"comparison operator: eq, neq, contains, gt, lt, gte, lte, or in"`
	// Value is intentionally `any`: a string or number for every operator
	// except "in", which requires an array. It is always compared as plain
	// data — never interpolated into any Google API call or evaluated as a
	// formula, regardless of what it looks like (docs/06-sheets-adapter.md
	// §6).
	Value any `json:"value" jsonschema:"the value to compare against (a string, number, or bool); for op=\"in\" this must be an array of such values. Always treated as a literal, never as a formula or expression"`
}

type queryRowsInput struct {
	SpreadsheetID  string                 `json:"spreadsheet_id" jsonschema:"the spreadsheet id, from sheets_list_spreadsheets; must be on this connector's allowlist"`
	SheetName      string                 `json:"sheet_name" jsonschema:"the tab name to query, from sheets_describe_spreadsheet"`
	Filters        []queryRowsFilterInput `json:"filters,omitempty" jsonschema:"AND-ed conditions on column values; omit or leave empty to match every row"`
	Columns        []string               `json:"columns,omitempty" jsonschema:"which column headers to return per row, in this order; omit for every column (projecting fewer columns both reduces what you read and helps avoid RESULT_TOO_LARGE)"`
	Limit          int                    `json:"limit,omitempty" jsonschema:"how many matching rows to return, 1-200, default 50"`
	Offset         int                    `json:"offset,omitempty" jsonschema:"how many matching rows to skip before returning limit more, default 0; use next_offset from a previous call to page forward"`
	ResponseFormat string                 `json:"response_format,omitempty" jsonschema:"\"markdown\" (a readable table, default) or \"json\" (structured rows) for how the matched rows are rendered in this result's text"`
}

// queryRowsOutput is sheets_query_rows' result shape — docs/07 step 7's
// Decision 4, replacing docs/06 §4.2's unconditional (and, at scale,
// unbuildable) "total" with a bounded scan's honest signal. Total is
// present only when ScanComplete is true, in which case it is exact.
type queryRowsOutput struct {
	Count        int                 `json:"count" jsonschema:"how many rows this call is returning (after offset/limit windowing)"`
	Offset       int                 `json:"offset"`
	HasMore      bool                `json:"has_more" jsonschema:"true if at least one more matching row is already known beyond this page — call again with next_offset to see it"`
	NextOffset   int                 `json:"next_offset" jsonschema:"the offset to use for the next page, when has_more is true"`
	ScannedRows  int                 `json:"scanned_rows" jsonschema:"how many of the sheet's data rows this call actually evaluated"`
	ScanComplete bool                `json:"scan_complete" jsonschema:"true only if the scan reached the sheet's real end within this call's budget — when false, count/total are a lower bound, not the final answer"`
	Total        *int                `json:"total,omitempty" jsonschema:"the exact count of all matching rows in the whole sheet — present only when scan_complete is true"`
	Rows         []map[string]string `json:"rows"`
}

func registerQueryRows(s *gomcp.Server, svc *Service, conn db.Connector) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "sheets_query_rows",
		Description: sheetsQueryRowsDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *gomcp.CallToolRequest, in queryRowsInput) (*gomcp.CallToolResult, queryRowsOutput, error) {
		// Presence/bounds/shape checks first, before any config
		// decryption or network call, exactly like
		// sheets_describe_spreadsheet's include_sample_rows check.
		if in.SpreadsheetID == "" {
			return missingSpreadsheetID(), queryRowsOutput{}, nil
		}
		if in.SheetName == "" {
			return missingSheetName(), queryRowsOutput{}, nil
		}
		responseFormat := in.ResponseFormat
		if responseFormat == "" {
			responseFormat = "markdown"
		}
		if responseFormat != "markdown" && responseFormat != "json" {
			return invalidResponseFormat(responseFormat), queryRowsOutput{}, nil
		}
		limit := in.Limit
		if limit == 0 {
			limit = googlesheets.DefaultLimit
		}

		filters := make([]googlesheets.Filter, 0, len(in.Filters))
		for _, f := range in.Filters {
			filters = append(filters, googlesheets.Filter{
				Column: f.Column,
				Op:     googlesheets.Operator(f.Op),
				Value:  f.Value,
			})
		}
		adapterInput := googlesheets.QueryRowsInput{
			SpreadsheetID: in.SpreadsheetID,
			SheetName:     in.SheetName,
			Filters:       filters,
			Columns:       in.Columns,
			Limit:         limit,
			Offset:        in.Offset,
		}
		if err := googlesheets.ValidateQueryRowsInput(adapterInput); err != nil {
			return invalidQueryRowsInput(err), queryRowsOutput{}, nil
		}

		cfg, err := svc.openGoogleSheetsConfig(ctx, conn)
		if err != nil {
			return ErrorResult(err), queryRowsOutput{}, nil
		}
		if !cfg.IsSpreadsheetAllowed(in.SpreadsheetID) {
			return SpreadsheetNotAllowed(in.SpreadsheetID), queryRowsOutput{}, nil
		}

		ts := svc.sheetsTokens.Get(ctx, conn.ID, cfg.OAuth)
		// svc itself satisfies googlesheets.RateCharger (ChargeRateLimit
		// has the exact signature the adapter's scan loop needs) — this is
		// the seam step 4 built for exactly this call, docs/07 step 7:
		// "charge one unit per page fetched, not one per tool call."
		result, err := googlesheets.QueryRows(ctx, ts, cfg, conn.ID, svc, adapterInput)
		if err != nil {
			switch {
			case errors.Is(err, googlesheets.ErrSheetNotFound):
				return SheetNotFound(in.SheetName), queryRowsOutput{}, nil
			case errors.Is(err, googlesheets.ErrColumnNotFound):
				return ColumnNotFound(columnNameFromError(err)), queryRowsOutput{}, nil
			case errors.Is(err, googlesheets.ErrResultTooLarge):
				return ResultTooLarge(), queryRowsOutput{}, nil
			default:
				return ErrorResult(err), queryRowsOutput{}, nil
			}
		}

		out := queryRowsOutput{
			Count:        result.Count,
			Offset:       result.Offset,
			HasMore:      result.HasMore,
			NextOffset:   result.NextOffset,
			ScannedRows:  result.ScannedRows,
			ScanComplete: result.ScanComplete,
			Total:        result.Total,
			Rows:         result.Rows,
		}
		return buildQueryRowsResult(result, out, responseFormat), out, nil
	})
}

// columnNameFromError recovers the offending column name from an error
// wrapping googlesheets.ErrColumnNotFound, built as
// fmt.Errorf("%w: %s", ErrColumnNotFound, columnName) — trimming the
// sentinel's own message plus the ": " separator that format string always
// produces leaves exactly the column name.
func columnNameFromError(err error) string {
	return strings.TrimPrefix(err.Error(), googlesheets.ErrColumnNotFound.Error()+": ")
}

// buildQueryRowsResult renders result as the CallToolResult text the model
// reads: a markdown table (default) or the JSON-encoded out DTO, per
// responseFormat. Either way, when the scan did not reach the sheet's real
// end (ScanComplete false), a second text block spells out that the count
// is a lower bound and names the fix (docs/07 step 7's Decision 4) — kept
// as a *second* content block rather than woven into the JSON text so a
// json-format caller can still parse that first block as pure JSON.
func buildQueryRowsResult(result *googlesheets.QueryRowsOutput, out queryRowsOutput, responseFormat string) *gomcp.CallToolResult {
	var content []gomcp.Content
	switch responseFormat {
	case "json":
		encoded, err := json.Marshal(out)
		if err != nil {
			// Unreachable in practice (out's fields are all
			// trivially-marshalable primitives and string maps), but fail
			// safe rather than panic: StructuredContent is still set
			// correctly by the SDK from the typed return value regardless
			// of this Content block.
			encoded = []byte("{}")
		}
		content = []gomcp.Content{&gomcp.TextContent{Text: string(encoded)}}
	default: // "markdown"
		content = []gomcp.Content{&gomcp.TextContent{Text: renderQueryRowsMarkdown(result)}}
	}
	if !result.ScanComplete {
		content = append(content, &gomcp.TextContent{Text: queryRowsPartialScanWarning(result)})
	}
	return &gomcp.CallToolResult{Content: content}
}

// renderQueryRowsMarkdown builds a markdown table from result's projected
// columns and rows, followed by a one-line summary of the pagination/scan
// signal fields. Every cell is escaped so a value containing "|" or a
// newline can't corrupt the table structure — the same "never let
// agent-controlled data control formatting" discipline as the formula-
// injection guardrail, just applied to markdown instead of a query
// language.
func renderQueryRowsMarkdown(result *googlesheets.QueryRowsOutput) string {
	var b strings.Builder
	if len(result.Columns) == 0 || len(result.Rows) == 0 {
		b.WriteString("No matching rows.\n\n")
	} else {
		b.WriteString("| " + strings.Join(result.Columns, " | ") + " |\n")
		b.WriteString("|" + strings.Repeat(" --- |", len(result.Columns)) + "\n")
		for _, row := range result.Rows {
			cells := make([]string, len(result.Columns))
			for i, c := range result.Columns {
				cells[i] = escapeMarkdownCell(row[c])
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b,
		"count=%d offset=%d has_more=%v next_offset=%d scanned_rows=%d scan_complete=%v",
		result.Count, result.Offset, result.HasMore, result.NextOffset, result.ScannedRows, result.ScanComplete)
	if result.Total != nil {
		fmt.Fprintf(&b, " total=%d", *result.Total)
	}
	b.WriteString("\n")
	return b.String()
}

func escapeMarkdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// queryRowsPartialScanWarning is the text docs/07 step 7's Decision 4
// requires: when scan_complete is false, both the tool description and the
// result text must tell the agent the answer is partial and to narrow the
// filter.
func queryRowsPartialScanWarning(result *googlesheets.QueryRowsOutput) string {
	return fmt.Sprintf(
		"Partial result: this call scanned %d rows and stopped before reaching the sheet's end (enough matches "+
			"already found, this call's scan budget, or the connector's rate-limit budget) — there is no total, "+
			"and count/scanned_rows are a lower bound, not a final answer. Narrow your filter to get a complete "+
			"answer, or call again with offset=%d to see more of what has already matched.",
		result.ScannedRows, result.NextOffset)
}

// ---------------------------------------------------------------------------
// sheets_read_range
// ---------------------------------------------------------------------------

// sheetsReadRangeDescription is deliberate prompt surface, written with the
// same care as sheetsQueryRowsDescription (docs/07-sheets-adapter-plan.md
// step 8): it steers the model toward sheets_query_rows for anything the
// filter DSL can express, spells out the one hard constraint on range
// (explicit row bounds on both ends — anything else risks reading a whole
// sheet, docs/06 §6's "life-or-death at 87GB"), and tells it how an
// omitted sheet name resolves so a partial range still produces a
// predictable read.
const sheetsReadRangeDescription = "The escape hatch for what sheets_query_rows' filter DSL can't express: reads " +
	"an explicit A1 range directly, with no filtering and no column projection. Prefer sheets_query_rows for " +
	"anything expressible as a column filter — it pages, filters, and caps its output for you; use this tool only " +
	"when you need an arbitrary block of cells (a header plus data, a specific region) the DSL cannot select. " +
	"range must be A1 notation with explicit numeric row bounds on both ends, e.g. \"Contracts!A1:D100\" or " +
	"\"Contracts!1:50\" (every column, rows 1-50) — a bare sheet name, a column-only range like \"A:D\", or " +
	"anything else without row bounds is rejected, because that could mean reading the entire sheet. Call " +
	"sheets_describe_spreadsheet first to learn tab names; if you omit the sheet name from range, the " +
	"spreadsheet's first tab is used, and the result's range field always echoes back exactly what was resolved " +
	"and read. A single call is capped at 1,000 rows and 20,000 cells — narrow the range and call again to read " +
	"more."

type readRangeInput struct {
	SpreadsheetID string `json:"spreadsheet_id" jsonschema:"the spreadsheet id, from sheets_list_spreadsheets; must be on this connector's allowlist"`
	Range         string `json:"range" jsonschema:"an A1-notation range with explicit numeric row bounds on both ends, e.g. \"Contracts!A1:D100\" or \"Contracts!1:50\"; a bare sheet name, a column-only range (\"A:D\"), or anything without row bounds is rejected. Omit the sheet name to read from the spreadsheet's first tab."`
}

// readRangeOutput is sheets_read_range's result shape (docs/07 step 8).
// Range is always the fully resolved range actually read (sheet name
// filled in, column bounds resolved for a row-only request), never the
// caller's raw string, so the model always knows exactly what it got back
// even when its own request left something implicit. RowCount is tagged
// "row_count", not "count" — service.go's rowCountFromResult probes both
// field names for exactly this reason, so this tool's audit row still
// carries a row_count without widening that extractor to anything beyond
// another integer count.
type readRangeOutput struct {
	SpreadsheetID string     `json:"spreadsheet_id"`
	Range         string     `json:"range" jsonschema:"the fully resolved range actually read — sheet name and column bounds filled in even if range omitted them"`
	SheetName     string     `json:"sheet_name"`
	Columns       []string   `json:"columns" jsonschema:"the A1 column letter for each column in rows, in order"`
	Rows          [][]string `json:"rows" jsonschema:"the range's cell values as text, one row per array; every row is padded to the same width as columns"`
	RowCount      int        `json:"row_count"`
	ColumnCount   int        `json:"column_count"`
}

func registerReadRange(s *gomcp.Server, svc *Service, conn db.Connector) {
	gomcp.AddTool(s, &gomcp.Tool{
		Name:        "sheets_read_range",
		Description: sheetsReadRangeDescription,
		Annotations: &gomcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *gomcp.CallToolRequest, in readRangeInput) (*gomcp.CallToolResult, readRangeOutput, error) {
		// Presence checks first, before any parsing, config decryption, or
		// network call — same discipline as registerQueryRows's
		// SpreadsheetID/SheetName checks.
		if in.SpreadsheetID == "" {
			return missingSpreadsheetID(), readRangeOutput{}, nil
		}
		if strings.TrimSpace(in.Range) == "" {
			return missingRange(), readRangeOutput{}, nil
		}

		// ValidateReadRangeInput parses range (docs/07 step 8: "the A1
		// range must be parsed and re-validated ... not a trusted
		// identifier") and checks it for malformed syntax, unbounded row
		// extent, and the pre-fetch row/cell cap — entirely networkless,
		// so a bad range never spends a byte of the connector's OAuth
		// credential or rate-limit budget, exactly like
		// ValidateQueryRowsInput above.
		adapterInput := googlesheets.ReadRangeInput{SpreadsheetID: in.SpreadsheetID, RangeStr: in.Range}
		parsed, err := googlesheets.ValidateReadRangeInput(adapterInput)
		if err != nil {
			return invalidReadRangeInput(err), readRangeOutput{}, nil
		}

		cfg, err := svc.openGoogleSheetsConfig(ctx, conn)
		if err != nil {
			return ErrorResult(err), readRangeOutput{}, nil
		}
		if !cfg.IsSpreadsheetAllowed(in.SpreadsheetID) {
			return SpreadsheetNotAllowed(in.SpreadsheetID), readRangeOutput{}, nil
		}

		ts := svc.sheetsTokens.Get(ctx, conn.ID, cfg.OAuth)
		// svc satisfies googlesheets.RateCharger, same seam sheets_query_rows
		// uses; read_range charges it exactly once, for the single
		// Values.Get call (docs/07 step 8: "charge one unit via
		// RateCharger for the Values fetch").
		result, err := googlesheets.ReadRange(ctx, ts, cfg, conn.ID, svc, adapterInput, parsed)
		if err != nil {
			var rlErr *googlesheets.RateLimitedError
			switch {
			case errors.Is(err, googlesheets.ErrSheetNotFound):
				return SheetNotFound(sheetNameFromReadRangeError(err)), readRangeOutput{}, nil
			case errors.Is(err, googlesheets.ErrResultTooLarge):
				return ResultTooLarge(), readRangeOutput{}, nil
			case errors.As(err, &rlErr):
				// read_range has no partial answer to fall back to
				// (contrast sheets_query_rows' scan, which ends cleanly
				// with ScanComplete: false) — an exhausted bucket is
				// always a full denial here.
				return RateLimited(rlErr.RetryAfter), readRangeOutput{}, nil
			default:
				return ErrorResult(err), readRangeOutput{}, nil
			}
		}

		out := readRangeOutput{
			SpreadsheetID: result.SpreadsheetID,
			Range:         result.Range,
			SheetName:     result.SheetName,
			Columns:       result.Columns,
			Rows:          result.Rows,
			RowCount:      result.RowCount,
			ColumnCount:   result.ColumnCount,
		}
		return buildReadRangeResult(result), out, nil
	})
}

// sheetNameFromReadRangeError recovers the offending sheet name from an
// error wrapping googlesheets.ErrSheetNotFound, built as
// fmt.Errorf("%w: %s", ErrSheetNotFound, sheetName) — the same trick as
// columnNameFromError above. sheets_read_range needs this (unlike
// sheets_query_rows, which already has sheet_name as a separate input
// field to fall back on) because its only sheet-name signal is whatever
// ParseA1Range extracted from range itself.
func sheetNameFromReadRangeError(err error) string {
	return strings.TrimPrefix(err.Error(), googlesheets.ErrSheetNotFound.Error()+": ")
}

// buildReadRangeResult renders result as the CallToolResult text the model
// reads: a markdown table of the resolved range's cells, using the same
// per-cell escaping discipline (escapeMarkdownCell) renderQueryRowsMarkdown
// uses, so a cell containing "|" or a newline can't corrupt the table.
func buildReadRangeResult(result *googlesheets.ReadRangeOutput) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: renderReadRangeMarkdown(result)}}}
}

// renderReadRangeMarkdown builds a markdown table headed by each column's
// A1 letter (result.Columns), followed by a one-line summary naming the
// resolved range, sheet, and dimensions actually read.
func renderReadRangeMarkdown(result *googlesheets.ReadRangeOutput) string {
	var b strings.Builder
	if result.RowCount == 0 || result.ColumnCount == 0 {
		fmt.Fprintf(&b, "No cells in %s (sheet %q).\n", result.Range, result.SheetName)
		return b.String()
	}
	b.WriteString("| " + strings.Join(result.Columns, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat(" --- |", len(result.Columns)) + "\n")
	for _, row := range result.Rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = escapeMarkdownCell(c)
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	fmt.Fprintf(&b, "\nrange=%s sheet=%q row_count=%d column_count=%d\n", result.Range, result.SheetName, result.RowCount, result.ColumnCount)
	return b.String()
}
