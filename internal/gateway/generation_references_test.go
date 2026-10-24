package gateway

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

func generationReferenceDocument(t *testing.T, body string) oif.Document {
	t.Helper()
	document, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestGenerationResourceReferencesUseDialectPositions(t *testing.T) {
	source := generationReferenceDocument(t, `{"conversation":{"id":"conv-local"},"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-local"}]},{"type":"function_call","arguments":"{\"file_id\":\"application-data\"}"},{"type":"computer_call_output","output":{"type":"computer_screenshot","file_id":"screenshot-local"}}],"tools":[{"type":"file_search","vector_store_ids":["store-local"]},{"type":"function","parameters":{"file_id":"schema-data"}}],"metadata":{"file_id":"application-data"}}`)
	refs, err := generationReferences(source, wire.Responses)
	if err != nil || len(refs) != 4 {
		t.Fatalf("references: %+v %v", refs, err)
	}
	for i, path := range []string{"/conversation/id", "/input/0/content/0/file_id", "/input/2/output/file_id", "/tools/0/vector_store_ids/0"} {
		if refs[i].pointer != path {
			t.Fatalf("reference %d: %+v", i, refs[i])
		}
	}
	for _, body := range []string{`{"conversation":{}}`, `{"conversation":false}`, `{"tools":[{"type":"file_search","vector_store_ids":[7]}]}`, `{"input":[{"type":"item_reference","id":"native"}]}`} {
		if _, err := generationReferences(generationReferenceDocument(t, body), wire.Responses); err == nil {
			t.Fatalf("accepted malformed reference: %s", body)
		}
	}
}

func TestGenerationResourceRawIDsRefusedWithoutDispatch(t *testing.T) {
	for _, tc := range []struct {
		family wire.Format
		body   string
	}{
		{wire.Responses, `{"model":"route","input":"hello","store":false,"conversation":"conv_native"}`},
		{wire.Responses, `{"model":"route","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_native"}]}],"store":false}`},
		{wire.Responses, `{"model":"route","input":"hello","store":false,"tools":[{"type":"file_search","vector_store_ids":["vs_native"]}]}`},
		{wire.Gemini, `{"contents":[{"parts":[{"text":"hello"}]}],"cachedContent":"cachedContents/native"}`},
		{wire.GeminiStream, `{"contents":[{"parts":[{"text":"hello"}]}],"cachedContent":"cachedContents/native"}`},
	} {
		parsed, err := protocols.Parse(tc.family, []byte(tc.body), "route")
		if err != nil {
			t.Fatal(err)
		}
		x := &call{keyID: "owner", generation: generationCall{parsed: parsed}}
		x.authority.Policy.AllowProviderState = true
		// Native spellings are rejected by the local-ID parser before any DB
		// access. A nil pool makes accidental resource adoption observable.
		s := &Server{Resources: resources.New(nil)}
		failure := s.bindGenerationReferences(t.Context(), x)
		if failure == nil || failure.Code != "invalid_resource_reference" || x.dispatched {
			t.Fatalf("accepted native reference: %s failure=%v", tc.body, failure)
		}
	}
}

func TestGenerationResourceCombinedAffinity(t *testing.T) {
	credential := "credential"
	base := resources.Resource{APIKeyID: "owner", RouteSlug: "route", RouteRevisionID: "route-revision", ProviderID: "provider", ProviderRevisionID: "provider-revision", TargetID: "target", SlotID: "slot", CredentialID: &credential, Metadata: []byte(`{"upstream_model":"model"}`)}
	for _, change := range []func(*resources.Resource){
		func(r *resources.Resource) { r.APIKeyID = "other" },
		func(r *resources.Resource) { r.ProviderRevisionID = "other" },
		func(r *resources.Resource) { r.RouteRevisionID = "other" },
		func(r *resources.Resource) { r.TargetID = "other" },
		func(r *resources.Resource) { r.SlotID = "other" },
		func(r *resources.Resource) { r.CredentialID = nil },
		func(r *resources.Resource) { r.Metadata = []byte(`{"upstream_model":"other"}`) },
	} {
		other := base
		change(&other)
		if sameGenerationAffinity(&base, &other) {
			t.Fatal("accepted different generation authority")
		}
	}
	other := base
	other.Kind = "conversation"
	if !sameGenerationAffinity(&base, &other) {
		t.Fatal("compatible resource kinds did not share authority")
	}
}

func TestGenerationResourceResultProjectionUnaryAndStream(t *testing.T) {
	for _, body := range []string{
		`{"id":"response-native","conversation":{"id":"conv-native"},"tools":[{"type":"file_search","vector_store_ids":["store-native"]}],"output":[{"type":"file_search_call","results":[{"file_id":"file-native","score":1.2300}]},{"type":"message","content":[{"type":"output_text","text":"answer","annotations":[{"type":"file_citation","file_id":"file-native"}]}]},{"type":"function_call","arguments":"{\"file_id\":\"opaque\"}"}]}`,
		`{"type":"response.output_item.done","item":{"type":"file_search_call","results":[{"file_id":"file-native","score":1.2300}]}}`,
		`{"type":"response.output_text.annotation.added","annotation":{"type":"file_citation","file_id":"file-native"}}`,
	} {
		doc := generationReferenceDocument(t, body)
		projected, err := projectGenerationReferences(doc, func(kind, id string) (string, error) { return kind + "-local", nil })
		if err != nil || strings.Contains(string(projected.Bytes()), `"file-native"`) {
			t.Fatalf("projection: %s %v", projected.Bytes(), err)
		}
		if strings.Contains(body, "1.2300") && !strings.Contains(string(projected.Bytes()), "1.2300") {
			t.Fatal("projection normalized native score")
		}
		if strings.Contains(body, "opaque") && !strings.Contains(string(projected.Bytes()), "opaque") {
			t.Fatal("projection changed tool arguments")
		}
		if _, err := projectGenerationReferences(doc, func(string, string) (string, error) { return "", resources.ErrNotFound }); err == nil {
			t.Fatal("unowned result ID was exposed")
		}
	}
}

func TestGenerationResourceReferencesPreventCacheReplay(t *testing.T) {
	for _, body := range []string{`{"cachedContent":"context_cache_local"}`, `{"tools":[{"type":"file_search","vector_store_ids":["vector_store_local"]}]}`} {
		if !containsCacheReference(generationReferenceDocument(t, body).Root()) {
			t.Fatalf("stateful cache replay admitted: %s", body)
		}
	}
}

func TestGenerationResourceDefaultsCannotInjectAuthority(t *testing.T) {
	for _, tc := range []struct {
		family wire.Format
		body   string
	}{
		{wire.Gemini, `{"cachedContent":"cachedContents/native"}`},
		{wire.Responses, `{"conversation":"native"}`},
		{wire.Responses, `{"previous_response_id":"native"}`},
		{wire.Responses, `{"tools":[{"type":"file_search","vector_store_ids":["native"]}]}`},
		{wire.Responses, `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"native"}]}]}`},
	} {
		x := &call{generation: generationCall{parsed: &wire.Request{Family: tc.family}}}
		if err := x.checkGenerationResourceDestination(&routing.Provider{}, "model", tc.family, generationReferenceDocument(t, tc.body)); err == nil {
			t.Fatalf("defaults injected unresolved provider resource: %s", tc.body)
		}
	}
}

func TestGenerationResourceContainerExecutionRefusedBeforeDispatch(t *testing.T) {
	for _, body := range []string{
		`{"model":"route","input":"hello","store":false,"tools":[{"type":"code_interpreter","container":"container_owned"}]}`,
		`{"model":"route","input":"hello","store":false,"tools":[{"type":"code_interpreter","container":{"type":"auto","file_ids":["file_owned"]}}]}`,
		`{"model":"route","input":[{"type":"code_interpreter_call","id":"call","container_id":"native-container"}],"store":false}`,
	} {
		parsed, err := protocols.Parse(wire.Responses, []byte(body), "")
		if err != nil {
			t.Fatal(err)
		}
		x := &call{generation: generationCall{parsed: parsed}}
		s := &Server{}
		if failure := s.bindGenerationReferences(t.Context(), x); failure == nil || failure.Code != "invalid_resource_reference" || x.dispatched {
			t.Fatalf("container execution acquired lifecycle authority: %v", failure)
		}
		if err := x.checkGenerationResourceDestination(&routing.Provider{}, "model", wire.Responses, parsed.OIF().Document()); err == nil {
			t.Fatal("defaults introduced unqualified container execution")
		}
	}
}

func TestGenerationResourceCacheTokensReachAdmissionCostAndContext(t *testing.T) {
	x := costExecution(t, `{"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":5}}`, wire.Gemini, 1, routing.Attempt{UpstreamModel: "gemini-2.5-flash", Price: costPrice()})
	attempt := x.attempts[0]
	provider := x.snapshot().Providers[attempt.ProviderID]
	provider.Kind = "gemini"
	x.snapshot().Providers[provider.ID] = provider
	base, err := x.attemptEstimate(&provider, attempt.TargetID, attempt.UpstreamModel)
	if err != nil {
		t.Fatal(err)
	}
	baseContext := x.sourceDemand(attempt.UpstreamModel)
	baseCost := (&Server{}).costReservation(x, budgeted()).amount
	parsed, err := x.generation.parsed.WithField("cachedContent", json.RawMessage(`"cachedContents/native"`))
	if err != nil {
		t.Fatal(err)
	}
	x.generation.parsed = parsed
	x.generation.resources = &generationResources{resolved: map[string]string{"/cachedContent": "cachedContents/native"}, cacheInput: 1000}
	serving := provider.Connector().ServingIdentity(provider.ID, provider.RevisionID, attempt.UpstreamModel)
	x.serving = &serving
	for range 2 {
		bound, err := x.attemptEstimate(&provider, attempt.TargetID, attempt.UpstreamModel)
		if err != nil || bound.input != base.input+1000 || bound.reserve != base.reserve+1000 {
			t.Fatalf("cached input not reserved exactly once: base=%+v bound=%+v error=%v", base, bound, err)
		}
		context := x.sourceDemand(attempt.UpstreamModel)
		if context.EstimatedInputTokens != baseContext.EstimatedInputTokens+1000 || *context.CostInputTokens != *baseContext.CostInputTokens+1000 {
			t.Fatalf("cached context omitted: base=%+v bound=%+v", baseContext, context)
		}
		cost := (&Server{}).costReservation(x, budgeted()).amount
		before, ok := new(big.Rat).SetString(baseCost)
		if !ok {
			t.Fatal(baseCost)
		}
		after, ok := new(big.Rat).SetString(cost)
		if !ok {
			t.Fatal(cost)
		}
		if new(big.Rat).Sub(after, before).Cmp(big.NewRat(1, 1000)) != 0 {
			t.Fatalf("cached input monetary exposure omitted: before=%s after=%s", baseCost, cost)
		}
	}
	if e := x.summarizeSource().prompt.WithRetainedInputTokens(estimate.MaxTokens).Estimate(estimate.ForModel(attempt.UpstreamModel), nil); e.Input != estimate.MaxTokens || e.ContextInput != estimate.MaxTokens || e.Tokens() != estimate.MaxTokens {
		t.Fatalf("retained input count overflowed limiter: %+v", e)
	}
}

func TestGenerationResourceUnknownInputsRefuseConsumptionCaps(t *testing.T) {
	provider := routing.Provider{ID: "provider", Slots: []routing.Slot{{ID: "slot"}}}
	route := routing.Route{Slug: "route"}
	p := &pin{provider: provider, slot: provider.Slots[0]}
	current := &routing.Snapshot{Routes: map[string]routing.Route{route.Slug: route}, Providers: map[string]routing.Provider{provider.ID: provider}}
	s := &Server{Runtime: &fakeRuntime{release: &runtime.Release{Snapshot: current}}}
	x := &call{}
	unknown := &generationResources{unknownInput: true}
	if failure := s.generationReferenceBudget(x, p, &route, unknown); failure != nil {
		t.Fatal(failure)
	}
	x.authority.Policy.TokensPerMinute = new(int64(100))
	if failure := s.generationReferenceBudget(x, p, &route, unknown); failure == nil || failure.Code != "unbounded_work_budget" {
		t.Fatalf("remote history/retrieval bypassed token cap: %v", failure)
	}
	if failure := s.generationReferenceBudget(x, p, &route, &generationResources{cacheInput: 1000}); failure != nil {
		t.Fatalf("authoritative cache tokens treated as unknown: %v", failure)
	}
	x.authority.Policy.TokensPerMinute = nil
	provider.Slots[0].Supply = routing.Supply{DailyCostLimit: new("1")}
	current.Providers[provider.ID] = provider
	if failure := s.generationReferenceBudget(x, p, &route, unknown); failure == nil || failure.Code != "unbounded_work_budget" {
		t.Fatalf("remote history/retrieval bypassed late slot cost cap: %v", failure)
	}
}

func TestGenerationResourceGeminiFilesRequireQualifiedAuthority(t *testing.T) {
	for _, family := range []wire.Format{wire.Gemini, wire.GeminiStream} {
		for _, body := range []string{
			`{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"files/native"}}]}]}`,
			`{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/native"}}]}]}`,
			`{"contents":[{"parts":[{"text":"hello"}]}],"cachedContent":"context_cache_owned","systemInstruction":{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://GENERATIVELANGUAGE.googleapis.com./v1beta/files/native"}}]}}`,
			`{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"files/native"}}]}],"cachedContent":"context_cache_owned"}`,
		} {
			parsed, err := protocols.Parse(family, []byte(body), "route")
			if err != nil {
				t.Fatal(err)
			}
			x := &call{generation: generationCall{parsed: parsed}}
			s := &Server{}
			if failure := s.bindGenerationReferences(t.Context(), x); failure == nil || failure.Code != "invalid_resource_reference" || x.dispatched {
				t.Fatalf("provider file reference escaped ownership before dispatch: %v", failure)
			}
			if err := x.checkGenerationResourceDestination(&routing.Provider{}, "model", wire.Gemini, parsed.OIF().Document()); err == nil {
				t.Fatal("defaults introduced unqualified Gemini file reference")
			}
		}
	}
	doc := generationReferenceDocument(t, `{"contents":[{"parts":[{"fileData":{"mimeType":"text/plain","fileUri":"https://cdn.example.test/file.txt"}},{"functionResponse":{"name":"lookup","response":{"fileData":{"fileUri":"files/application-data"}}}}]}]}`)
	if refs, err := generationReferences(doc, wire.Gemini); err != nil || len(refs) != 0 {
		t.Fatalf("external URL or opaque function data acquired resource semantics: %+v %v", refs, err)
	}
}
