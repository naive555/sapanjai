package googlesheets

import "testing"

func TestFilter_Validate(t *testing.T) {
	cases := []struct {
		name    string
		filter  Filter
		wantErr bool
	}{
		{"eq with scalar value ok", Filter{Column: "status", Op: OpEq, Value: "draft"}, false},
		{"missing column", Filter{Column: "", Op: OpEq, Value: "draft"}, true},
		{"eq with nil value", Filter{Column: "status", Op: OpEq, Value: nil}, true},
		{"eq with array value rejected", Filter{Column: "status", Op: OpEq, Value: []any{"a", "b"}}, true},
		{"in with array value ok", Filter{Column: "status", Op: OpIn, Value: []any{"draft", "pending"}}, false},
		{"in with empty array rejected", Filter{Column: "status", Op: OpIn, Value: []any{}}, true},
		{"in with scalar value rejected", Filter{Column: "status", Op: OpIn, Value: "draft"}, true},
		{"unknown operator rejected", Filter{Column: "status", Op: "regex", Value: "x"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.filter.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

// TestFilter_FormulaValueIsALiteral is the plan's required test: a filter
// value that looks like a Google formula/query-language expression must
// round-trip as an ordinary literal string and reach no evaluator — it
// either matches a cell whose text is byte-identical, or it doesn't, exactly
// like any other string. There is no code path in Filter.Matches that
// inspects a value's leading character at all.
func TestFilter_FormulaValueIsALiteral(t *testing.T) {
	const formula = "=IMPORTRANGE(\"evil\", \"Sheet1!A1\")"

	f := Filter{Column: "notes", Op: OpEq, Value: formula}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a formula-shaped value to validate like any other string", err)
	}

	// The literal, byte-identical cell matches.
	if !f.Matches(formula) {
		t.Error("Matches(formula) = false, want true: an eq filter against the identical cell text must match")
	}
	// A cell that merely starts the same way does not match — proves this
	// is a full-string literal comparison, not a "starts with =" special
	// case that would hint at formula interpretation.
	if f.Matches("=IMPORTRANGE(\"evil\", \"Sheet1!A2\")") {
		t.Error("Matches(different formula text) = true, want false: comparison must be exact-string, not prefix/formula-aware")
	}

	// contains: the literal substring must be found in cell text exactly as
	// given, again with no special handling of the leading "=".
	containsFilter := Filter{Column: "notes", Op: OpContains, Value: "=IMPORTRANGE"}
	if !containsFilter.Matches("see " + formula + " for details") {
		t.Error("contains filter did not find the literal formula-shaped substring")
	}

	// "in" with a formula-shaped member.
	inFilter := Filter{Column: "notes", Op: OpIn, Value: []any{"draft", formula}}
	if !inFilter.Matches(formula) {
		t.Error("in filter did not match a formula-shaped member of its value list")
	}

	// neq is the logical negation — must also treat the formula as literal
	// text, not as "any formula" or "any expression."
	neqFilter := Filter{Column: "notes", Op: OpNeq, Value: formula}
	if neqFilter.Matches(formula) {
		t.Error("neq filter matched the identical formula text, want it excluded")
	}
	if !neqFilter.Matches("draft") {
		t.Error("neq filter should match any cell whose text differs from the formula literal")
	}
}

func TestFilter_Matches_Eq(t *testing.T) {
	f := Filter{Column: "status", Op: OpEq, Value: "draft"}
	if !f.Matches("draft") {
		t.Error("want match on identical string")
	}
	if f.Matches("pending") {
		t.Error("want no match on different string")
	}
}

func TestFilter_Matches_Eq_Numeric(t *testing.T) {
	// A JSON number decodes to float64 for both the cell (from the Sheets
	// API) and the filter value (from tool arguments), so 5 == 5 must
	// match numerically even though a naive string compare of "5" vs "5"
	// would also happen to pass — use 5.0 vs the int-shaped 5 to actually
	// exercise the numeric path.
	f := Filter{Column: "count", Op: OpEq, Value: float64(5)}
	if !f.Matches(float64(5)) {
		t.Error("want numeric match")
	}
	if f.Matches(float64(6)) {
		t.Error("want no match for a different number")
	}
}

func TestFilter_Matches_Neq(t *testing.T) {
	f := Filter{Column: "status", Op: OpNeq, Value: "draft"}
	if f.Matches("draft") {
		t.Error("want no match on identical string")
	}
	if !f.Matches("pending") {
		t.Error("want match on different string")
	}
}

func TestFilter_Matches_Contains(t *testing.T) {
	f := Filter{Column: "partner_name", Op: OpContains, Value: "ก่อสร้าง"}
	if !f.Matches("หจก. ก่อสร้าง จำกัด") {
		t.Error("want substring match")
	}
	if f.Matches("บริษัท อื่น") {
		t.Error("want no match when substring absent")
	}
}

func TestFilter_Matches_Ordered_Numeric(t *testing.T) {
	gt := Filter{Column: "amount", Op: OpGt, Value: float64(100)}
	if !gt.Matches(float64(150)) || gt.Matches(float64(50)) || gt.Matches(float64(100)) {
		t.Error("gt: unexpected result")
	}
	gte := Filter{Column: "amount", Op: OpGte, Value: float64(100)}
	if !gte.Matches(float64(100)) {
		t.Error("gte: want match at boundary")
	}
	lt := Filter{Column: "amount", Op: OpLt, Value: float64(100)}
	if !lt.Matches(float64(50)) || lt.Matches(float64(100)) {
		t.Error("lt: unexpected result")
	}
	lte := Filter{Column: "amount", Op: OpLte, Value: float64(100)}
	if !lte.Matches(float64(100)) {
		t.Error("lte: want match at boundary")
	}
}

func TestFilter_Matches_Ordered_NonNumericFallsBackToStringCompare(t *testing.T) {
	// Neither side parses as a number: falls back to lexicographic string
	// comparison rather than panicking or always failing.
	f := Filter{Column: "code", Op: OpGt, Value: "B"}
	if !f.Matches("C") {
		t.Error("want lexicographic C > B")
	}
	if f.Matches("A") {
		t.Error("want lexicographic A < B to not match gt")
	}
}

func TestFilter_Matches_In(t *testing.T) {
	f := Filter{Column: "status", Op: OpIn, Value: []any{"draft", "pending"}}
	if !f.Matches("draft") || !f.Matches("pending") {
		t.Error("want match for any listed value")
	}
	if f.Matches("archived") {
		t.Error("want no match for a value outside the list")
	}
}

func TestFilter_Matches_NilCell(t *testing.T) {
	// A ragged row (shorter than the header) yields a nil cell for a
	// missing trailing column — must not panic, and must compare as an
	// empty string via cellString.
	f := Filter{Column: "notes", Op: OpEq, Value: ""}
	if !f.Matches(nil) {
		t.Error("want nil cell to match an eq filter for empty string")
	}
}
