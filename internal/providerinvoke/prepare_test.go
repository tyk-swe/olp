package providerinvoke

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
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

// A plugin profile's rewrites change the prepared dialect request, which OIF
// records; its envelope wraps only what OLP sends.
func TestPluginProfileRewritesThePreparedRequestOutsideItsEnvelope(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	plugin, err := connectors.NewPluginProfile(digest, abi.Manifest{Name: "acme", Version: "1.0.0", Profiles: []abi.Profile{{
		ID: "acme-gemini", Label: "Acme Gemini", Dialect: "gemini-generate-content",
		Hosting: abi.Hosting{
			Address:  "https://api.acme.example/v1beta",
			Headers:  map[string]string{"Authorization": "Bearer {credential}"},
			Envelope: &abi.Envelope{Request: "request", Fields: map[string]string{"model": "{model}"}, Response: "response"},
			Rewrites: []abi.Rewrite{
				{Op: abi.RewriteSet, Path: "/generationConfig/candidateCount", Value: json.RawMessage(`1`)},
				{Op: abi.RewriteDelete, Path: "/generationConfig/seed"},
			},
		},
	}}}, "acme-gemini")
	if err != nil {
		t.Fatal(err)
	}
	cfg := connectors.Config{Plugin: plugin, Kind: connectors.KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-gemini", ProfileRevision: digest, Endpoint: plugin.Address(nil)}
	request, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"hi"}],"seed":7,"n":2,"max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Prepare(request, cfg, "acme-large", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Contents         []json.RawMessage          `json:"contents"`
		GenerationConfig map[string]json.RawMessage `json:"generationConfig"`
		Request          json.RawMessage            `json:"request"`
	}
	prepared := result.Prepared.Document().Bytes()
	if json.Unmarshal(prepared, &body) != nil || result.Wire != openai.FamilyGemini || len(body.Contents) != 1 || body.Request != nil {
		t.Fatalf("prepared %s for %s", prepared, result.Wire)
	}
	if string(body.GenerationConfig["candidateCount"]) != "1" || string(body.GenerationConfig["maxOutputTokens"]) != "16" || body.GenerationConfig["seed"] != nil {
		t.Fatalf("rewrote the generation config to %s", prepared)
	}
	rewritten := map[string]bool{}
	for _, entry := range result.Prepared.Provenance() {
		rewritten[entry.Pointer] = rewritten[entry.Pointer] || entry.Origin == "hosting_rewrite"
	}
	if !rewritten["/generationConfig/candidateCount"] || !rewritten["/generationConfig/seed"] {
		t.Fatalf("provenance %+v", result.Prepared.Provenance())
	}
	if sent := cfg.WrapRequest(prepared, "acme-large"); string(sent) != `{"model":"acme-large","request":`+string(prepared)+`}` {
		t.Fatalf("sent %s", sent)
	}
}
