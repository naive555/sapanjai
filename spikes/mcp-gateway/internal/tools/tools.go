// Package tools is the spike's tool catalog. It is the answer to task item 3:
// every MCP tool is declared alongside the controlplane RBAC action that
// gates it, so "which tools does this agent session see?" is a pure function
// of (principal, catalog) and never a hand-maintained list.
//
// The catalog is the single source of truth consumed by three places:
//   - gateway.BuildServer, which registers only the permitted tools;
//   - gateway.enforce, the belt-and-braces tools/call check;
//   - the docs, which render the table from Catalog().
package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/junctera/spikes/mcp-gateway/internal/mockdata"
	"github.com/junctera/spikes/mcp-gateway/internal/rbac"
)

// Entry binds one MCP tool to one RBAC action.
//
// Register closes over the principal rather than reading it from the request
// context. That is deliberate: the org id used for every upstream read comes
// from the session's authenticated principal, so there is no code path in
// which a model-supplied argument can widen the tenant scope. A tool cannot
// "forget" to scope itself.
type Entry struct {
	// Name is the MCP tool name as the model sees it.
	Name string
	// Permission is the controlplane RBAC action required to see and call
	// this tool, in the same "<resource>:<verb>" shape as
	// RequirePermission("invoice:read").
	Permission string
	// Description is mirrored into the mcp.Tool for the model.
	Description string
	// Register adds the tool to s, bound to p.
	Register func(s *mcp.Server, p *rbac.Principal)
}

// Catalog returns every tool the gateway knows how to expose, permitted or
// not. Order is stable so tools/list output is deterministic.
func Catalog() []Entry {
	return []Entry{
		{
			Name:        "list_invoices",
			Permission:  "invoice:read",
			Description: "List invoices for the current organization, optionally filtered by status.",
			Register:    registerListInvoices,
		},
		{
			Name:        "get_invoice_by_id",
			Permission:  "invoice:read",
			Description: "Fetch a single invoice by its id, scoped to the current organization.",
			Register:    registerGetInvoice,
		},
		{
			Name:        "create_invoice",
			Permission:  "invoice:write",
			Description: "Create a new draft invoice in the current organization.",
			Register:    registerCreateInvoice,
		},
	}
}

// PermissionFor returns the RBAC action gating tool name, and whether the
// name is in the catalog at all.
func PermissionFor(name string) (string, bool) {
	for _, e := range Catalog() {
		if e.Name == name {
			return e.Permission, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// list_invoices
// ---------------------------------------------------------------------------

type listInvoicesInput struct {
	Status string `json:"status,omitempty" jsonschema:"filter by status: draft, sent, paid, or overdue"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of invoices to return"`
}

type listInvoicesOutput struct {
	Invoices []mockdata.Invoice `json:"invoices" jsonschema:"the matching invoices"`
	Count    int                `json:"count" jsonschema:"number of invoices returned"`
}

func registerListInvoices(s *mcp.Server, p *rbac.Principal) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_invoices",
		Description: "List invoices for the current organization, optionally filtered by status.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in listInvoicesInput) (*mcp.CallToolResult, listInvoicesOutput, error) {
		// p.OrgID, not an argument: tenant scope is not negotiable by the model.
		found := mockdata.ListInvoices(p.OrgID, in.Status, in.Limit)
		return nil, listInvoicesOutput{Invoices: found, Count: len(found)}, nil
	})
}

// ---------------------------------------------------------------------------
// get_invoice_by_id
// ---------------------------------------------------------------------------

type getInvoiceInput struct {
	InvoiceID string `json:"invoiceId" jsonschema:"the invoice id, e.g. inv_1001"`
}

func registerGetInvoice(s *mcp.Server, p *rbac.Principal) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_invoice_by_id",
		Description: "Fetch a single invoice by its id, scoped to the current organization.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in getInvoiceInput) (*mcp.CallToolResult, mockdata.Invoice, error) {
		inv, err := mockdata.GetInvoice(p.OrgID, in.InvoiceID)
		if err != nil {
			// A tool-level failure, not a protocol error: return IsError so
			// the model can see and recover from it. Protocol errors (the
			// third return value) abort the turn instead.
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, mockdata.Invoice{}, nil
		}
		return nil, inv, nil
	})
}

// ---------------------------------------------------------------------------
// create_invoice — the write tool, present to prove read/write split
// ---------------------------------------------------------------------------

type createInvoiceInput struct {
	CustomerName  string  `json:"customerName" jsonschema:"customer legal name"`
	CustomerTaxID string  `json:"customerTaxId" jsonschema:"13-digit Thai tax identification number"`
	Subtotal      float64 `json:"subtotal" jsonschema:"amount in THB before 7% VAT"`
	DueDate       string  `json:"dueDate" jsonschema:"payment due date, YYYY-MM-DD"`
}

func registerCreateInvoice(s *mcp.Server, p *rbac.Principal) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_invoice",
		Description: "Create a new draft invoice in the current organization.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false)},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in createInvoiceInput) (*mcp.CallToolResult, mockdata.Invoice, error) {
		if in.CustomerName == "" {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "customerName is required"}},
			}, mockdata.Invoice{}, nil
		}
		if in.Subtotal <= 0 {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("subtotal must be positive, got %v", in.Subtotal)}},
			}, mockdata.Invoice{}, nil
		}
		return nil, mockdata.CreateInvoice(p.OrgID, in.CustomerName, in.CustomerTaxID, in.Subtotal, in.DueDate), nil
	})
}

func ptr[T any](v T) *T { return &v }
