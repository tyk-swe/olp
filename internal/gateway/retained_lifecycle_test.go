package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestRetainedAgentInput(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"description":"agent","system_instruction":"Be brief","agent_config":{"antigravity":{}}}`, true},
		{`{"id":"provider-id"}`, false}, {`{"execute":true}`, false},
		{`{"base_environment":"provider-environment"}`, false},
		{`{"base_environment":{"env":{"TOKEN":{"credential":"another-owner"}}}}`, false},
		{`{"base_environment":{"env":{"TOKEN":{"credential":""}}}}`, false},
		{`{"base_environment":{"env":{"TOKEN":{"credential":null,"value":"inline"}}}}`, true},
		{`{"base_environment":{"env":{"credential":{"value":"inline"}}}}`, true},
		{`{"base_environment":{"network":{"allowlist":[{"domain":"*","credential":"another-owner"}]}}}`, false},
		{`{"base_environment":{"network":{"allowlist":[{"domain":"*","credential":""}]}}}`, false},
		{`{"base_environment":{"network":{"allowlist":[{"domain":"example.com","credential":null}]}}}`, true},
		{`{"base_environment":{"network":{"allowlist":"disabled"}}}`, true},
		{`{"base_agent":""}`, true},
		{`{"description":{"text":"wrong"}}`, false},
		{`{"tools":"wrong"}`, false},
		{`{"agent_config":false}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			source, err := oif.ParseJSON([]byte(tc.body), oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if got := retainedAgentInput(source); (got == nil) != tc.valid {
				t.Fatalf("validation=%v, valid=%v", got, tc.valid)
			}
		})
	}
}

