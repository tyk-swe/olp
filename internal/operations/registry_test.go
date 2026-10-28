package operations

import (
	"net/url"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func registryTestDialect(id string) Dialect {
	return Dialect{
		Identity:  oif.Identity{ID: id, Revision: Revision},
		Operation: oif.Identity{ID: "search", Revision: Revision},
		Surface:   "native",
		Label:     id + " test dialect",
		Address:   Address{RelativePath: "search"},
		Evidence:  id + "-results/1",
		Request:   func(oif.Request) (oif.View, error) { return nil, nil },
		Result:    func(oif.Request, oif.Result) (oif.View, error) { return nil, nil },
		Probe:     func(string) []byte { return []byte(`{}`) },
	}
}

func TestRegisterNormalizesEmptyTransportToJSON(t *testing.T) {
	if Transport("").Normalized() != TransportJSON {
		t.Fatal("an omitted transport must mean the JSON transport")
	}
	for _, transport := range []Transport{TransportJSON, TransportGetQuery, TransportPoll202} {
		if transport.Normalized() != transport {
			t.Fatalf("normalization rewrote the explicit transport %q", transport)
		}
	}
	registry := NewRegistry()
	dialect := registryTestDialect("plain-json")
	if err := registry.Register(dialect); err != nil {
		t.Fatal(err)
	}
	stored, ok := registry.Lookup("plain-json", Revision)
	if !ok {
		t.Fatal("registered dialect vanished")
	}
	if stored.Transport != TransportJSON {
		t.Fatalf("stored dialect kept transport %q instead of the JSON default", stored.Transport)
	}
}

func TestRegisterAcceptsGetQueryAndPoll202Shapes(t *testing.T) {
	registry := NewRegistry()
	getQuery := registryTestDialect("brave-shaped")
	getQuery.Transport = TransportGetQuery
	getQuery.Query = func(oif.Request) (url.Values, error) { return url.Values{"q": {"olp"}}, nil }
	if err := registry.Register(getQuery); err != nil {
		t.Fatal(err)
	}
	stored, ok := registry.Lookup("brave-shaped", Revision)
	if !ok || stored.Transport != TransportGetQuery || stored.Query == nil {
		t.Fatalf("get-query dialect lost its lowering: %+v", stored)
	}
	values, err := stored.Query(oif.Request{})
	if err != nil || values.Get("q") != "olp" {
		t.Fatalf("stored query lowering stopped working: %v %v", values, err)
	}

	polling := registryTestDialect("azure-shaped")
	polling.Transport = TransportPoll202
	polling.Wire = func(oif.View) (WireRequest, error) {
		return WireRequest{Query: url.Values{"api-version": {"1"}}, Body: []byte(`{"base64Source":"AA=="}`)}, nil
	}
	polling.Poll = func(oif.Document) (PollState, error) { return PollSucceeded, nil }
	polling.CancelPoll = func(pollURL string) (string, string, bool) { return "DELETE", pollURL, pollURL != "" }
	if err := registry.Register(polling); err != nil {
		t.Fatal(err)
	}
	stored, ok = registry.Lookup("azure-shaped", Revision)
	if !ok || stored.Transport != TransportPoll202 || stored.Wire == nil || stored.Poll == nil || stored.CancelPoll == nil {
		t.Fatalf("poll-202 dialect lost a polling hook: %+v", stored)
	}
	wire, err := stored.Wire(nil)
	if err != nil || wire.Query.Get("api-version") != "1" || len(wire.Body) == 0 {
		t.Fatalf("stored wire lowering stopped working: %+v %v", wire, err)
	}
	if state, err := stored.Poll(oif.Document{}); err != nil || state != PollSucceeded {
		t.Fatalf("stored poll reading stopped working: %q %v", state, err)
	}
	if method, target, ok := stored.CancelPoll("https://provider.example/results/1"); !ok || method != "DELETE" || target == "" {
		t.Fatalf("stored cancel hook stopped working: %q %q %v", method, target, ok)
	}
}

func TestRegisterRejectsTransportHookMismatches(t *testing.T) {
	query := func(oif.Request) (url.Values, error) { return nil, nil }
	wire := func(oif.View) (WireRequest, error) { return WireRequest{}, nil }
	poll := func(oif.Document) (PollState, error) { return PollPending, nil }
	cancel := func(string) (string, string, bool) { return "", "", false }
	for _, test := range []struct {
		name   string
		mutate func(*Dialect)
	}{
		{"json with query lowering", func(d *Dialect) { d.Query = query }},
		{"json with wire lowering", func(d *Dialect) { d.Wire = wire }},
		{"json with poll reading", func(d *Dialect) { d.Poll = poll }},
		{"json with cancel hook", func(d *Dialect) { d.CancelPoll = cancel }},
		{"get-query without query lowering", func(d *Dialect) { d.Transport = TransportGetQuery }},
		{"get-query with wire lowering", func(d *Dialect) { d.Transport, d.Query, d.Wire = TransportGetQuery, query, wire }},
		{"get-query with poll reading", func(d *Dialect) { d.Transport, d.Query, d.Poll = TransportGetQuery, query, poll }},
		{"poll-202 without wire lowering", func(d *Dialect) { d.Transport, d.Poll = TransportPoll202, poll }},
		{"poll-202 without poll reading", func(d *Dialect) { d.Transport, d.Wire = TransportPoll202, wire }},
		{"poll-202 with query lowering", func(d *Dialect) { d.Transport, d.Wire, d.Poll, d.Query = TransportPoll202, wire, poll, query }},
		{"unregistered transport", func(d *Dialect) { d.Transport = Transport("carrier-pigeon") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := NewRegistry()
			dialect := registryTestDialect("mismatched")
			test.mutate(&dialect)
			if err := registry.Register(dialect); err == nil {
				t.Fatal("mismatched dialect was accepted")
			}
			if _, ok := registry.Lookup("mismatched", Revision); ok {
				t.Fatal("rejected dialect became dispatchable")
			}
			if registry.HasOperation(dialect.Operation.ID) {
				t.Fatal("rejected dialect enabled its operation")
			}
		})
	}
}
