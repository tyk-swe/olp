package interaction

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
)

// The fixture dialect registers the compatible-chat profile's operation label
// in a private registry: the same JSON grammar as the built-in adapter would
// provide, owned entirely by the test contract. It proves a dialect can sit
// behind the operation-owned contract without touching generic orchestration.
func fixtureGenerationDialect(id, surface string, streaming bool) generation.Dialect {
	identity := oif.Identity{ID: id, Revision: "fixture-1"}
	d := generation.Dialect{
		Identity:  identity,
		Operation: generation.Contract(),
		Surface:   surface,
		Label:     id,
		Evidence:  "fixture dialect registration",
		Streaming: streaming,
		Address:   generation.Address{RelativePath: "fixture/" + id},
		IdentityRules: []oif.IdentityRule{
			{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String},
		},
		Lift: func(body []byte, route string, transportStream bool, maxBytes int) (generation.Source, error) {
			document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxBytes})
			if err != nil {
				return generation.Source{}, err
			}
			request, err := oif.NewRequest(generation.Descriptor(identity, transportStream), document)
			if err != nil {
				return generation.Source{}, err
			}
			return generation.Source{Request: request, Route: route, Stream: transportStream}, nil
		},
		IdentityChanges: func(source generation.Source, model string) ([]oif.Change, error) {
			value, _ := json.Marshal(model)
			return []oif.Change{{Pointer: "/model", Value: string(value), Origin: oif.IdentityBinding, Reason: "fixture model binding"}}, nil
		},
		DecodeNative: func(in generation.DecodeInput) (*generation.Native, error) {
			document, err := oif.ParseJSON(in.Body, oif.Limits{MaxBytes: in.MaxBytes})
			if err != nil {
				return nil, err
			}
			result, err := oif.NewResult(generation.Descriptor(identity, in.Source.Stream), document, oif.Complete)
			if err != nil {
				return nil, err
			}
			return &generation.Native{Result: result, Body: in.Body, Route: in.Route, OutputText: "fixture", Usage: &generation.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}}, nil
		},
		Estimate:   func(oif.Document) generation.Estimate { return generation.Estimate{Input: 7} },
		Parameters: func(oif.Document) []string { return []string{"model", "fixture-param"} },
	}
	if streaming {
		d.StreamNative = func(in generation.StreamInput, emit func([]byte) error, observe func(oif.Event) error) (*generation.Native, error) {
			return &generation.Native{Route: in.Route}, nil
		}
		d.ValidateEvent = func(oif.Event) error { return nil }
	}
	return d
}

func fixtureRegistry(t *testing.T, dialects ...generation.Dialect) *generation.Registry {
	t.Helper()
	registry := generation.NewRegistry()
	for _, d := range dialects {
		if err := registry.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func fixtureTemplate(t *testing.T, registry *generation.Registry) *Template {
	t.Helper()
	config := configuration(t, "compatible-chat")
	config.Registry = registry
	return template(t, config)
}

func fixtureSource(t *testing.T, dialect generation.Dialect, body string, stream bool) generation.Source {
	t.Helper()
	source, err := dialect.Lift([]byte(body), "route", stream, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestFixtureDialectBindsNativeUnaryThroughContract(t *testing.T) {
	dialect := fixtureGenerationDialect("openai-chat", "fixture", false)
	template := fixtureTemplate(t, fixtureRegistry(t, dialect))
	plan := bindSource(t, template, fixtureSource(t, dialect, `{"model":"route","messages":[{"role":"user","content":"hi"}]}`, false), Context{})
	if plan.Receipt().Class != NativeIdentity || plan.Receipt().SourceDialect != "openai-chat" || plan.Receipt().TargetDialect != "openai-chat" {
		t.Fatalf("unexpected receipt %+v", plan.Receipt())
	}
	if model, ok := plan.Effective().Root().Lookup("model"); !ok || model.Raw() != `"native-model"` {
		t.Fatalf("serving model not bound through dialect identity contract: %s", plan.Effective().Bytes())
	}
	if got := plan.Parameters(); len(got) != 2 || got[0] != "model" || got[1] != "fixture-param" {
		t.Fatalf("parameters did not come from the dialect contract: %v", got)
	}
	if plan.Estimate().Input != 7 {
		t.Fatalf("reservation did not come from the dialect contract: %+v", plan.Estimate())
	}
}

func TestFixtureDialectRejectsUndeclaredDeliveryAndSources(t *testing.T) {
	dialect := fixtureGenerationDialect("openai-chat", "fixture", false)
	other := fixtureGenerationDialect("fixture-alt", "fixture-alt", false)
	template := fixtureTemplate(t, fixtureRegistry(t, dialect, other))

	if _, err := template.Bind(fixtureSource(t, dialect, `{"model":"route","messages":[]}`, true), Context{}); err == nil {
		t.Fatal("unary fixture admitted incremental delivery")
	}
	if _, err := template.Bind(generation.Source{Route: "route"}, Context{}); err == nil {
		t.Fatal("empty source admitted")
	}
	// The alternate source dialect is registered but no qualified mapping
	// reaches the compatible-chat target: the binding fails closed.
	if _, err := template.Bind(fixtureSource(t, other, `{"model":"route","messages":[]}`, false), Context{}); err == nil {
		t.Fatal("unmapped source dialect admitted")
	} else {
		assertReason(t, err, "target_capability")
	}
	// A caller dialect the registry has never seen fails at admission, not in
	// the provider path.
	unregistered := fixtureGenerationDialect("fixture-unknown", "fixture-unknown", false)
	if _, err := template.Bind(fixtureSource(t, unregistered, `{"model":"route"}`, false), Context{}); err == nil {
		t.Fatal("unregistered source dialect admitted")
	} else {
		assertReason(t, err, "target_capability")
	}
}

func TestFixtureDialectQualifiedMappingBindsAcrossDialects(t *testing.T) {
	target := fixtureGenerationDialect("openai-chat", "fixture", false)
	source := fixtureGenerationDialect("fixture-alt", "fixture-alt", false)
	registry := fixtureRegistry(t, target, source)
	mapping := generation.Mapping{
		Source: source.Identity, Target: target.Identity, Evidence: "fixture mapping evidence",
		Lower: func(in generation.LowerInput) (generation.Lowered, error) {
			return generation.Lowered{Document: in.Source.Document(), Evidence: "fixture lowering"}, nil
		},
		ProjectResult: func(in generation.ProjectInput, result *generation.Native, handle string) (*generation.Continuation, generation.Delivery, error) {
			return nil, generation.Delivery{Body: result.Body}, nil
		},
	}
	if err := registry.RegisterMapping(mapping); err != nil {
		t.Fatal(err)
	}
	plan := bindSource(t, fixtureTemplate(t, registry), fixtureSource(t, source, `{"model":"route","messages":[{"role":"user","content":"hi"}]}`, false), Context{})
	if plan.Receipt().Class != QualifiedInteraction {
		t.Fatalf("mapping did not produce a qualified interaction: %+v", plan.Receipt())
	}
	if plan.Receipt().Obligations.Continuation != "stateless_text_history" {
		t.Fatalf("stateless mapping claimed a continuation obligation: %+v", plan.Receipt().Obligations)
	}
}

func bindSource(t *testing.T, template *Template, source generation.Source, context Context) *Plan {
	t.Helper()
	plan, err := template.Bind(source, context)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
