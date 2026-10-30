package codeclient_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
)

func TestGenerateOpenCodeCatalog(t *testing.T) {
	models := []string{"private-code-v42", "qwen-private-v42", "gpt-private-v42"}
	route := codemode.Route{Slug: "coding", RevisionID: "revision", Enabled: true, Models: append(models, "unavailable-model")}
	offers := codeclient.Offers{{Adapter: codemode.AdapterOpenCodeGo, Models: append(models, "unpublished-model")}}
	config, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Models, codeclient.Catalog(route, offers)) || !reflect.DeepEqual(config.NativeModels, models) {
		t.Fatalf("configuration differs from reachable catalog %+v", config)
	}
	var document struct {
		Model    string `json:"model"`
		Provider map[string]struct {
			Whitelist []string `json:"whitelist"`
			Models    map[string]struct {
				ID       string `json:"id"`
				Provider struct {
					NPM string `json:"npm"`
				} `json:"provider"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(config.Configuration), &document); err != nil {
		t.Fatal(err)
	}
	provider := document.Provider["opencode-go"]
	if document.Model != "opencode-go/"+models[0] || !reflect.DeepEqual(provider.Whitelist, models) || len(provider.Models) != len(models) {
		t.Fatalf("models absent from client catalog %+v", document)
	}
	// OpenCode's model.provider.npm contract, checked 2026-10-30: https://github.com/anomalyco/opencode/blob/v1.18.34/packages/opencode/src/provider/provider.ts
	for id, npm := range map[string]string{
		models[0]: "@ai-sdk/openai-compatible",
		models[1]: "@ai-sdk/anthropic",
		models[2]: "@ai-sdk/openai",
	} {
		if model := provider.Models[id]; model.ID != id || model.Provider.NPM != npm {
			t.Fatalf("model %s cannot use its native protocol %+v", id, model)
		}
	}
}

func TestGenerateKeepsDisjointAccountModelsSeparate(t *testing.T) {
	models := []string{"private-code-a", "private-code-b"}
	route := codemode.Route{Slug: "coding", RevisionID: "revision", Enabled: true, Models: models}
	offers := codeclient.Offers{
		{Adapter: codemode.AdapterZAICoding, Models: models[:1]},
		{Adapter: codemode.AdapterZAICoding, Models: models[1:]},
	}
	for _, model := range models {
		config, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode, Model: model})
		if err != nil || config.Model != model || !reflect.DeepEqual(config.NativeModels, models) {
			t.Fatalf("independently reachable model %s, config %+v, error %v", model, config, err)
		}
	}
	if _, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode, Model: models[0], PlanModel: models[1]}); err == nil {
		t.Fatal("configuration combined disjoint accounts of the same subscription")
	}
}
