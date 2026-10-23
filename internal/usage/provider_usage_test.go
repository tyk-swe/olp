package usage

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestObservedAttemptUsageTokenlessRetained(t *testing.T) {
	evidence := ObservedAttemptUsage(&wire.Usage{}, true, "vector_store")
	if evidence.InputTokens != nil || evidence.OutputTokens != nil || !evidence.Complete || !evidence.Observed {
		t.Fatalf("retained usage not tokenless: %+v", evidence)
	}
	price := &RoutingPrice{Price: Price{ProviderKind: "openai", Model: "m", Operation: "vector_store", PerRequest: new("0.01")}}
	cost, ok := price.Cost(evidence, time.Now(), true, nil)
	if !ok || cost.String() != "0.01" {
		t.Fatalf("fixed fee not priced: %s ok=%v", cost.String(), ok)
	}
	generation := ObservedAttemptUsage(&wire.Usage{}, true, "generation")
	if generation.InputTokens == nil || generation.OutputTokens == nil {
		t.Fatalf("generation usage lost token dimensions: %+v", generation)
	}
}
