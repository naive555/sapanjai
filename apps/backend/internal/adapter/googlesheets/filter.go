package googlesheets

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Operator is one of the eight comparison operators sheets_query_rows'
// filter DSL supports (docs/06-sheets-adapter.md §4.2). This is the
// entirety of the filter surface — there is deliberately no ninth "raw
// expression" or "formula" operator: docs/06 §6 rules out ever exposing
// Google Visualization Query Language to the model, and a fixed, closed set
// of operators evaluated in our own code is what makes that guarantee
// checkable rather than aspirational.
type Operator string

const (
	OpEq       Operator = "eq"
	OpNeq      Operator = "neq"
	OpContains Operator = "contains"
	OpGt       Operator = "gt"
	OpLt       Operator = "lt"
	OpGte      Operator = "gte"
	OpLte      Operator = "lte"
	OpIn       Operator = "in"
)

// ErrColumnNotFound is returned when a filter or projection names a column
// absent from the sheet's header row, wrapped with the offending name. A
// header name is not a credential, so it is safe to echo back.
var ErrColumnNotFound = errors.New("googlesheets: column not found in sheet header")

// Filter is one AND-ed condition of sheets_query_rows' filter DSL. Column
// names a header from the sheet's header row (validated against it before
// any scan begins); Value is agent-supplied data, compared as a literal
// against each row's cell — never interpolated into any Google API call,
// never treated as a formula regardless of its leading character. That is
// the whole of docs/06-sheets-adapter.md §6's formula-injection guardrail:
// this package simply never builds a query string for Google to evaluate,
// so a value beginning "=", "+", "-", or "@" has nothing to inject into.
type Filter struct {
	Column string   `json:"column"`
	Op     Operator `json:"op"`
	// Value is the comparison operand. A string, number, or bool for every
	// operator except "in", which requires a non-empty array of scalars.
	Value any `json:"value"`
}

// Validate reports whether f is a well-formed filter: a non-empty column
// name, a known operator, and a value shaped correctly for that operator.
// It does not check the column against any header — that requires the
// sheet's actual header row and is checked separately once queryRows has
// fetched it (a column name is spreadsheet-specific; validating it here
// would need a network call this function deliberately never makes).
func (f Filter) Validate() error {
	if strings.TrimSpace(f.Column) == "" {
		return errors.New("googlesheets: filter.column is required")
	}
	switch f.Op {
	case OpEq, OpNeq, OpContains, OpGt, OpLt, OpGte, OpLte:
		if f.Value == nil {
			return fmt.Errorf("googlesheets: filter on column %q requires a value", f.Column)
		}
		if _, ok := f.Value.([]any); ok {
			return fmt.Errorf("googlesheets: filter on column %q with op %q requires a scalar value, not an array", f.Column, f.Op)
		}
	case OpIn:
		items, ok := f.Value.([]any)
		if !ok || len(items) == 0 {
			return fmt.Errorf("googlesheets: filter on column %q with op \"in\" requires a non-empty array value", f.Column)
		}
	default:
		return fmt.Errorf("googlesheets: unsupported filter operator %q on column %q", f.Op, f.Column)
	}
	return nil
}

// Matches reports whether cell (one raw value as the Sheets API returns
// it — string, float64, bool, or nil) satisfies f. Every comparison is
// literal: a filter value that looks like a formula ("=IMPORTRANGE(...)",
// "+A1", "-1", "@SUM") is compared as ordinary text or, if genuinely
// numeric, as a plain number — there is no code path here that treats a
// leading =/+/-/@ specially, because doing so would be the first step
// toward building the query-language surface docs/06 §6 rules out.
func (f Filter) Matches(cell any) bool {
	switch f.Op {
	case OpEq:
		return valuesEqual(cell, f.Value)
	case OpNeq:
		return !valuesEqual(cell, f.Value)
	case OpContains:
		return strings.Contains(cellString(cell), cellString(f.Value))
	case OpGt:
		return compareOrdered(cell, f.Value) > 0
	case OpLt:
		return compareOrdered(cell, f.Value) < 0
	case OpGte:
		return compareOrdered(cell, f.Value) >= 0
	case OpLte:
		return compareOrdered(cell, f.Value) <= 0
	case OpIn:
		items, _ := f.Value.([]any)
		for _, item := range items {
			if valuesEqual(cell, item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// valuesEqual compares a raw cell value against a filter operand: as
// numbers when both sides parse as one, otherwise as literal text via the
// same cellString stringification describe.go uses for display. Neither
// path ever evaluates value as anything but data.
func valuesEqual(cell, value any) bool {
	if cn, ok := asFloat(cell); ok {
		if vn, ok2 := asFloat(value); ok2 {
			return cn == vn
		}
	}
	return cellString(cell) == cellString(value)
}

// compareOrdered returns -1/0/1 for cell</=/> value, numerically when both
// sides parse as a number, otherwise lexicographically over their literal
// text — still just data, never a Google-evaluated expression.
func compareOrdered(cell, value any) int {
	if cn, ok := asFloat(cell); ok {
		if vn, ok2 := asFloat(value); ok2 {
			switch {
			case cn < vn:
				return -1
			case cn > vn:
				return 1
			default:
				return 0
			}
		}
	}
	return strings.Compare(cellString(cell), cellString(value))
}

// asFloat reports whether v can be read as a number: a JSON number decodes
// to float64 already (both a fetched cell and a tool argument go through
// encoding/json), and a numeric-looking string ("42", "3.5") also counts —
// but "=IMPORTRANGE(...)" and similar formula-shaped strings fail
// strconv.ParseFloat and fall through to a literal string comparison,
// exactly like any other non-numeric text.
func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
