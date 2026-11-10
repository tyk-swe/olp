package limits

import "testing"

func TestWithinBoundAndAmountEqualShareExactDecimalRules(t *testing.T) {
	if !WithinBound("1.00", "0.40", "0.6") {
		t.Fatal("amount reaching the bound exactly refused")
	}
	if WithinBound("1.00", "0.40", "0.600000000001") {
		t.Fatal("amount one hair past the bound admitted")
	}
	if WithinBound("not-a-number", "0", "0") || WithinBound("1", "", "0") {
		t.Fatal("malformed bound inputs did not fail closed where required")
	}
	if !AmountEqual("0.250", "0.25") || !AmountEqual("0001.250000000000", "1.25") || AmountEqual("0.25", "0.26") || AmountEqual("0.000000000001", "0") || AmountEqual("x", "0.25") {
		t.Fatal("decimal equality does not compare values")
	}
}
