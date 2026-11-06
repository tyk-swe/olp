package authority

import (
	"bytes"
	"encoding/json"
	"github.com/tyk-swe/olp/internal/attribution"
)

// AttributionBudgets bounds allocated spend for up to 64 project label/value pairs.
// Callers may choose unpinned labels; such caps are allocation controls, not identity.
type AttributionBudgets map[string]map[string]BudgetPolicy

func (b AttributionBudgets) ValidateAndNormalize() error {
	count := 0
	for key, values := range b {
		if !attribution.ValidKey(key) || len(values) == 0 {
			return invalid("budgets", "Use valid label keys with at least one budgeted value.")
		}
		for value, policy := range values {
			count++
			if !attribution.ValidValue(value) || !policy.Limited() {
				return invalid("budgets", "Use machine-token values with at least one positive cost cap.")
			}
			if err := policy.ValidateAndNormalize(); err != nil {
				return err
			}
		}
	}
	if count > 64 {
		return invalid("budgets", "Configure at most 64 label/value budgets per project.")
	}
	return nil
}

func (b AttributionBudgets) JSON() []byte {
	if b == nil {
		return []byte("{}")
	}
	data, _ := json.Marshal(b)
	return data
}

func (b AttributionBudgets) Equal(other AttributionBudgets) bool {
	return bytes.Equal(b.JSON(), other.JSON())
}

// Matches avoids cost estimation for a request outside every allocation.
func (b AttributionBudgets) Matches(labels map[string]string) bool {
	if len(b) == 0 {
		return false
	}
	for key, value := range labels {
		if policy, ok := b[key][value]; ok && policy.Limited() {
			return true
		}
	}
	return false
}
