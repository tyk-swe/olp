package providerinvoke

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestConfiguredPreparationKeepsCallerPresenceAndActualDefaultOrigins(t *testing.T) {
	request, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"hi"}],"temperature":null,"top_p":0,"parallel_tool_calls":false,"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := connectors.Config{ProfileID: "openai-chat", ProfileRevision: "1", Kind: "openai", AuthMode: "api_key", OperationDefaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"temperature": json.RawMessage(`0.7`), "top_p": json.RawMessage(`0.8`), "parallel_tool_calls": json.RawMessage(`true`), "tools": json.RawMessage(`[{"type":"function","function":{"name":"default_tool"}}]`), "max_tokens": json.RawMessage(`100`)}}}, Bindings: map[string]connectors.Binding{"logical": {Model: "native-model", Defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`200`)}}}}}}
	result, err := Prepare(request, cfg, "logical", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(result.Prepared.Document().Bytes(), &body) != nil {
		t.Fatal("invalid prepared body")
	}
	for key, want := range map[string]string{"model": `"native-model"`, "temperature": "null", "top_p": "0", "parallel_tool_calls": "false", "tools": "[]", "max_tokens": "200"} {
		if string(body[key]) != want {
			t.Fatalf("%s=%s want=%s", key, body[key], want)
		}
	}
	if len(result.Defaults) != 1 || result.Defaults[0].Pointer != "/max_tokens" || result.Defaults[0].Source != "binding_default" {
		t.Fatalf("false default provenance: %+v", result.Defaults)
	}
	if result.Prepared.Descriptor().Profile.ID != "openai-chat" || string(request.Field("model")) != `"route"` {
		t.Fatal("profile/source identity was lost")
	}
}

func TestEmbeddingDefaultsReachEachActualNativeRequest(t *testing.T) {
	cfg := connectors.Config{Kind: "gemini", AuthMode: "api_key", ProfileID: "gemini-generation", ProfileRevision: "1", OperationDefaults: map[string]connectors.DefaultSet{"embeddings": {Dialect: "gemini-embeddings", Values: map[string]json.RawMessage{"taskType": json.RawMessage(`"RETRIEVAL_DOCUMENT"`), "outputDimensionality": json.RawMessage(`8`)}}}}
	for _, explicit := range []string{"", `,"dimensions":3`} {
		request, err := openai.Parse(openai.FamilyEmbeddings, []byte(`{"model":"route","input":["first","second"]`+explicit+`}`))
		if err != nil {
			t.Fatal(err)
		}
		result, err := Prepare(request, cfg, "embedding-model", nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.Wire != openai.FamilyGeminiEmbeddingsBatch || result.Prepared.Descriptor().Dialect.ID != "gemini-embeddings-batch" {
			t.Fatal("actual batch dialect was lost")
		}
		var body struct {
			Requests []struct {
				Task       string `json:"taskType"`
				Dimensions int    `json:"outputDimensionality"`
				Content    struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		if json.Unmarshal(result.Prepared.Document().Bytes(), &body) != nil || len(body.Requests) != 2 {
			t.Fatal("batch shape changed")
		}
		want := 8
		if explicit != "" {
			want = 3
		}
		for i, entry := range body.Requests {
			if entry.Task != "RETRIEVAL_DOCUMENT" || entry.Dimensions != want || len(entry.Content.Parts) != 1 || entry.Content.Parts[0].Text != []string{"first", "second"}[i] {
				t.Fatalf("native input/default changed: %+v", entry)
			}
		}
	}
	request, err := openai.Parse(openai.FamilyEmbeddings, []byte(`{"model":"route","input":"first","dimensions":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(request, cfg, "embedding-model", nil); err == nil {
		t.Fatal("translated explicit null was overwritten by a default")
	}
}
