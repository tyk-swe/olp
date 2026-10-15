package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/vendors"
)

// TestEveryCompatibleVendorUsesItsReviewedOperationContract checks that each
// compatible vendor is admitted exactly the operations its contract lists,
// that a vendor without Responses is sent Chat Completions, and that a
// renamed token limit reaches the vendor under its documented name.
func TestEveryCompatibleVendorUsesItsReviewedOperationContract(t *testing.T) {
	for _, contract := range vendors.All() {
		if contract.Connector != "openai_compatible" || contract.KindDefault {
			continue
		}
		t.Run(contract.ID, func(t *testing.T) {
			for _, operation := range []string{"generation", "embeddings", "token_count", "moderation"} {
				for _, surface := range []string{"openai", "anthropic", "gemini"} {
					for _, mode := range []string{"unary", "streaming"} {
						kindServes := surface == "openai" && (operation == "generation" || mode == "unary")
						// Generation is translated only to a vendor that speaks
						// an OpenAI dialect; another serves its own natively.
						translatable := operation != "generation" || contract.Speaks("openai-chat") || contract.Speaks("openai-responses")
						want := kindServes && contract.Serves(operation) && translatable
						if got := connectors.Supports("openai_compatible", contract.ID, operation, surface, mode); got != want {
							t.Fatalf("%s/%s/%s: supported=%v want %v", operation, surface, mode, got, want)
						}
					}
				}
			}
			if !contract.Serves("generation") || !contract.Speaks("openai-chat") && !contract.Speaks("openai-responses") {
				return
			}
			wantWire := wire.Chat
			limit := "max_completion_tokens"
			for _, rewrite := range contract.Request("generation").Rewrites {
				if rewrite.From == "max_completion_tokens" {
					limit = rewrite.To
				}
			}
			for _, family := range []wire.Format{wire.Chat, wire.Responses} {
				fields := map[string]any{"model": "team-model"}
				if family == wire.Chat {
					fields["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					fields["max_completion_tokens"] = 17
				} else {
					fields["input"] = "hello"
					fields["max_output_tokens"] = 17
					if contract.Speaks("openai-responses") {
						wantWire = wire.Responses
					}
				}
				body, _ := json.Marshal(fields)
				request, err := Parse(family, body, "")
				if err != nil {
					t.Fatal(err)
				}
				encoded, encodedWire, err := Encode(request, "openai_compatible", contract.ID, "wire-model", nil)
				if err != nil || encodedWire != wantWire {
					t.Fatalf("%s: %s %v", family, encodedWire, err)
				}
				document, _ := object(encoded)
				if encodedWire == wire.Chat && string(document[limit]) != "17" || string(document["model"]) != `"wire-model"` {
					t.Fatalf("wrong %s request: %s", family, encoded)
				}
			}
		})
	}
}

func TestCohereAndVoyageKeepTheirDifferentEmbeddingRefusals(t *testing.T) {
	for _, field := range []string{`"dimensions":32`, `"input_type":"query"`, `"truncate":true`} {
		request, err := Parse(wire.Embeddings, []byte(`{"model":"team-model","input":"hello",`+field+`}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Encode(request, "openai_compatible", "cohere", "wire-model", nil); err == nil {
			t.Fatalf("Cohere accepted %s", field)
		}
	}
	request, _ := Parse(wire.Embeddings, []byte(`{"model":"team-model","input":[1,2,3]}`), "")
	if _, _, err := Encode(request, "openai_compatible", "voyage", "wire-model", nil); err == nil {
		t.Fatal("Voyage accepted tokenized non-text input")
	}
	request, _ = Parse(wire.Responses, []byte(`{"model":"team-model","input":"hello","max_output_tokens":17,"previous_response_id":"owned-resource"}`), "")
	if request == nil {
		t.Fatal("Responses profile dropped the provider-state reference the gateway must resolve")
	}
	request, _ = Parse(wire.Responses, []byte(`{"model":"team-model","input":"hello","max_output_tokens":17,"metadata":{"private":"value"}}`), "")
	if _, _, err := Encode(request, "openai_compatible", "deepseek", "wire-model", nil); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("Responses-only extension was discarded: %v", err)
	}
}
