package routing

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// A strict batch route on a non-OpenAI surface activates without a durable
// template; its handler refuses strict batch at request time. An
// OpenAI-surface batch target still has to qualify for the durable contract.
func TestCompileDurableSkipsNonOpenAIBatchSurfaces(t *testing.T) {
	route := Route{Slug: "batches", Operations: []string{"batch"}, Fidelity: RouteFidelity{Mode: FidelityStrict},
		Targets: []Target{{ID: "target-1", ProviderID: "provider-1", ProviderModel: "wire-model"}}}
	s := &Snapshot{Providers: map[string]Provider{"provider-1": {
		ID: "provider-1", Kind: "anthropic", VendorID: "anthropic", Enabled: true, RevisionID: "revision-1",
		ProfileID: "anthropic-messages", ProfileRevision: "1",
		Capabilities: []Capability{{Model: "wire-model", Operation: "batch", Surface: "anthropic", Mode: "unary"}},
	}}}
	templates, err := s.compileDurable(route)
	if err != nil || len(templates) != 0 {
		t.Fatalf("anthropic batch surface: templates=%v err=%v", templates, err)
	}
	provider := s.Providers["provider-1"]
	provider.Kind, provider.VendorID, provider.ProfileID = "openai_compatible", "together", "together-files-batches"
	provider.Capabilities = []Capability{{Model: "wire-model", Operation: "batch", Surface: "openai", Mode: "unary"}}
	s.Providers["provider-1"] = provider
	if _, err := s.compileDurable(route); err == nil {
		t.Fatal("unqualified OpenAI batch hosting compiled a durable template")
	}
}

func TestCompletionEffectiveOutputLimitIgnoresChatAlias(t *testing.T) {
	for _, fields := range []string{`,"max_tokens":1000,"max_completion_tokens":1`, `,"max_completion_tokens":1`} {
		request, err := openai.Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello"`+fields+`}`))
		if err != nil {
			t.Fatal(err)
		}
		bound := EffectiveOutputLimit(request)
		if request.Field("max_tokens") == nil {
			if bound != nil {
				t.Fatalf("chat-only alias supplied a completion bound: %d", *bound)
			}
		} else if bound == nil || *bound != 1000 {
			t.Fatalf("completion bound: %v", bound)
		}
	}
}
