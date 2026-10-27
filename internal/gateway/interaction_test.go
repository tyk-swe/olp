package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
)

func strictHarness(t *testing.T, configure func(*routing.Snapshot)) *harness {
	t.Helper()
	return strictHarnessCfg(t, Config{}, configure)
}

func strictHarnessCfg(t *testing.T, cfg Config, configure func(*routing.Snapshot)) *harness {
	t.Helper()
	h := newHarness(t, cfg)
	release := h.rt.release
	snapshot := release.Snapshot
	credentials := map[string][]byte{}
	for id, provider := range snapshot.Providers {
		provider.ProfileID, provider.ProfileRevision = "compatible-chat", connectors.ProfileRevision
		snapshot.Providers[id] = provider
		for _, slot := range provider.Slots {
			credentials[*slot.CredentialID], _ = release.Credential(*slot.CredentialID)
		}
	}
	route := snapshot.Routes[routeSlug]
	route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
	snapshot.Routes[routeSlug] = route
	if configure != nil {
		configure(snapshot)
	}
	var err error
	h.rt.release, err = runtime.NewRelease(release.ID, release.Sequence, snapshot, credentials)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestStrictFailoverCannotChangeDefaultsThroughModelAlias(t *testing.T) {
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			provider.Bindings = map[string]connectors.Binding{
				modelA:          {Model: "same-upstream", Defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage("1024")}}}},
				"smaller-alias": {Model: "same-upstream", Defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage("32")}}}},
			}
			provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: "smaller-alias", Operation: "generation", Surface: "openai", Mode: "unary"})
			snapshot.Providers[id] = provider
			route := snapshot.Routes[routeSlug]
			first := route.Targets[0]
			second := first
			second.ID, second.RoutingID, second.ProviderModel, second.Priority = uuid.NewString(), uuid.NewString(), "smaller-alias", 1
			route.Targets = []routing.Target{first, second}
			snapshot.Routes[routeSlug] = route
		}
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if h.mock.count("a") == 1 {
			status(400, `{"error":{"code":"context_length_exceeded","message":"capacity refusal"}}`)(w, r)
			return
		}
		completion("same-upstream", answerText)(w, r)
	})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != 400 || errorCode(t, body) != "upstream_rejected" || h.mock.count("a") != 1 {
		t.Fatalf("strict defaults substituted: %d %v calls=%d", resp.StatusCode, body, h.mock.count("a"))
	}
}
