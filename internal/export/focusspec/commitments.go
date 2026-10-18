package focusspec

import (
	"fmt"
	"github.com/shopspring/decimal"
)

// CommitmentBalance is the period evidence required to check dynamic rule
// D-082-C. Feed all daily rows of a period, after applying its latest revisions.
type CommitmentBalance struct{ Purchased, Allocated, Unused string }

// ValidateCommitments supplements the pinned static model with cross-row
// reconciliation against independently materialized obligation totals.
func ValidateCommitments(rows []map[string]string, expected map[string]CommitmentBalance) error {
	for id, want := range expected {
		purchased, allocated, unused := decimal.Zero, decimal.Zero, decimal.Zero
		for _, row := range rows {
			if row["CommitmentDiscountId"] != id {
				continue
			}
			column := "EffectiveCost"
			if row["ChargeCategory"] == "Purchase" {
				column = "BilledCost"
			}
			value, err := decimal.NewFromString(row[column])
			if err != nil {
				return fmt.Errorf("commitment %s: invalid %s", id, column)
			}
			switch {
			case row["ChargeCategory"] == "Purchase":
				purchased = purchased.Add(value)
			case row["CommitmentDiscountStatus"] == "Used":
				allocated = allocated.Add(value)
			case row["CommitmentDiscountStatus"] == "Unused":
				unused = unused.Add(value)
				for _, key := range []string{"SubAccountId", "SubAccountName", "ConsumedQuantity", "ConsumedUnit", "x_ApiKeyId", "x_Route"} {
					if row[key] != "" {
						return fmt.Errorf("CAU-CostAndUsage-D-082-C: unused commitment has %s", key)
					}
				}
			}
		}
		for _, pair := range []struct {
			got  decimal.Decimal
			want string
		}{{purchased, want.Purchased}, {allocated, want.Allocated}, {unused, want.Unused}} {
			target, err := decimal.NewFromString(pair.want)
			if err != nil {
				return err
			}
			if !pair.got.Equal(target) {
				return fmt.Errorf("CAU-CostAndUsage-D-082-C: %s got %s, expected %s", id, pair.got, target)
			}
		}
		if !purchased.Equal(allocated.Add(unused)) {
			return fmt.Errorf("commitment %s does not reconcile", id)
		}
	}
	return nil
}
