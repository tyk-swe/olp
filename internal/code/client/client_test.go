package client_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/client"
)

func TestCatalogReachesPaidModelPoolFallback(t *testing.T) {
	t.Parallel()
	route := code.Route{
		Enabled: true, RevisionID: "revision", Slug: "route", Models: []string{"native-a", "native-b"},
		ModelRewriting: true, Aliases: map[string]string{"pool": "native-a"},
		ModelPools:        map[string][]string{"pool": {"native-a", "native-b"}},
		PaidParticipation: true, InitialRecoveryAttempts: 1,
		PaidFallbackTargets: map[string]code.PaidTarget{"native-b": {}},
	}
	offers := client.Offers{Paid: []client.PaidOffer{{Model: "native-b", Protocol: code.ProtocolResponses, Streaming: true}}}
	fallback, err := route.PaidFallbackModel(code.Operation{Model: "pool"}, nil)
	if err != nil || fallback == nil || fallback.Model != "native-b" {
		t.Fatalf("paid fallback = %+v, error = %v", fallback, err)
	}
	pool := func(route code.Route, offers client.Offers) *code.Model {
		models := client.Catalog(route, offers)
		index := slices.IndexFunc(models, func(model code.Model) bool { return model.ID == "pool" })
		if index < 0 {
			return nil
		}
		return &models[index]
	}
	model := pool(route, offers)
	if model == nil || !model.Paid || !slices.Contains(model.Clients, code.ClientCodex) || len(model.Capabilities) != 1 || model.Capabilities[0].Protocol != code.ProtocolResponses {
		t.Fatalf("pool catalog entry = %+v", model)
	}
	configuration, err := client.Generate(route, offers, client.Request{GatewayURL: "http://localhost:4000", Client: code.ClientCodex, Model: "pool"})
	if err != nil || configuration.Model != "pool" || !slices.Contains(configuration.PaidModels, "pool") || !strings.Contains(configuration.Configuration, "supports_websockets = false") {
		t.Fatalf("client configuration = %+v, error = %v", configuration, err)
	}
	if missing := pool(route, client.Offers{}); missing != nil {
		t.Fatalf("unavailable paid target advertised: %+v", missing)
	}
	withoutRecovery := route
	withoutRecovery.InitialRecoveryAttempts = 0
	if got := pool(withoutRecovery, offers); got != nil {
		t.Fatalf("disabled fallback advertised: %+v", got)
	}
	withSubscription := client.Offers{
		Subscriptions: []client.SubscriptionOffer{{Adapter: code.AdapterCodex, Models: []string{"native-a"}}},
		Paid:          offers.Paid,
	}
	configuration, err = client.Generate(route, withSubscription, client.Request{GatewayURL: "http://localhost:4000", Client: code.ClientCodex, Model: "pool"})
	if err != nil || !strings.Contains(configuration.Configuration, "supports_websockets = true") {
		t.Fatalf("subscription client configuration = %+v, error = %v", configuration, err)
	}
}
