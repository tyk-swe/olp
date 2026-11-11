package usage_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func TestObservedAttemptUsageTokenlessRetained(t *testing.T) {
	evidence := usage.ObservedAttemptUsage(&wire.Usage{}, true, "vector_store")
	if evidence.InputTokens != nil || evidence.OutputTokens != nil || !evidence.Evidence.Complete() || !evidence.Evidence.Observed() {
		t.Fatalf("retained usage not tokenless: %+v", evidence)
	}
	price := rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{ProviderKind: "openai", Model: "m", Operation: "vector_store", PerRequest: new("0.01")}})
	cost, ok := price.Cost(evidence, time.Now(), true, nil)
	if !ok || cost.String() != "0.01" {
		t.Fatalf("fixed fee not priced: %s ok=%v", cost.String(), ok)
	}
	generation := usage.ObservedAttemptUsage(&wire.Usage{}, true, "generation")
	if generation.InputTokens == nil || generation.OutputTokens == nil {
		t.Fatalf("generation usage lost token dimensions: %+v", generation)
	}
}
