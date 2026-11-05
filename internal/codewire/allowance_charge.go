package codewire

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func AllowanceCharge(usage oif.Value) json.RawMessage {
	vendor := field(usage, "opencode")
	if vendor.Kind() != oif.Object {
		return nil
	}
	raw := vendor.Raw()
	cost, reported := usage.Lookup("cost")
	_, hasCost := vendor.Lookup("cost")
	combined := raw
	if reported && cost.Kind() == oif.Number && !hasCost {
		separator := ","
		if len(vendor.Members()) == 0 {
			separator = ""
		}
		combined = raw[:len(raw)-1] + separator + `"cost":` + cost.Raw() + "}"
	}
	charge := json.RawMessage(combined)
	if codemode.ValidateAllowanceCharge(charge) != nil {
		return nil
	}
	return charge
}
