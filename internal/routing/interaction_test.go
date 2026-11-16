package routing

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
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
	provider.Kind, provider.VendorID, provider.ProfileID = "gemini", "google", "gemini-generation"
	provider.Capabilities = []Capability{{Model: "wire-model", Operation: "batch", Surface: "openai", Mode: "unary"}}
	s.Providers["provider-1"] = provider
	if templates, err := s.compileDurable(route); err != nil || len(templates) != 0 {
		t.Fatalf("gemini OpenAI-surface batch: templates=%v err=%v", templates, err)
	}
	provider.Kind, provider.VendorID, provider.ProfileID = "openai", "openai", ""
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

func TestToolContinuationPreservesUpstreamStreamErrors(t *testing.T) {
	template, err := interaction.Compile(interaction.Config{
		Provider: connectors.Config{
			Kind: "anthropic", VendorID: "anthropic", AuthMode: "api_key", ProfileID: "anthropic-messages", ProfileRevision: "1",
			OperationDefaults: map[string]connectors.DefaultSet{
				"generation": {Dialect: "anthropic-messages", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`1024`)}},
			},
		},
		ProviderID: "provider", RevisionID: "revision", Model: "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := openai.Parse(wire.Chat, []byte(`{"model":"route","messages":[{"role":"user","content":"hello"}],"stream":true,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(request, interaction.Context{ContinuationVersion: interaction.ContinuationV1, DurableContinuation: true, AllowProviderState: true})
	if err != nil {
		t.Fatal(err)
	}
	start := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"message","type":"message","role":"assistant","model":"model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	tool := "event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"lookup","input":{}}}` + "\n\n"
	// Anthropic error events, https://platform.claude.com/docs/en/build-with-claude/streaming (2026-11-13).
	failure := "event: error\ndata: " + `{"type":"error","error":{"type":"overloaded_error","message":"synthetic overload"}}` + "\n\n"
	for _, tc := range []struct {
		name, prefix, failure string
		protocol              bool
	}{
		{name: "before message", failure: failure},
		{name: "after message", prefix: start, failure: failure},
		{name: "during tool", prefix: start + tool, failure: failure},
		{name: "malformed error", prefix: start, failure: "event: error\ndata: {\"type\":\"error\"}\n\n", protocol: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection, err := plan.NewToolProjection(1 << 20)
			if err != nil {
				t.Fatal(err)
			}
			var emitted [][]byte
			completion, err := protocols.StreamWithEvents(wire.Anthropic, wire.Anthropic, strings.NewReader(tc.prefix+tc.failure), 1<<20, "route", true, func([]byte) error { return nil }, func(event oif.Event) error {
				frames, err := projection.Observe(event)
				emitted = append(emitted, frames...)
				return err
			})
			var violation *oif.Incompatibility
			if errors.As(err, &violation) {
				t.Fatalf("provider failure replaced by projection refusal: %v", err)
			}
			if tc.protocol {
				var malformed *wire.ProtocolError
				if !errors.As(err, &malformed) {
					t.Fatalf("malformed error = %T %v", err, err)
				}
			} else {
				var upstream *wire.UpstreamError
				if !errors.As(err, &upstream) || upstream.Type != "overloaded_error" || upstream.Message != "synthetic overload" {
					t.Fatalf("upstream error = %T %v", err, err)
				}
			}
			if len(emitted) != 0 {
				t.Fatalf("failed tool stream emitted %d projected frames", len(emitted))
			}
			if state, _, err := projection.Complete(completion, "handle"); err == nil || state != nil {
				t.Fatal("failed stream produced a ready continuation")
			}
		})
	}
}