func TestRetainedAgentNormalizationAndProjection(t *testing.T) {
	source, err := oif.ParseJSON([]byte(`{"base_agent":"","agent_config":{"max_total_tokens":9007199254740993}}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeRetainedAgent(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := normalized.Root().Lookup("base_agent"); present || !bytes.Contains(normalized.Bytes(), []byte("9007199254740993")) {
		t.Fatalf("normalization: %s", normalized.Bytes())
	}
	if _, present := source.Root().Lookup("base_agent"); !present {
		t.Fatal("normalization modified source")
	}
	s := &Server{}
	resource := &resources.Resource{ID: "agent_resource_local", UpstreamID: "native-agent"}
	for _, tc := range []struct {
		body string
		bad  bool
	}{
		{body: `{"id":"native-agent","base_agent":""}`},
		{body: `{"id":"native-agent","base_environment":{"env":{"TOKEN":{"credential":"foreign"}}}}`, bad: true},
		{body: `{"id":"native-agent","base_environment":{"network":{"allowlist":[{"credential":"foreign"}]}}}`, bad: true},
	} {
		projected, err := s.projectRetainedAgent(t.Context(), &call{}, resource, []byte(tc.body))
		if (err != nil) != tc.bad {
			t.Fatalf("projection of %s: %s, %v", tc.body, projected, err)
		}
		if !tc.bad && (bytes.Contains(projected, []byte("base_agent")) || !bytes.Contains(projected, []byte(resource.ID))) {
			t.Fatalf("projected standalone agent: %s", projected)
		}
	}
}

func TestRetainedControlPricing(t *testing.T) {
	now := time.Now()
	project := "route-project"
	price := usage.RoutingPrice{Price: usage.Price{ProviderKind: "gemini", Model: "agent-model", Operation: "agent_resource"}, EffectiveAt: now.Add(-time.Minute)}
	card := usage.RoutingRateCard{ProjectID: &project, Revisions: []usage.RoutingCardRevision{{ID: "card", EffectiveAt: now.Add(-time.Minute)}}}
	for _, tc := range []struct {
		name   string
		inputs *usage.RoutingInputs
		route  *routing.Route
		denied bool
	}{
		{name: "unpriced", inputs: &usage.RoutingInputs{RefreshedAt: now}},
		{name: "historical pin with effective price", inputs: &usage.RoutingInputs{RefreshedAt: now, Prices: []usage.RoutingPrice{price}}, denied: true},
		{name: "collection route project card", inputs: &usage.RoutingInputs{RefreshedAt: now, RateCards: []usage.RoutingRateCard{card}}, route: &routing.Route{ProjectID: &project}, denied: true},
		{name: "unrelated project card", inputs: &usage.RoutingInputs{RefreshedAt: now, RateCards: []usage.RoutingRateCard{card}}, route: &routing.Route{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Runtime: &fakeRuntime{inputs: tc.inputs}}
			x := &call{operationOverride: "agent_resource", request: request{startedAt: now}}
			p := &pin{provider: routing.Provider{ID: "provider", Kind: "gemini"}, model: "agent-model"}
			err := s.retainedControlPricing(x, p, tc.route)
			if (err != nil) != tc.denied || err != nil && err.Code != "unsupported_pricing" {
				t.Fatalf("pricing error=%v, denied=%v", err, tc.denied)
			}
			if x.route != nil {
				t.Fatal("pricing assigned one route to a collection request")
			}
		})
	}
}

type retainedDigestRuntime struct {
	*fakeRuntime
	key *secrets.AuthKey
}

func (r retainedDigestRuntime) Digest(p secrets.DigestPurpose, value string) []byte {
	return r.key.Digest(p, value)
}

func TestRetainedSubmissionIdentityUsesCallerInput(t *testing.T) {
	rt := retainedDigestRuntime{key: secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), "installation")}
	s := &Server{Runtime: rt}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "agent_resource", Kind: "agent_resource"}}
	before, err := s.retainedSubmissionDigest(e, "route", []byte(`{"description":"agent","seed":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.retainedSubmissionDigest(e, "route", []byte(`{ "seed": 9007199254740993, "description": "agent" }`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("equivalent caller input changed submission identity")
	}
	for _, tc := range []struct{ route, body string }{{"another-route", `{"description":"agent","seed":9007199254740993}`}, {"route", `{"description":"agent","seed":9007199254740992}`}} {
		other, err := s.retainedSubmissionDigest(e, tc.route, []byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(before, other) {
			t.Fatal("changed request reused submission identity")
		}
	}
}

func TestRetainedResponseDelivery(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             int
		body               []byte
		writeErr, flushErr error
	}{
		{name: "create", status: 201, body: []byte(`{"id":"agent_resource_local"}`)},
		{name: "get", status: 200, body: []byte(`{"id":"agent_resource_local"}`)},
		{name: "delete", status: 200, body: []byte(`{}`)},
		{name: "list", status: 200, body: []byte(`{"agents":[]}`)},
		{name: "write failure", status: 200, body: []byte(`{}`), writeErr: errors.New("write failed")},
		{name: "flush failure", status: 200, body: []byte(`{}`), flushErr: errors.New("flush failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &capture{}
			s := &Server{now: time.Now, log: quiet, Sink: sink}
			x := &call{family: wire.GeminiFile, operationOverride: "agent_resource", mode: "unary", actor: "api_key_resource", request: request{id: "request", startedAt: time.Now()}}
			w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: tc.writeErr, flushErr: tc.flushErr}
			w.beforeDelivery = func() {
				remaining := time.Until(w.deadline)
				if remaining <= 0 || remaining > responseWriteTimeout {
					t.Fatalf("delivery has no bounded deadline: %s", remaining)
				}
			}
			s.writeStateJSONStatus(w, x, tc.body, tc.status)
			// The handler's deferred fallback must not overwrite the delivery result.
			s.finish(x, nil, tc.status)
			env := sink.last(t)
			if len(sink.envs) != 1 {
				t.Fatal("delivery was finalized twice")
			}
			if tc.writeErr != nil || tc.flushErr != nil {
				if env.Outcome != "cancelled" || env.ErrorClass != "client_delivery_failed" || !env.Committed {
					t.Fatalf("failed delivery: %+v", env)
				}
			} else if env.Outcome != "success" || env.Status != tc.status || !env.Committed || w.Body.String() != string(tc.body) {
				t.Fatalf("successful delivery: %+v", env)
			}
		})
	}
}

func TestRetainedAffinity(t *testing.T) {
	cred := "v1"
	p := &pin{provider: routing.Provider{ID: "provider", RevisionID: "provider-revision"}, target: routing.Target{ID: "target"}, slot: routing.Slot{ID: "slot", CredentialID: &cred}}
	route := &routing.Route{Slug: "route", RevisionID: "route-revision"}
	r := &resources.Resource{RouteSlug: route.Slug, RouteRevisionID: route.RevisionID, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: &cred}
	if !retainedAffinity(r, p, route) {
		t.Fatal("matching affinity rejected")
	}
	for _, mutate := range []func(*resources.Resource){func(r *resources.Resource) { r.RouteRevisionID = "changed" }, func(r *resources.Resource) { r.TargetID = "changed" }, func(r *resources.Resource) { r.CredentialID = nil }, func(r *resources.Resource) { r.SlotID = "changed" }} {
		copy := *r
		mutate(&copy)
		if retainedAffinity(&copy, p, route) {
			t.Fatal("incompatible affinity accepted")
		}
	}
}

func TestRetainedSelectionUsesOwningTargetAndSlot(t *testing.T) {
	h := newHarness(t, Config{})
	route := h.rt.release.Snapshot.Routes[routeSlug]
	target := route.Targets[1]
	provider := h.rt.release.Snapshot.Providers[target.ProviderID]
	slot := provider.Slots[0]
	slot.ID = "owned-slot"
	provider.Slots = append(provider.Slots, slot)
	h.rt.release.Snapshot.Providers[provider.ID] = provider
	bound := &resources.Resource{RouteSlug: route.Slug, RouteRevisionID: route.RevisionID, ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, TargetID: target.ID, SlotID: slot.ID, CredentialID: slot.CredentialID}
	x := &call{keyID: h.keyID, family: wire.Chat, request: request{release: h.rt.release, startedAt: time.Now()}}
	x.route = &route
	p, err := h.gateway.selectBoundPinSurface(t.Context(), x, &route, "generation", "openai", "unary", bound, func(*routing.Provider, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer h.gateway.resourceSettle(t.Context(), x, p)
	if !retainedAffinity(bound, p, &route) {
		t.Fatalf("selected a different owner: %+v", p)
	}
}

func TestRetainedProviderErrorDoesNotExposeIDs(t *testing.T) {
	param := "native_secret"
	original := &Error{Status: http.StatusBadRequest, Code: param, Type: param, Message: "bad " + param, Param: &param}
	got := retainedPublicError(original, nil, nil)
	if strings.Contains(got.Message, param) || strings.Contains(got.Code, param) || strings.Contains(got.Type, param) || got.Param != nil || got.Status != 400 {
		t.Fatalf("unsafe error: %+v", got)
	}
	if original.Param == nil || original.Message != "bad "+param {
		t.Fatal("mutated shared provider error")
	}
}
