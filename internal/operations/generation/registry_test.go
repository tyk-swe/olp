package generation

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

// fixtureDialect builds a minimal registered dialect over a plain document
// grammar: it lifts raw JSON, binds /model, decodes a native result and owns a
// reservation shape. Streaming fixtures add the native event grammar.
func fixtureDialect(id string, streaming bool) Dialect {
	identity := oif.Identity{ID: id, Revision: "fixture-1"}
	d := Dialect{
		Identity:  identity,
		Operation: Contract(),
		Surface:   "fixture-" + id,
		Label:     id,
		Evidence:  "fixture dialect registration",
		Streaming: streaming,
		Address:   Address{RelativePath: "fixture/" + id},
		IdentityRules: []oif.IdentityRule{
			{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String},
		},
		Lift: func(body []byte, route string, transportStream bool, maxBytes int) (Source, error) {
			document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxBytes})
			if err != nil {
				return Source{}, err
			}
			request, err := oif.NewRequest(Descriptor(identity, transportStream), document)
			if err != nil {
				return Source{}, err
			}
			return Source{Request: request, Route: route, Stream: transportStream}, nil
		},
		IdentityChanges: func(source Source, model string) ([]oif.Change, error) {
			value, _ := json.Marshal(model)
			return []oif.Change{{Pointer: "/model", Value: string(value), Origin: oif.IdentityBinding, Reason: "fixture model binding"}}, nil
		},
		DecodeNative: func(in DecodeInput) (*Native, error) {
			document, err := oif.ParseJSON(in.Body, oif.Limits{MaxBytes: in.MaxBytes})
			if err != nil {
				return nil, err
			}
			result, err := oif.NewResult(Descriptor(identity, false), document, oif.Complete)
			if err != nil {
				return nil, err
			}
			return &Native{Result: result, Body: in.Body, Route: in.Route, OutputText: "fixture"}, nil
		},
		Estimate:   func(oif.Document) Estimate { return Estimate{Input: 1} },
		Parameters: func(oif.Document) []string { return []string{"model"} },
	}
	if streaming {
		d.StreamNative = func(in StreamInput, emit func([]byte) error, observe func(oif.Event) error) (*Native, error) {
			return &Native{Route: in.Route}, nil
		}
		d.ValidateEvent = func(oif.Event) error { return nil }
	}
	return d
}

func TestRegistryRejectsUnqualifiedDialects(t *testing.T) {
	base := fixtureDialect("fixture-a", true)
	cases := map[string]func(Dialect) Dialect{
		"missing_revision":   func(d Dialect) Dialect { d.Identity.Revision = ""; return d },
		"missing_label":      func(d Dialect) Dialect { d.Label = ""; return d },
		"missing_evidence":   func(d Dialect) Dialect { d.Evidence = ""; return d },
		"label_mismatch":     func(d Dialect) Dialect { d.Label = "other-label"; return d },
		"wrong_operation":    func(d Dialect) Dialect { d.Operation = oif.Identity{ID: "other", Revision: "1"}; return d },
		"missing_lift":       func(d Dialect) Dialect { d.Lift = nil; return d },
		"missing_decode":     func(d Dialect) Dialect { d.DecodeNative = nil; return d },
		"missing_grammar":    func(d Dialect) Dialect { d.StreamNative = nil; return d },
		"missing_eventguard": func(d Dialect) Dialect { d.ValidateEvent = nil; return d },
		"dual_addressing":    func(d Dialect) Dialect { d.Address.LegacyPath = "chat"; return d },
		"no_addressing":      func(d Dialect) Dialect { d.Address = Address{}; return d },
		"unsafe_path":        func(d Dialect) Dialect { d.Address.RelativePath = "/abs/../x"; return d },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := NewRegistry().Register(mutate(base)); err == nil {
				t.Fatal("unqualified dialect registered")
			}
		})
	}
	// A unary dialect must not claim an event grammar.
	unary := fixtureDialect("fixture-unary", false)
	unary.StreamNative = base.StreamNative
	unary.ValidateEvent = base.ValidateEvent
	if err := NewRegistry().Register(unary); err == nil {
		t.Fatal("unary dialect claimed an event grammar")
	}
}

func TestRegistryLooksUpDialectsByIdentityAndLabel(t *testing.T) {
	registry := NewRegistry()
	dialect := fixtureDialect("fixture-b", true)
	if err := registry.Register(dialect); err != nil {
		t.Fatal(err)
	}
	if byID, ok := registry.Dialect(dialect.Identity); !ok || byID.Label != dialect.Label {
		t.Fatal("identity lookup failed")
	}
	if byLabel, ok := registry.DialectLabel(dialect.Label); !ok || byLabel.Identity != dialect.Identity {
		t.Fatal("label lookup failed")
	}
	if _, ok := registry.Dialect(oif.Identity{ID: dialect.Identity.ID, Revision: "other"}); ok {
		t.Fatal("unbounded revision matched a registered identity")
	}
	if err := registry.Register(dialect); err == nil {
		t.Fatal("duplicate identity registered")
	}
	twin := fixtureDialect("fixture-c", false)
	twin.Label = dialect.Label
	if err := registry.Register(twin); err == nil {
		t.Fatal("duplicate label registered")
	}
}

