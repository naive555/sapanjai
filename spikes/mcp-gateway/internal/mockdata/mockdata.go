// Package mockdata is the stand-in for a real Thai accounting/ERP connector
// (PEAK, FlowAccount, Xero TH, SAP B1). It holds hardcoded invoices for two
// organizations so the spike can prove tenant isolation: every read is keyed
// by an organization id that comes from the caller's auth context, never from
// a tool argument.
package mockdata

import "fmt"

// Invoice is a deliberately Thai-shaped invoice: THB amounts, a 13-digit
// juristic-person tax id, and VAT broken out at 7%.
type Invoice struct {
	ID             string  `json:"id" jsonschema:"invoice id"`
	OrganizationID string  `json:"organizationId" jsonschema:"owning organization id"`
	Number         string  `json:"number" jsonschema:"human-readable invoice number"`
	CustomerName   string  `json:"customerName" jsonschema:"customer legal name"`
	CustomerTaxID  string  `json:"customerTaxId" jsonschema:"13-digit Thai tax identification number"`
	IssueDate      string  `json:"issueDate" jsonschema:"issue date, YYYY-MM-DD"`
	DueDate        string  `json:"dueDate" jsonschema:"payment due date, YYYY-MM-DD"`
	Currency       string  `json:"currency" jsonschema:"ISO 4217 currency code"`
	Subtotal       float64 `json:"subtotal" jsonschema:"amount before VAT"`
	VatRate        float64 `json:"vatRate" jsonschema:"VAT rate as a decimal, e.g. 0.07"`
	VatAmount      float64 `json:"vatAmount" jsonschema:"VAT charged"`
	Total          float64 `json:"total" jsonschema:"total payable including VAT"`
	Status         string  `json:"status" jsonschema:"one of: draft, sent, paid, overdue"`
}

// Organization ids used throughout the spike. In the real gateway these are
// the uuid PKs of controlplane's organizations table.
const (
	OrgSiamTrading = "org_11111111-1111-1111-1111-111111111111"
	OrgBangkokLogi = "org_22222222-2222-2222-2222-222222222222"
)

// invoices is the fake upstream. Keyed by organization id so that a lookup
// without an org id is not expressible.
var invoices = map[string][]Invoice{
	OrgSiamTrading: {
		newInvoice("inv_1001", OrgSiamTrading, "INV-2026-0001", "บริษัท เจริญกิจ จำกัด", "0105558001234",
			"2026-06-01", "2026-06-30", 125000, "paid"),
		newInvoice("inv_1002", OrgSiamTrading, "INV-2026-0002", "Thanawat Engineering Co., Ltd.", "0105561009876",
			"2026-06-14", "2026-07-14", 48200, "sent"),
		newInvoice("inv_1003", OrgSiamTrading, "INV-2026-0003", "Lotus Retail Group PCL", "0107537000123",
			"2026-05-02", "2026-06-01", 310500, "overdue"),
	},
	OrgBangkokLogi: {
		newInvoice("inv_2001", OrgBangkokLogi, "BKL-2026-0044", "Siam Cold Chain Co., Ltd.", "0105549004567",
			"2026-07-01", "2026-07-31", 87600, "sent"),
		newInvoice("inv_2002", OrgBangkokLogi, "BKL-2026-0045", "Andaman Freight Partners", "0835560001122",
			"2026-07-05", "2026-08-04", 22400, "draft"),
	},
}

// newInvoice fills in the VAT arithmetic so the fixtures stay internally
// consistent (subtotal + 7% VAT = total).
func newInvoice(id, orgID, number, customer, taxID, issued, due string, subtotal float64, status string) Invoice {
	const vatRate = 0.07
	vat := round2(subtotal * vatRate)
	return Invoice{
		ID:             id,
		OrganizationID: orgID,
		Number:         number,
		CustomerName:   customer,
		CustomerTaxID:  taxID,
		IssueDate:      issued,
		DueDate:        due,
		Currency:       "THB",
		Subtotal:       round2(subtotal),
		VatRate:        vatRate,
		VatAmount:      vat,
		Total:          round2(subtotal + vat),
		Status:         status,
	}
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// ListInvoices returns orgID's invoices, optionally filtered by status and
// capped at limit (limit <= 0 means no cap). An unknown orgID yields an empty,
// non-nil slice rather than an error — the caller has already been authorized
// for *some* org, and leaking "this org exists but has no data" vs "this org
// does not exist" is a tenant-enumeration hint we do not want.
func ListInvoices(orgID, status string, limit int) []Invoice {
	out := []Invoice{}
	for _, inv := range invoices[orgID] {
		if status != "" && inv.Status != status {
			continue
		}
		out = append(out, inv)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// GetInvoice returns the invoice with id invoiceID *within orgID*. The org
// scope is part of the lookup, not a post-filter, so a caller in org A cannot
// read org B's invoice by guessing its id.
func GetInvoice(orgID, invoiceID string) (Invoice, error) {
	for _, inv := range invoices[orgID] {
		if inv.ID == invoiceID {
			return inv, nil
		}
	}
	return Invoice{}, fmt.Errorf("invoice %q not found", invoiceID)
}

// CreateInvoice appends a draft invoice to orgID's list. It mutates package
// state and is not safe for concurrent use — acceptable for a spike whose
// point is the protocol and authorization path, not the data layer.
func CreateInvoice(orgID, customerName, customerTaxID string, subtotal float64, dueDate string) Invoice {
	seq := len(invoices[orgID]) + 1
	inv := newInvoice(
		fmt.Sprintf("inv_%s_%d", shortOrg(orgID), seq),
		orgID,
		fmt.Sprintf("DRAFT-%04d", seq),
		customerName,
		customerTaxID,
		"2026-08-05",
		dueDate,
		subtotal,
		"draft",
	)
	invoices[orgID] = append(invoices[orgID], inv)
	return inv
}

func shortOrg(orgID string) string {
	if len(orgID) >= 8 {
		return orgID[4:8]
	}
	return orgID
}
