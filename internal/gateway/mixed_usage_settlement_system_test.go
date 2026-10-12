//go:build integration

package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestIntegrationMixedCompleteAndPartialUsageChargesBeyondReservationFloor(t *testing.T) {
	client, namespace := mediaValkey(t)
	limiter, err := limits.New(client, namespace)
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(1000)
	request := limits.Request{CostOwnerID: uuid.NewString(), LookupID: strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
		TokensPerMinute: &tokens, RequestedTokens: 300, LeaseTTL: time.Minute, ReportRate: true}
	lease, err := limiter.Reserve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := lease.RateState().ResetAfter; remaining < 2*time.Second {
		if err = lease.Refund(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(remaining + 10*time.Millisecond)
		lease, err = limiter.Reserve(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
	}
	complete, partial := AttemptFact{}, AttemptFact{}
	complete.observeUsage(&openai.Usage{InputTokens: 200, OutputTokens: 15}, true)
	complete.recordEvidence(false)
	partial.observeUsage(&openai.Usage{InputTokens: 14, OutputTokens: 1}, false)
	partial.recordEvidence(true)
	x := execution{estimate: 150, facts: []AttemptFact{complete, partial}}
	if err = lease.Reconcile(t.Context(), *x.settledTokens()); err != nil {
		t.Fatal(err)
	}
	request.RequestedTokens = 1
	check, err := limiter.Reserve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := check.RateState().TokenRemaining; got != 634 {
		t.Fatalf("remaining=%d want634 afterone additional token: complete215 plus conservativepartial150 must charge365, beyond the300 reservation floor", got)
	}
}
