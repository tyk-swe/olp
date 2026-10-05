package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
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
						want := kindServes && contract.Serves(operation)
						if got := connectors.Supports("openai_compatible", contract.ID, operation, surface, mode); got != want {
							t.Fatalf("%s/%s/%s: supported=%v want %v", operation, surface, mode, got, want)
						}
					}
				}
			}
			if !contract.Serves("generation") {
				return
			}
			wantWire := openai.FamilyChat
			limit := "max_completion_tokens"
			for _, rewrite := range contract.Request("generation").Rewrites {
				if rewrite.From == "max_completion_tokens" {
					limit = rewrite.To
				}
			}
			for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
				fields := map[string]any{"model": "team-model"}
				if family == openai.FamilyChat {
					fields["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					fields["max_completion_tokens"] = 17
				} else {
					fields["input"] = "hello"
					fields["max_output_tokens"] = 17
					if contract.Speaks("openai-responses") {
						wantWire = openai.FamilyResponses
					}
				}
				body, _ := json.Marshal(fields)
				request, err := Parse(family, body, "")
				if err != nil {
					t.Fatal(err)
				}
				encoded, wire, err := Encode(request, "openai_compatible", contract.ID, "wire-model", nil)
				if err != nil || wire != wantWire {
					t.Fatalf("%s: %s %v", family, wire, err)
				}
				document, _ := object(encoded)
				if wire == openai.FamilyChat && string(document[limit]) != "17" || string(document["model"]) != `"wire-model"` {
					t.Fatalf("wrong %s request: %s", family, encoded)
				}
			}
		})
	}
}

func TestCohereAndVoyageKeepTheirDifferentEmbeddingRefusals(t *testing.T) {
	for _, field := range []string{`"dimensions":32`, `"input_type":"query"`, `"truncate":true`} {
		request, err := Parse(openai.FamilyEmbeddings, []byte(`{"model":"team-model","input":"hello",`+field+`}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Encode(request, "openai_compatible", "cohere", "wire-model", nil); err == nil {
			t.Fatalf("Cohere accepted %s", field)
		}
	}
	request, _ := Parse(openai.FamilyEmbeddings, []byte(`{"model":"team-model","input":[1,2,3]}`), "")
	if _, _, err := Encode(request, "openai_compatible", "voyage", "wire-model", nil); err == nil {
		t.Fatal("Voyage accepted tokenized non-text input")
	}
	request, _ = Parse(openai.FamilyResponses, []byte(`{"model":"team-model","input":"hello","max_output_tokens":17,"previous_response_id":"owned-resource"}`), "")
	if request == nil {
		t.Fatal("Responses profile dropped the provider-state reference the gateway must resolve")
	}
	request, _ = Parse(openai.FamilyResponses, []byte(`{"model":"team-model","input":"hello","max_output_tokens":17,"metadata":{"private":"value"}}`), "")
	if _, _, err := Encode(request, "openai_compatible", "deepseek", "wire-model", nil); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("Responses-only extension was discarded: %v", err)
	}
}
