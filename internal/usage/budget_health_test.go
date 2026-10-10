package usage

import (
	"errors"
	"testing"
)

func TestBudgetAccountingRefusesUncheckpointedLocalLoss(t *testing.T) {
	emitter := NewEmitter(1)
	emitter.Drop()
	if err := CheckBudgetAccounting(t.Context(), nil, emitter); !errors.Is(err, ErrIncompleteBudgetAccounting) {
		t.Fatalf("lost event admitted: %v", err)
	}
}
