package gateway

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestRetainedPinnedEvalFixtures(t *testing.T) {
	for _, tc := range []struct{ file, kind string }{{"openai-eval-definition", "eval"}, {"openai-eval-output", "eval_output_item"}} {
		body, err := os.ReadFile("../resources/testdata/retained/" + tc.file + ".json")
		if err != nil {
			t.Fatal(err)
		}
		document, err := oif.ParseJSON(body, oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: tc.kind, Operation: "eval", Identity: "id", Collection: "data"}, Action: "get"}
		bound, err := retainedapi.Rewrite(document, retainedResponseRefs(e, document), func(kind, id string) (string, error) {
			if id == "grader-diagnostic-id" || id == "line-1" {
				t.Fatal("diagnostic identity became a resource")
			}
			return kind + "-local", nil
		})
		if err != nil || !strings.Contains(bound.Raw(), tc.kind+"-local") {
			t.Fatalf("fixture %s=%s %v", tc.file, bound.Raw(), err)
		}
	}
}

func TestRetainedContainerMultipartFileReference(t *testing.T) {
	s := &Server{cfg: Config{MaxBodyBytes: 4096}}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "container", Kind: "container_file"}, Action: "create", Method: http.MethodPost}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("file_id", "file-local")
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	_, source, contentType, failure := s.readRetainedBody(request, e)
	if failure != nil {
		t.Fatal(failure)
	}
	bound, err := retainedapi.Rewrite(source, retainedRequestRefs(e), func(kind, id string) (string, error) {
		if kind != resources.KindFile || id != "file-local" {
			t.Fatal("multipart source lost typed authority")
		}
		return "file-native", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, nativeType, err := encodeRetainedRequestBody(e, bound, contentType)
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(nativeType)
	form, err := multipart.NewReader(bytes.NewReader(encoded), params["boundary"]).ReadForm(4096)
	if err != nil || form.Value["file_id"][0] != "file-native" {
		t.Fatalf("native multipart=%s %v", encoded, err)
	}
	defer form.RemoveAll()
	cache, _ := oif.ParseJSON([]byte(`{"model":"caller-model"}`), oif.Limits{})
	cache, err = bindRetainedModel(cache, retainedapi.Endpoint{Family: retainedapi.Family{Kind: "context_cache", Operation: "context_cache"}, Action: "create"}, &pin{model: "cache-model"})
	if err != nil || !strings.Contains(cache.Raw(), `"models/cache-model"`) {
		t.Fatalf("native cache model=%s %v", cache.Raw(), err)
	}
	container := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "container"}, Action: "create"}
	unchanged, err := retainedCorrelationBody(container, &resources.Resource{UUID: uuid.New()}, []byte(`{"name":"sandbox"}`))
	if err != nil || string(unchanged) != `{"name":"sandbox"}` {
		t.Fatal("metadata injected into a container API which does not support it")
	}
}

func TestRetainedEvalSourceCodecs(t *testing.T) {
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "eval", Kind: "eval_run"}, Action: "create"}
	for _, body := range []string{`{"data_source":{"type":"jsonl","source":{"type":"file_id","id":"file-local"}}}`, `{"data_source":{"type":"responses","source":{"type":"file_id","id":"file-local"},"model":"m"}}`} {
		document, _ := oif.ParseJSON([]byte(body), oif.Limits{})
		if err := validateEvalSource(document.Root(), e); err != nil {
			t.Fatal(err)
		}
		bound, err := retainedapi.Rewrite(document, retainedRequestRefsForSource(e, document), func(kind, id string) (string, error) {
			if kind != resources.KindFile || id != "file-local" {
				t.Fatal("incorrect source authority")
			}
			return "file-native", nil
		})
		if err != nil || !bytes.Contains(bound.Bytes(), []byte(`"id":"file-native"`)) {
			t.Fatalf("binding=%s %v", bound.Raw(), err)
		}
	}
	raw := `{"data_source":{"type":"jsonl","source":{"type":"file_content","content":[{"item":{"id":"customer-id","file_id":"opaque-user-data"}}]}}}`
	document, _ := oif.ParseJSON([]byte(raw), oif.Limits{})
	bound, err := retainedapi.Rewrite(document, retainedRequestRefsForSource(e, document), func(string, string) (string, error) {
		t.Fatal("inline dataset data became a resource authority")
		return "", nil
	})
	if err != nil || bound.Raw() != raw {
		t.Fatalf("inline content changed: %s %v", bound.Raw(), err)
	}
	for _, body := range []string{`{"data_source":{"type":"completions","source":{"type":"stored_completions","metadata":{"user":"foreign"}}}}`, `{"data_source":{"type":"jsonl","source":{"type":"file_id"}}}`} {
		document, _ := oif.ParseJSON([]byte(body), oif.Limits{})
		if err := validateEvalSource(document.Root(), e); err == nil {
			t.Fatal("unowned native dataset admitted")
		}
	}
	trajectory, _ := oif.ParseJSON([]byte(`{"data_source":{"type":"responses","model":"m","input_messages":{"type":"item_reference","item_reference":"item.trajectory"},"source":{"type":"file_content","content":[]}}}`), oif.Limits{})
	if err := validateEvalSource(trajectory.Root(), e); err == nil {
		t.Fatal("unverified per-item native trajectory admitted")
	}
}

