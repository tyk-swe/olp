package usage

import (
	"errors"
	"math"
	"regexp"
)

var cacheAmount = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,12})?$`)

// CacheEvidence contains no content; budget debits remain separate from billing.
type CacheEvidence struct {
	Result       string   `json:"result"`
	Currency     string   `json:"currency,omitempty"`
	AgeSeconds   int64    `json:"age_seconds"`
	Similarity   *float64 `json:"similarity,omitempty"`
	AvoidedCost  string   `json:"avoided_cost,omitempty"`
	BudgetCost   string   `json:"budget_cost,omitempty"`
	BudgetCharge string   `json:"budget_charge,omitempty"`
}

func (e *CacheEvidence) Validate() error {
	if e == nil {
		return nil
	}
	if e.Result != "hit" && e.Result != "semantic-hit" || e.AgeSeconds < 0 || e.AgeSeconds > 604800 {
		return errors.New("invalid cache evidence")
	}
	if e.Similarity != nil && (*e.Similarity < 0 || *e.Similarity > 1 || math.IsNaN(*e.Similarity)) {
		return errors.New("invalid cache similarity")
	}
	for _, v := range []string{e.AvoidedCost, e.BudgetCost, e.BudgetCharge} {
		if v == "" {
			continue
		}
		if !cacheAmount.MatchString(v) {
			return errors.New("invalid cache amount")
		}
	}
	return nil
}
