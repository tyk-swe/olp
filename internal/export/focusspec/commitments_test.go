package focusspec

import (
	"strings"
	"testing"
)

func TestUnusedCommitmentRequiresAReconciledRow(t *testing.T) {
	rows := []map[string]string{{"CommitmentDiscountId": "c", "ChargeCategory": "Purchase", "BilledCost": "1"}}
	expected := map[string]CommitmentBalance{"c": {Purchased: "1", Allocated: "0", Unused: "1"}}
	if err := ValidateCommitments(rows, expected); err == nil || !strings.Contains(err.Error(), "D-082-C") {
		t.Fatalf("missing unused charge was accepted: %v", err)
	}
	rows = append(rows, map[string]string{"CommitmentDiscountId": "c", "ChargeCategory": "Usage", "CommitmentDiscountStatus": "Unused", "EffectiveCost": "1"})
	if err := ValidateCommitments(rows, expected); err != nil {
		t.Fatal(err)
	}
	rows[1]["SubAccountId"] = "consumer"
	if err := ValidateCommitments(rows, expected); err == nil {
		t.Fatal("unallocated cost attributed to a consumer")
	}
}