func TestRetainedEvalHostedToolsRequireQualifiedContracts(t *testing.T) {
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "eval", Kind: "eval_run"}, Action: "create"}
	for _, tool := range []string{
		`{"type":"code_interpreter","container":"container-foreign"}`,
		`{"type":"file_search","vector_store_ids":["vs-foreign"]}`,
		`{"type":"web_search"}`,
		`{"type":"mcp","server_url":"https://example.test"}`,
	} {
		document := generationReferenceDocument(t, `{"data_source":{"type":"responses","model":"m","source":{"type":"file_content","content":[]},"sampling_params":{"tools":[`+tool+`]}}}`)
		if err := validateEvalSource(document.Root(), e); err == nil {
			t.Fatalf("unqualified hosted tool admitted: %s", tool)
		}
	}
	for _, choice := range []string{`"auto"`, `{"type":"function","name":"lookup"}`} {
		document := generationReferenceDocument(t, `{"data_source":{"type":"responses","model":"m","source":{"type":"file_content","content":[]},"sampling_params":{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":`+choice+`}}}`)
		if err := validateEvalSource(document.Root(), e); err != nil {
			t.Fatalf("function tool refused: %v", err)
		}
	}
	document := generationReferenceDocument(t, `{"data_source":{"type":"responses","model":"m","source":{"type":"file_content","content":[]},"sampling_params":{"tool_choice":{"type":"code_interpreter"}}}}`)
	if err := validateEvalSource(document.Root(), e); err == nil {
		t.Fatal("hosted tool choice admitted without a qualified tool")
	}
}

func TestRetainedCachePinsResolvedServingModel(t *testing.T) {
	p := &pin{model: "logical-model", provider: routing.Provider{Kind: "gemini", Bindings: map[string]connectors.Binding{"logical-model": {Model: "native-model"}}}}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "context_cache", Kind: "context_cache"}, Action: "create"}
	source := generationReferenceDocument(t, `{"model":"caller-model"}`)
	metadata, err := retainedInputMetadata(p, nil, "key", e, source, nil)
	if err != nil || metadata["upstream_model"] != "logical-model" || metadata["serving_model"] != "native-model" {
		t.Fatalf("model authority not pinned: %v %v", metadata, err)
	}
	native, err := bindRetainedModel(source, e, p)
	model, _ := native.Lookup("/model")
	name, _ := model.Text()
	if err != nil || name != "models/native-model" {
		t.Fatalf("native request model: %s %v", native.Raw(), err)
	}
	raw, _ := json.Marshal(metadata)
	r := &resources.Resource{Kind: "context_cache", Metadata: raw}
	response := generationReferenceDocument(t, `{"model":"models/native-model","usageMetadata":{"totalTokenCount":100},"expireTime":"2026-10-24T12:00:00Z"}`)
	if err := resources.EnrichRetained(r, e, response, time.Now()); err != nil {
		t.Fatalf("valid bound cache rejected: %v", err)
	}
	wrong := generationReferenceDocument(t, `{"model":"models/other-model","usageMetadata":{"totalTokenCount":100},"expireTime":"2026-10-24T12:00:00Z"}`)
	if err := resources.EnrichRetained(r, e, wrong, time.Now()); err == nil {
		t.Fatal("cache accepted a different serving model")
	}
}

