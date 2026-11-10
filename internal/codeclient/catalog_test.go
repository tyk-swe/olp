package codeclient

import (
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
