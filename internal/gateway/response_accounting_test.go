package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestPendingGenerationSeparatesKnownNativeEvidenceFromUnfinishedHTTP(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		env := accountingEnvelope(t)
		fact := &env.Attempts[len(env.Attempts)-1]
		fact.UsageObserved, fact.UsageComplete = false, false
		fact.Interaction = &usage.InteractionEvidence{Fidelity: "strict", PlanClass: "native_identity", UpstreamState: usage.UpstreamNotSent, ClientState: usage.ClientUnobserved}
		original := *fact
		x := &call{family: wire.Responses, keyID: env.KeyID, facts: env.Attempts,
			request: request{id: env.RequestID, durableID: env.AccountingID, startedAt: env.StartedAt,
				release: &runtime.Release{Snapshot: &routing.Snapshot{Generation: routing.Generation{ID: env.RuntimeGenerationID}}}}, routeLeg: routeLeg{route: &routing.Route{Slug: env.Route}},
		}
		if pinned {
			x.exchange = &pinnedExchange{state: attemptState{accepted: true, settled: true}}
			x.exchange.state.dispatched.Store(true)
		}
		server := &Server{now: func() time.Time { return env.CompletedAt }, log: quiet}
		metadata := map[string]json.RawMessage{}
		if !server.pendingGenerationUsage(x, fact, metadata) {
			t.Fatal("native accepted generation did not create its retained accounting identity")
		}
		event, err := usage.Decode(metadata["pending_usage"])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := usage.Validate(event); err != nil {
			t.Fatal(err)
		}
		final := event.Attempts[len(event.Attempts)-1]
		if !event.HTTPOutcomePending || event.StatusCode != nil || event.Committed || final.Committed || final.ErrorClass != nil ||
			!final.Usage.Observed || final.Usage.Complete || !final.Usage.BillingUncertain || *final.Usage.InputTokens != fact.Usage.InputTokens ||
			final.Routing.Interaction.ClientState != usage.ClientUnobserved || final.Routing.Interaction.UpstreamState == usage.UpstreamNotSent {
			t.Fatalf("pending template confuses native evidence with finalized HTTP: %+v", event)
		}
		if !reflect.DeepEqual(*fact, original) {
			t.Fatal("pending template mutated the live native fact")
		}
		if event.EventID != x.generation.generationHTTP.event.EventID || event.RequestID != env.AccountingID || final.ID != x.generation.generationHTTP.event.Attempts[len(event.Attempts)-1].ID {
			t.Fatal("pending identity was not retained for original HTTP completion")
		}
		raw, _ := json.Marshal(metadata)
		resource := &resources.Resource{ID: "retained-resource", APIKeyID: event.APIKeyID, ProviderID: *event.ProviderID, Metadata: raw}
		if err := attachGenerationHTTP(x, resource); err != nil {
			t.Fatal(err)
		}
		resource.APIKeyID = event.EventID
		if err := attachGenerationHTTP(x, resource); err == nil {
			t.Fatal("different key owner was attached")
		}
	}
}