func TestRetainedDiscoveredChildHasIndependentLifecycleMetadata(t *testing.T) {
	parent := &resources.Resource{UUID: uuid.New(), Kind: "vector_store", Metadata: json.RawMessage(`{"upstream_model":"m","created_at":"2026-10-23T10:00:00Z","terminal_observed_at":"2026-10-23T11:00:00Z","usage_bytes":1000}`)}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "vector_store_file", Operation: "vector_store"}, Action: "list"}
	item := generationReferenceDocument(t, `{"id":"file-native","created_at":1792749900,"status":"completed"}`)
	child, err := retainedDiscoveredChild(parent, e.Kind, "file-native", item.Root(), e, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if resources.EnrichRetained(child, e, item, time.Now()) != nil {
		t.Fatal("child inherited an incompatible parent timestamp")
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(child.Metadata, &metadata) != nil || metadata["upstream_model"] == nil || metadata["terminal_observed_at"] != nil || metadata["usage_bytes"] != nil || bytes.Contains(child.Metadata, []byte(`2026-10-23T10:00:00Z`)) {
		t.Fatalf("child inherited parent lifecycle facts: %s", child.Metadata)
	}
}

func TestRetainedEvalModelAuthority(t *testing.T) {
	p := &pin{provider: routing.Provider{ID: "provider", Kind: "openai", Capabilities: []routing.Capability{{Model: "sample", Operation: "eval", Surface: "openai", Mode: "unary"}, {Model: "grader", Operation: "eval", Surface: "openai", Mode: "unary"}, {Model: "sample", Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: "grader", Operation: "generation", Surface: "openai", Mode: "unary"}}}, slot: routing.Slot{Enabled: true}, model: "sample"}
	route := &routing.Route{Slug: "eval-route", Operations: []string{"eval", "generation"}, Targets: []routing.Target{{ProviderID: "provider", ProviderModel: "sample"}, {ProviderID: "provider", ProviderModel: "grader"}}}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "eval", Kind: "eval"}, Action: "create"}
	document, _ := oif.ParseJSON([]byte(`{"data_source_config":{"type":"custom","item_schema":{"properties":{"model":{"type":"string"}}}},"testing_criteria":[{"type":"score_model","model":"grader"}]}`), oif.Limits{})
	metadata, err := retainedInputMetadata(p, route, "key", e, document, nil)
	if err != nil || strings.Join(metadata["authorized_models"].([]string), ",") != "grader" {
		t.Fatalf("grader pins=%v %v", metadata, err)
	}
	p.slot.AllowedModels = []string{"sample"}
	if _, err := retainedInputMetadata(p, route, "key", e, document, nil); err == nil {
		t.Fatal("grader bypassed slot model grants")
	}
	p.slot.AllowedModels = nil
	capabilities := p.provider.Capabilities
	p.provider.Capabilities = capabilities[:3]
	if _, err := retainedInputMetadata(p, route, "key", e, document, nil); err == nil {
		t.Fatal("uncertified generation grader admitted by eval capability alone")
	}
	p.provider.Capabilities = capabilities
	foreign, _ := oif.ParseJSON([]byte(strings.Replace(document.Raw(), `"grader"`, `"foreign"`, 1)), oif.Limits{})
	if _, err := retainedInputMetadata(p, route, "key", e, foreign, nil); err == nil {
		t.Fatal("unapproved grader admitted")
	}
	parent := &resources.Resource{Kind: "eval", Metadata: json.RawMessage(`{"authorized_models":["grader"]}`)}
	e.Kind, e.Action = "eval_run", "create"
	run, _ := oif.ParseJSON([]byte(`{"data_source":{"type":"responses","model":"sample","source":{"type":"file_content","content":[]}}}`), oif.Limits{})
	metadata, err = retainedInputMetadata(p, route, "key", e, run, parent)
	if err != nil || strings.Join(metadata["authorized_models"].([]string), ",") != "grader,sample" {
		t.Fatalf("combined run/grader pins=%v %v", metadata, err)
	}
	claimedCriteria, err := oif.Apply(run, []oif.Change{{Pointer: "/testing_criteria", Value: `[]`, Origin: oif.ResourceBinding, Reason: "caller attempts to replace inherited graders"}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = retainedInputMetadata(p, route, "key", e, claimedCriteria, parent)
	if err != nil || strings.Join(metadata["authorized_models"].([]string), ",") != "grader,sample" {
		t.Fatalf("run request dropped inherited grader authority: %v %v", metadata, err)
	}
	legacy := &resources.Resource{Kind: "eval", Metadata: json.RawMessage(`{"upstream_model":"sample"}`)}
	if _, err := retainedInputMetadata(p, route, "key", e, run, legacy); err == nil {
		t.Fatal("legacy grader definitions acquired empty authority")
	}
	parent.Metadata = json.RawMessage(`{"authorized_models":[]}`)
	if _, err := retainedInputMetadata(p, route, "key", e, run, parent); err != nil {
		t.Fatalf("qualified definition without model graders refused: %v", err)
	}
}

func TestRetainedEvalNativeBindingsPreserveLogicalAuthority(t *testing.T) {
	p := &pin{model: "sample", provider: routing.Provider{ID: "provider", Kind: "openai", Bindings: map[string]connectors.Binding{"sample": {Model: "native-sample"}, "grader": {Model: "native-grader"}}, Capabilities: []routing.Capability{
		{Model: "sample", Operation: "eval", Surface: "openai", Mode: "unary"}, {Model: "grader", Operation: "eval", Surface: "openai", Mode: "unary"},
		{Model: "sample", Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: "grader", Operation: "generation", Surface: "openai", Mode: "unary"},
	}}, slot: routing.Slot{Enabled: true}}
	route := &routing.Route{Slug: "eval-route", Operations: []string{"eval", "generation"}, Targets: []routing.Target{{ProviderID: "provider", ProviderModel: "sample"}, {ProviderID: "provider", ProviderModel: "grader"}}}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "eval", Kind: "eval"}, Action: "create"}
	definition := generationReferenceDocument(t, `{"metadata":{"model":"opaque"},"testing_criteria":[{"type":"multi","graders":{"metadata":{"type":"score_model","model":"grader"},"model":{"type":"score_model","model":"grader"},"a/b~c":{"type":"score_model","model":"grader"}}}]}`)
	metadata, err := retainedInputMetadata(p, route, "owner", e, definition, nil)
	if err != nil || strings.Join(metadata["authorized_models"].([]string), ",") != "grader" || metadata["serving_models"].(map[string]string)["grader"] != "native-grader" {
		t.Fatalf("logical grader pins: %v %v", metadata, err)
	}
	native, err := bindRetainedModel(definition, e, p)
	if err != nil || strings.Count(native.Raw(), `"model":"native-grader"`) != 3 || !strings.Contains(native.Raw(), `"model":"opaque"`) {
		t.Fatalf("native grader bindings or opaque metadata changed: %s %v", native.Raw(), err)
	}
	parentMetadata, _ := json.Marshal(metadata)
	parent := &resources.Resource{Kind: "eval", Metadata: parentMetadata}
	e.Kind = "eval_run"
	run := generationReferenceDocument(t, `{"data_source":{"type":"responses","model":"sample","source":{"type":"file_content","content":[{"item":{"model":"opaque"}}]}}}`)
	metadata, err = retainedInputMetadata(p, route, "owner", e, run, parent)
	if err != nil || strings.Join(metadata["authorized_models"].([]string), ",") != "grader,sample" {
		t.Fatalf("logical run pins: %v %v", metadata, err)
	}
	native, err = bindRetainedModel(run, e, p)
	if err != nil || !strings.Contains(native.Raw(), `"model":"native-sample"`) || !strings.Contains(native.Raw(), `"model":"opaque"`) {
		t.Fatalf("native run binding: %s %v", native.Raw(), err)
	}
	legacy := &resources.Resource{Kind: "eval", Metadata: json.RawMessage(`{"authorized_models":["grader"]}`)}
	if _, err := retainedInputMetadata(p, route, "owner", e, run, legacy); err == nil {
		t.Fatal("unbound legacy grader alias inherited authority")
	}
	p.provider.Bindings["sample"] = connectors.Binding{Model: "native-grader"}
	if _, err := retainedInputMetadata(p, route, "owner", e, run, parent); err == nil {
		t.Fatal("ambiguous native usage alias mapping accepted")
	}
}

