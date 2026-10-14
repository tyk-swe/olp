package usage

import "testing"

// The guard cannot be reached through real pricing — the lines are built by
// the same rules that compute the cost — so it is exercised directly: a lines
// document that does not re-derive the stored amounts must drop to NULL while
// the costs still persist.
func TestCostLinesKept(t *testing.T) {
	mismatch := []byte(`[{"component":"input_per_million","tier":null,"unit":"1000000 Tokens","quantity":"0.001","list_unit_price":"9","contracted_unit_price":"9"}]`)
	list, contracted, charge := "0.003000000000", "0.003000000000", "0.003000000000"
	if got := costLinesKept(mismatch, &list, &contracted); got != nil {
		t.Fatalf("mismatched cost lines kept: %s", got)
	}
	if got := chargeLinesKept(mismatch, &charge); got != nil {
		t.Fatalf("mismatched charge lines kept: %s", got)
	}
	match := []byte(`[{"component":"input_per_million","tier":null,"unit":"1000000 Tokens","quantity":"0.001","list_unit_price":"3","contracted_unit_price":"3"}]`)
	if got := costLinesKept(match, &list, &contracted); got == nil {
		t.Fatal("matching cost lines dropped")
	}
	if got := chargeLinesKept(match, &charge); got == nil {
		t.Fatal("matching charge lines dropped")
	}
	// Rounding half away from zero, matching PostgreSQL's round(numeric): the
	// 0.5-micro unit boundary rounds up on either sign.
	halfUp := []byte(`[{"component":"per_request","tier":null,"unit":"Requests","quantity":"1","list_unit_price":"0.0030000000005","contracted_unit_price":"0.0030000000005"}]`)
	rounded := "0.003000000001"
	if got := costLinesKept(halfUp, &rounded, &rounded); got == nil {
		t.Fatal("half-up boundary mismatch")
	}
	negRounded := "-0.003000000001"
	halfDown := []byte(`[{"component":"per_request","tier":null,"unit":"Requests","quantity":"1","list_unit_price":"-0.0030000000005","contracted_unit_price":"-0.0030000000005"}]`)
	if got := costLinesKept(halfDown, &negRounded, &negRounded); got == nil {
		t.Fatal("negative half boundary mismatch")
	}
	if got := costLinesKept(halfUp, &list, &contracted); got != nil {
		t.Fatal("sub-scale mismatch kept")
	}
	if got := costLinesKept([]byte(`not-json`), &list, &contracted); got != nil {
		t.Fatal("malformed lines kept")
	}
	if got := costLinesKept([]byte(`[{"quantity":"x","list_unit_price":"1","contracted_unit_price":"1"}]`), &list, &contracted); got != nil {
		t.Fatal("malformed number kept")
	}
	if got := costLinesKept(match, nil, &contracted); got != nil {
		t.Fatal("lines kept beside a NULL list cost")
	}
	if got := chargeLinesKept(match, nil); got != nil {
		t.Fatal("charge lines kept beside a NULL charge")
	}
	if got := costLinesKept(nil, &list, &contracted); got != nil {
		t.Fatal("nil lines did not stay nil")
	}
}
