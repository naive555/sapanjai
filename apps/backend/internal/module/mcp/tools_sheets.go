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
