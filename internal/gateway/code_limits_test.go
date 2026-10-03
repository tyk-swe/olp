package gateway

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

func TestCodeRateExcludesMonetaryBudgetsButPreservesRequestLimits(t *testing.T) {
	budget := "1.00"
	group := "group"
	var admission *Admission
	authority := access.Authority{BudgetGroupID: &group, BudgetGroupDailyCostLimit: &budget}
	authority.Policy.DailyCostLimit = &budget
	if lease, err := admission.ReserveCodeRate(t.Context(), authority, 0, time.Minute); err != nil || lease != nil {
		t.Fatalf("monetary limit affected subscription: %v", err)
	}
	if authority.Policy.DailyCostLimit == nil || authority.BudgetGroupID == nil {
		t.Fatal("caller authority mutated")
	}
	count := int64(1)
	authority.Policy.MaxConcurrency = &count
	if _, err := admission.ReserveCodeRate(t.Context(), authority, 0, time.Minute); err == nil || err.Code != "distributed_limits_unavailable" {
		t.Fatalf("request limits bypassed: %v", err)
	}
}