func TestRetainedEvalSelectionFindsSlotForEveryDeclaredModel(t *testing.T) {
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	base := route.Targets[0]
	provider := snapshot.Providers[base.ProviderID]
	provider.Kind = "openai"
	provider.Capabilities = []routing.Capability{
		{Model: base.ProviderModel, Operation: "eval", Surface: "openai", Mode: "unary"}, {Model: "grader", Operation: "eval", Surface: "openai", Mode: "unary"},
		{Model: base.ProviderModel, Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: "grader", Operation: "generation", Surface: "openai", Mode: "unary"},
	}
	provider.Slots[0].AllowedModels = []string{base.ProviderModel}
	complete := provider.Slots[0]
	complete.ID = uuid.NewString()
	complete.AllowedModels = []string{base.ProviderModel, "grader"}
	provider.Slots = append(provider.Slots, complete)
	snapshot.Providers[provider.ID] = provider
	grader := base
	grader.ID, grader.ProviderModel = uuid.NewString(), "grader"
	route.Targets = []routing.Target{base, grader}
	route.Operations = []string{"eval", "generation"}
	snapshot.Routes[routeSlug] = route
	x := &call{keyID: h.keyID, family: wire.File, request: request{release: h.rt.release, startedAt: time.Now()}}
	x.route = &route
	source := generationReferenceDocument(t, `{"testing_criteria":[{"type":"score_model","model":"grader"}]}`)
	qualifySlot, err := retainedEvalSlots(&route, h.keyID, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, failure := h.gateway.selectBoundPinSurface(t.Context(), x, &route, "eval", "openai", "unary", nil, func(_ *routing.Provider, model string) bool { return model == base.ProviderModel }, qualifySlot)
	if failure != nil {
		t.Fatal(failure)
	}
	defer h.gateway.resourceSettle(t.Context(), x, p)
	if p.slot.ID != complete.ID {
		t.Fatalf("selected slot %s without grader authority, want %s", p.slot.ID, complete.ID)
	}
}

func TestRetainedCorrelationCannotBeSuppliedOrReplaced(t *testing.T) {
	e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "vector_store", Operation: "vector_store"}, Action: "create"}
	r := &resources.Resource{UUID: uuid.New()}
	for _, action := range []string{"create", "update"} {
		e.Action = action
		for _, field := range []string{"olp_submission_id", "olp_submission_digest"} {
			document, _ := oif.ParseJSON([]byte(`{"metadata":{"`+field+`":"forged"}}`), oif.Limits{})
			if _, err := retainedInputMetadata(&pin{}, nil, "key", e, document, nil); err == nil {
				t.Fatal("caller supplied recovery authority")
			}
		}
		body, err := retainedCorrelationBody(e, r, []byte(`{"metadata":{"user":"value"}}`))
		if err != nil || !strings.Contains(string(body), r.UUID.String()) || !strings.Contains(string(body), `"user":"value"`) {
			t.Fatalf("correlation=%s %v", body, err)
		}
	}
}

