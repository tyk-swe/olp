package codeclient

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func TestCatalogReachesPaidModelPoolFallback(t *testing.T) {
	t.Parallel()
	route := codemode.Route{
		Enabled: true, RevisionID: "revision", Slug: "route", Models: []string{"native-a", "native-b"},
		ModelRewriting: true, Aliases: map[string]string{"pool": "native-a"},
		ModelPools:        map[string][]string{"pool": {"native-a", "native-b"}},
		PaidParticipation: true, InitialRecoveryAttempts: 1,
		PaidFallbackTargets: map[string]codemode.PaidTarget{"native-b": {}},
	}
	offers := Offers{{PaidProtocol: codemode.ProtocolResponses, PaidStreaming: true, Models: []string{"native-b"}}}
	fallback, err := route.PaidFallbackModel(codemode.Operation{Model: "pool"}, nil)
	if err != nil || fallback == nil || fallback.Model != "native-b" {
		t.Fatalf("paid fallback = %+v, error = %v", fallback, err)
	}
	pool := func(route codemode.Route, offers Offers) *codemode.Model {
		models := Catalog(route, offers)
		index := slices.IndexFunc(models, func(model codemode.Model) bool { return model.ID == "pool" })
		if index < 0 {
			return nil
		}
		return &models[index]
	}
	model := pool(route, offers)
	if model == nil || !model.Paid || !slices.Contains(model.Clients, codeadapter.ClientCodex) || len(model.Capabilities) != 1 || model.Capabilities[0].Protocol != codemode.ProtocolResponses {
		t.Fatalf("pool catalog entry = %+v", model)
	}
	configuration, err := Generate(route, offers, Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: "pool"})
	if err != nil || configuration.Model != "pool" || !slices.Contains(configuration.PaidModels, "pool") || !strings.Contains(configuration.Configuration, "supports_websockets = false") {
		t.Fatalf("client configuration = %+v, error = %v", configuration, err)
	}
	missing := pool(route, nil)
	if missing != nil {
		t.Fatalf("unavailable paid target advertised: %+v", missing)
	}
	withoutRecovery := route
	withoutRecovery.InitialRecoveryAttempts = 0
	if model := pool(withoutRecovery, offers); model != nil {
		t.Fatalf("disabled fallback advertised: %+v", model)
	}
	withoutPool := route
	withoutPool.ModelPools = nil
	if model := pool(withoutPool, offers); model != nil {
		t.Fatalf("unreachable alternate advertised: %+v", model)
	}
	earlierFallback := route
	earlierFallback.PaidFallbackTargets = map[string]codemode.PaidTarget{"native-a": {}, "native-b": {}}
	if model := pool(earlierFallback, offers); model != nil {
		t.Fatalf("unselected alternate advertised: %+v", model)
	}
	messageOffers := Offers{{PaidProtocol: codemode.ProtocolMessages, PaidStreaming: true, Models: []string{"native-b"}}}
	messageModel := pool(route, messageOffers)
	if messageModel == nil || slices.Contains(messageModel.Clients, codeadapter.ClientCodex) || !slices.Contains(messageModel.Clients, codeadapter.ClientClaudeCode) {
		t.Fatalf("messages paid fallback = %+v", messageModel)
	}
	if _, err := Generate(route, messageOffers, Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: "pool"}); err == nil {
		t.Fatal("unsupported client accepted for paid fallback")
	}
	withoutStreaming := Offers{{PaidProtocol: codemode.ProtocolResponses, Models: []string{"native-b"}}}
	if model := pool(route, withoutStreaming); model == nil || len(model.Clients) != 0 {
		t.Fatalf("non-streaming paid fallback clients = %+v", model)
	}
	withSubscription := slices.Concat(offers, Offers{{Adapter: codemode.AdapterCodex, Models: []string{"native-a"}}})
	configuration, err = Generate(route, withSubscription, Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: "pool"})
	if err != nil || !strings.Contains(configuration.Configuration, "supports_websockets = true") {
		t.Fatalf("subscription client configuration = %+v, error = %v", configuration, err)
	}
}

func TestOpenCodePaidConfigurationsReachAdvertisedOperations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		protocol codemode.Protocol
		sdk      string
	}{
		{codemode.ProtocolMessages, "@ai-sdk/anthropic"},
		{codemode.ProtocolResponses, "@ai-sdk/openai"},
		{codemode.ProtocolChat, "@ai-sdk/openai-compatible"},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			protocol := test.protocol
			route := codemode.Route{Enabled: true, RevisionID: "revision", Slug: "paid", Models: []string{"paid-model"}, PaidParticipation: true, PaidTargets: map[string]codemode.PaidTarget{"paid-model": {}}}
			offers := Offers{{PaidProtocol: protocol, PaidStreaming: true, Models: []string{"paid-model"}}}
			configuration, err := Generate(route, offers, Request{GatewayURL: "https://gateway.example.test/prefix", Client: codeadapter.ClientOpenCode, Model: "paid-model"})
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Model    string `json:"model"`
				Provider map[string]struct {
					Models map[string]struct {
						Provider struct {
							NPM string `json:"npm"`
						} `json:"provider"`
					} `json:"models"`
					Options struct {
						BaseURL string `json:"baseURL"`
					} `json:"options"`
				} `json:"provider"`
			}
			if err := json.Unmarshal([]byte(configuration.Configuration), &document); err != nil {
				t.Fatal(err)
			}
			provider, model, found := strings.Cut(document.Model, "/")
			if !found {
				t.Fatalf("unqualified model %q", document.Model)
			}
			if sdk := document.Provider[provider].Models[model].Provider.NPM; sdk != test.sdk {
				t.Fatalf("model SDK %q, want %q", sdk, test.sdk)
			}
			base, err := url.Parse(document.Provider[provider].Options.BaseURL)
			if err != nil {
				t.Fatal(err)
			}
			capability, _ := paidCapability(protocol, true)
			// SDK endpoint composition was captured with @ai-sdk/anthropic 4.0.102 on 2026-11-12.
			// https://www.npmjs.com/package/@ai-sdk/anthropic/v/4.0.102
			operation := strings.TrimPrefix(capability.Path, "v1/")
			requestPath := strings.TrimRight(base.Path, "/") + "/" + operation
			expectedPath := "/prefix" + route.BasePath() + "/" + capability.Path
			if requestPath != expectedPath {
				t.Fatalf("generated SDK request path %q, advertised gateway path %q", requestPath, expectedPath)
			}
			endpoint, ok := codeadapter.ForPath(strings.TrimPrefix(requestPath, "/prefix"+route.BasePath()+"/"))
			if !ok || endpoint.Protocol != protocol {
				t.Fatalf("generated request is not recognized for protocol %q", protocol)
			}
		})
	}
}
