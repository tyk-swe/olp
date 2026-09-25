package protocols_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestEveryCompatibleVendorUsesItsReviewedOperationContract(t *testing.T) {
	for _, vendor := range []string{"deepseek", "fireworks", "deepinfra", "huggingface", "perplexity", "cohere", "voyage"} {
		t.Run(vendor, func(t *testing.T) {
			for _, operation := range []string{"generation", "embeddings", "token_count", "moderation"} {
				for _, surface := range []string{"openai", "anthropic", "gemini"} {
					for _, mode := range []string{"unary", "streaming"} {
						want := surface == "openai" && ((operation == "generation" && vendor != "voyage") || (operation == "embeddings" && mode == "unary" && (vendor == "voyage" || vendor == "cohere")))
						if got := connectors.Supports("openai_compatible", vendor, operation, surface, mode); got != want {
							t.Fatalf("%s/%s/%s: supported=%v want %v", operation, surface, mode, got, want)
						}
					}
				}
			}
			if vendor != "voyage" {
				for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
					for _, stream := range []bool{false, true} {
						fields := map[string]any{"model": "team-model", "stream": stream}
						if family == openai.FamilyChat {
							fields["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
							fields["max_completion_tokens"] = 17
							fields["seed"] = 5
						} else {
							fields["input"] = "hello"
							fields["max_output_tokens"] = 17
						}
						body, _ := json.Marshal(fields)
						request, err := protocols.Parse(family, body, "")
						if err != nil {
							t.Fatal(err)
						}
						encoded, wire, err := protocols.Encode(request, "openai_compatible", vendor, "wire-model", nil)
						if err != nil || wire != openai.FamilyChat {
							t.Fatalf("%s stream=%v: %s %v", family, stream, wire, err)
						}
						document, _ := object(encoded)
						if string(document["max_tokens"]) != "17" || document["max_completion_tokens"] != nil || string(document["model"]) != `"wire-model"` {
							t.Fatalf("wrong profile request: %s", encoded)
						}
					}
				}
			}
			if vendor == "cohere" || vendor == "voyage" {
				request, err := protocols.Parse(openai.FamilyEmbeddings, []byte(`{"model":"team-model","input":["hello","world"],"encoding_format":"base64"}`), "")
				if err != nil {
					t.Fatal(err)
				}
				if _, wire, err := protocols.Encode(request, "openai_compatible", vendor, "wire-model", nil); err != nil || wire != openai.FamilyEmbeddings {
					t.Fatalf("embedding profile: %s %v", wire, err)
				}
			}
		})
	}
}

func TestCohereAndVoyageKeepTheirDifferentEmbeddingRefusals(t *testing.T) {
	for _, field := range []string{`"dimensions":32`, `"input_type":"query"`, `"truncate":true`} {
		request, err := protocols.Parse(openai.FamilyEmbeddings, []byte(`{"model":"team-model","input":"hello",`+field+`}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := protocols.Encode(request, "openai_compatible", "cohere", "wire-model", nil); err == nil {
			t.Fatalf("Cohere accepted %s", field)
		}
	}
	request, _ := protocols.Parse(openai.FamilyEmbeddings, []byte(`{"model":"team-model","input":[1,2,3]}`), "")
	if _, _, err := protocols.Encode(request, "openai_compatible", "voyage", "wire-model", nil); err == nil {
		t.Fatal("Voyage accepted tokenized non-text input")
	}
	request, _ = protocols.Parse(openai.FamilyResponses, []byte(`{"model":"team-model","input":"hello","max_output_tokens":17,"previous_response_id":"owned-resource"}`), "")
	if request == nil {
		t.Fatal("Responses profile dropped the provider-state reference the gateway must resolve")
	}
	request, _ = protocols.Parse(openai.FamilyResponses, []byte(`{"model":"team-model","input":"hello","max_output_tokens":17,"metadata":{"private":"value"}}`), "")
	if _, _, err := protocols.Encode(request, "openai_compatible", "deepseek", "wire-model", nil); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("Responses-only extension was discarded: %v", err)
	}
}