func TestRetainedGeminiFileReferences(t *testing.T) {
	id := resources.LocalID(resources.KindGeminiFile, uuid.New())
	for _, raw := range []string{id, "files/" + id, "https://gateway.test/gemini/v1beta/files/" + id} {
		if got, err := retainedGeminiFileID(raw); err != nil || got != id {
			t.Fatalf("file binding=%q %v", got, err)
		}
	}
	for _, raw := range []string{"files/native", "https://generativelanguage.googleapis.com/v1beta/files/native", "https://gateway.test/gemini/v1beta/files/" + id + "?key=secret"} {
		if _, err := retainedGeminiFileID(raw); err == nil {
			t.Fatal("raw provider URI bypassed file authority")
		}
	}
	r := &resources.Resource{UpstreamID: "native", Metadata: json.RawMessage(`{"name":"files/native","uri":"https://generativelanguage.googleapis.com/v1beta/files/native"}`)}
	if uri, err := retainedGeminiFileURI(r, &pin{provider: routing.Provider{Endpoint: "https://generativelanguage.googleapis.com/v1beta"}}); err != nil || !strings.HasSuffix(uri, "files/native") {
		t.Fatalf("native URI=%q %v", uri, err)
	}
}

func TestRetainedNativeMethodRefusals(t *testing.T) {
	e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "fine_tuning_job", Operation: "fine_tuning"}, Action: "create"}
	document, _ := oif.ParseJSON([]byte(`{"method":{"type":"reinforcement","reinforcement":{"grader":{"model":"unapproved"}}}}`), oif.Limits{})
	if _, err := retainedInputMetadata(&pin{}, nil, "key", e, document, nil); err == nil {
		t.Fatal("unpriced reinforcement grader admitted")
	}
	e.Kind, e.Operation, e.Action = "context_cache", "context_cache", "patch"
	document, _ = oif.ParseJSON([]byte(`{"model":"other","ttl":"60s"}`), oif.Limits{})
	if _, err := retainedInputMetadata(&pin{}, nil, "key", e, document, nil); err == nil {
		t.Fatal("cache model mutation admitted")
	}
}

func TestRetainedVertexUsesNativeLabelsForCorrelation(t *testing.T) {
	r := &resources.Resource{UUID: uuid.New()}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: "vertex_tuning_job", Operation: "fine_tuning"}, Action: "create"}
	body, err := retainedCorrelationBody(e, r, []byte(`{"baseModel":"m","labels":{"tenant_id":"customer"}}`))
	if err != nil {
		t.Fatal(err)
	}
	document, _ := oif.ParseJSON(body, oif.Limits{})
	labels, _ := document.Root().Lookup("labels")
	marker, _ := labels.Lookup("olp_submission_id")
	id, _ := marker.Text()
	if id != r.UUID.String() {
		t.Fatalf("missing native label: %s", body)
	}
	if _, err = retainedapi.Rewrite(document, nil, func(string, string) (string, error) {
		t.Fatal("literal labels became resource authority")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
}
