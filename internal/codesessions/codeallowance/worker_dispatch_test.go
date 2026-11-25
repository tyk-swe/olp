package codeallowance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestFetchForAdapterRejectsEveryUnqualifiedAdapterBeforeAnyRead(t *testing.T) {
	reader := &Reader{}
	for _, adapter := range []codemode.Adapter{codemode.AdapterZAICoding, codemode.AdapterKimiCode, codemode.AdapterQwenTokenPlan, codemode.AdapterClaude, codemode.AdapterOpenCodeGo, codemode.AdapterAntigravity, ""} {
		allowance, parsed, err := reader.fetchForAdapter(context.Background(), adapter, DueAccount{}, time.Now())
		if !errors.Is(err, errAllowanceReadUnqualified) {
			t.Errorf("adapter %q: err = %v, want errAllowanceReadUnqualified", adapter, err)
		}
		if parsed || len(allowance.Windows) != 0 || allowance.Credits != nil {
			t.Errorf("adapter %q: allowance = %+v parsed %v, want no observation", adapter, allowance, parsed)
		}
	}
}