func TestRegistryQualifiesMappingsAndContracts(t *testing.T) {
	registry := NewRegistry()
	source, target := fixtureDialect("fixture-src", false), fixtureDialect("fixture-dst", true)
	for _, d := range []Dialect{source, target} {
		if err := registry.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	mapping := Mapping{Source: source.Identity, Target: target.Identity, Evidence: "fixture mapping", Lower: func(in LowerInput) (Lowered, error) {
		return Lowered{Document: in.Source.Document(), Evidence: "fixture"}, nil
	}, ProjectResult: func(in ProjectInput, result *Native, handle string) (*Continuation, Delivery, error) {
		return nil, Delivery{Body: result.Body}, nil
	}}
	if err := registry.RegisterMapping(mapping); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Mapping(source.Identity, target.Identity, ""); !ok {
		t.Fatal("stateless mapping not found")
	}
	if registry.KnownContract("test-contract-v1") {
		t.Fatal("unregistered contract reported known")
	}
	mapping.ClientContract = "test-contract-v1"
	if err := registry.RegisterMapping(mapping); err != nil {
		t.Fatal(err)
	}
	if !registry.KnownContract("test-contract-v1") || !registry.SourceContract(source.Identity, "test-contract-v1") {
		t.Fatal("contract lookup failed")
	}
	if registry.SourceContract(target.Identity, "test-contract-v1") {
		t.Fatal("contract claimed by the wrong source dialect")
	}
	if err := registry.RegisterMapping(mapping); err == nil {
		t.Fatal("duplicate mapping registered")
	}
	if err := registry.RegisterMapping(Mapping{Source: source.Identity, Target: source.Identity, Evidence: "x", Lower: mapping.Lower, ProjectResult: mapping.ProjectResult}); err == nil {
		t.Fatal("identity mapping registered")
	}
	unregistered := Mapping{Source: oif.Identity{ID: "absent", Revision: "1"}, Target: target.Identity, Evidence: "x", Lower: mapping.Lower, ProjectResult: mapping.ProjectResult}
	if err := registry.RegisterMapping(unregistered); err == nil {
		t.Fatal("mapping over unregistered source admitted")
	}
	if len(registry.Mappings(source.Identity)) != 2 || len(registry.Mappings(target.Identity)) != 0 {
		t.Fatal("mapping enumeration wrong")
	}
}

func TestRegistrySupportsTargetContracts(t *testing.T) {
	registry := NewRegistry()
	source, target := fixtureDialect("fixture-open", false), fixtureDialect("fixture-serving", true)
	for _, d := range []Dialect{source, target} {
		if err := registry.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	unary := fixtureDialect("fixture-unary-only", false)
	if err := registry.Register(unary); err != nil {
		t.Fatal(err)
	}
	if !registry.SupportsTarget(target.Identity, "native", "unary") || !registry.SupportsTarget(target.Identity, target.Surface, "streaming") {
		t.Fatal("native and own-surface capability not supported")
	}
	if registry.SupportsTarget(target.Identity, "other-surface", "unary") {
		t.Fatal("unqualified surface claimed")
	}
	if registry.SupportsTarget(unary.Identity, "native", "streaming") {
		t.Fatal("unary-only dialect claimed streaming")
	}
	if registry.SupportsTarget(target.Identity, "native", "async") {
		t.Fatal("unbounded delivery mode claimed")
	}
	if registry.SupportsTarget(oif.Identity{ID: "absent", Revision: "1"}, "native", "unary") {
		t.Fatal("unregistered dialect claimed")
	}
	mapping := Mapping{Source: source.Identity, Target: target.Identity, Evidence: "x", Lower: func(in LowerInput) (Lowered, error) {
		return Lowered{Document: in.Source.Document()}, nil
	}, ProjectResult: func(in ProjectInput, result *Native, handle string) (*Continuation, Delivery, error) {
		return nil, Delivery{Body: result.Body}, nil
	}}
	if err := registry.RegisterMapping(mapping); err != nil {
		t.Fatal(err)
	}
	if !registry.SupportsTarget(target.Identity, source.Surface, "unary") {
		t.Fatal("qualified mapping surface not supported")
	}
}

func TestRegistryPreparesDeclaredIdentityOnly(t *testing.T) {
	registry := NewRegistry()
	dialect := fixtureDialect("fixture-identity", false)
	if err := registry.Register(dialect); err != nil {
		t.Fatal(err)
	}
	source, err := dialect.Lift([]byte(`{"model":"caller","text":"hi"}`), "route", false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := dialect.IdentityChanges(source, "serving-model")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := registry.PrepareIdentity(source.Request, source.Descriptor(), changes)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := prepared.Document().Root().Lookup("model"); !ok || value.Raw() != `"serving-model"` {
		t.Fatalf("declared identity binding missing: %s", prepared.Document().Bytes())
	}
	if _, err := registry.PrepareIdentity(source.Request, source.Descriptor(), []oif.Change{{Pointer: "/text", Value: `"tampered"`, Origin: oif.IdentityBinding}}); err == nil {
		t.Fatal("undeclared identity overlay admitted")
	}
}
