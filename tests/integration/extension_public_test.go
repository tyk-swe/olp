//go:build integration && extension

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/tests/fidelity"
)

// The build tag isolates test-only trusted registrations from the ordinary
// fixed-inventory integration process. No external config installs a codec.
type extensionView struct {
	schema oif.Identity
	source oif.Document
}

func (v extensionView) Schema() oif.Identity { return v.schema }
func (v extensionView) Source() oif.Document { return v.source }

type extensionFixture struct {
	*httptest.Server
	mu       sync.Mutex
	path     string
	response []byte
	calls    [][]byte
}

func newExtensionFixture(t *testing.T, path, response string) *extensionFixture {
	t.Helper()
	f := &extensionFixture{path: path, response: []byte(response)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			http.Error(w, "fixture address or credential changed", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, body)
		response := bytes.Clone(f.response)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *extensionFixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}
func (f *extensionFixture) result(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.response = []byte(body)
}
func (f *extensionFixture) captured() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.calls))
	for i, call := range f.calls {
		out[i] = bytes.Clone(call)
	}
	return out
}

func registerExtensionProfile(t *testing.T, dialect, operation string) string {
	t.Helper()
	id := "fixture-" + dialect + "-" + uuid.NewString()[:8]
	profile := connectors.Profile{
		ID: id, Revision: "1", Label: "Fixture provider for " + dialect,
		Kind: "openai_compatible", Dialect: dialect, DialectRevision: "1",
		Hosting: "direct-compatible", Authentication: []string{"api_key"}, Transport: "http",
		Operations: []string{operation}, OperationDialects: map[string]string{operation: dialect},
		Documentation: "fixture-extension/1",
	}
	if err := connectors.RegisterOperationProfile(profile); err != nil {
		t.Fatal(err)
	}
	return id
}

func publishExtension(t *testing.T, h *accessHarness, owner *browser, f *extensionFixture, profileID, operation string) (string, string) {
	t.Helper()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          "Extension " + uuid.NewString(),
		"configuration": map[string]any{"kind": "openai_compatible", "profile_id": profileID, "profile_revision": "1", "auth_mode": "api_key", "endpoint": f.URL + "/v1"},
		"model":         vendorModel, "credential": vendorSecret,
	}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + provider["id"].(string)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": operation, "surface": "native", "mode": "unary"}}}, etagHeader(provider), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(provider), 200)
	if certified["status"] != "certified" {
		t.Fatalf("registered operation was not certified: %v", certified)
	}
	provider = h.want(owner, "GET", path, nil, nil, 200)
	f.reset()
	h.want(owner, "POST", path+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "extension-" + uuid.NewString()[:8]
	draft := fidelityDraft(slug, provider["id"])
	draft["operations"] = []string{operation}
	draft["fidelity"] = map[string]any{"mode": "strict"}
	working := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+working["id"].(string)+"/activate", nil, withMatch(working, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "fixture extension", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func TestRegisteredExtensionsPublic(t *testing.T) {
	t.Run("provider reuses existing dialect", func(t *testing.T) {
		profileID := registerExtensionProfile(t, "tei-embeddings", "embeddings")
		f := newExtensionFixture(t, "/v1/embed", `[[1,-2]]`)
		h := newAccessHarness(t)
		owner := h.owner()
		slug, key := publishExtension(t, h, owner, f, profileID, "embeddings")
		body := []byte(`{"inputs":["native text"],"normalize":false,"prompt_name":null}`)
		sink := make(strictOutcomeSink, 2)
		h.Gateway.Sink = sink
		status, result, _ := h.gatewayRaw("POST", "/native/tei-embeddings/models/"+slug, key, bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != 200 || string(result) != `[[1,-2]]` {
			t.Fatalf("reused dialect did not serve exact native result: %d %s", status, result)
		}
		calls := f.captured()
		if len(calls) != 1 || fidelity.Compare(body, calls[0]) != nil {
			t.Fatal("fixture provider changed the existing dialect request")
		}
		event := <-sink
		if event.Operation != "embeddings" || len(event.Attempts) != 1 || event.Attempts[0].Interaction == nil || event.Usage != nil {
			t.Fatal("fixture provider bypassed shared attempt/accounting", event.Operation)
		}
	})
	t.Run("new dialect and operation remain independent of generation", func(t *testing.T) {
		for _, example := range []struct {
			operation, field, resultField, path, source, native string
		}{
			{
				"embeddings", "texts", "vectors", "vectors",
				`{"model":"ROUTE","payload":{"texts":["native text"],"task":"query"},"future":{"count":9007199254740993}}`,
				`{"model":"fixture-model","vectors":[{"index":0,"values":[0.25,-0]}],"native":{"revision":9007199254740993}}`,
			},
			{
				"fixture_signal", "items", "scores", "signal",
				`{"model":"ROUTE","payload":{"items":[{"id":"doc-a","text":"native text"}]},"future":{"threshold":-0}}`,
				`{"model":"fixture-model","scores":[{"id":"doc-a","value":-0}],"native":{"settlement":"per-set"}}`,
			},
		} {
			t.Run(example.operation, func(t *testing.T) {
				dialect := registerFixtureDialect(t, example.operation, example.field, example.resultField, example.path)
				if err := connectors.RegisterOperationProfile(connectors.Profile{
					ID: "untrusted-host-" + uuid.NewString()[:8], Revision: "1", Label: "Invalid fixture host",
					Kind: "azure_openai", Hosting: "azure-deployment", Authentication: []string{"api_key"}, Transport: "http",
					Operations: []string{example.operation}, OperationDialects: map[string]string{example.operation: dialect},
				}); err == nil {
					t.Fatal("relative native path escaped its trusted direct-compatible hosting")
				}
				profileID := registerExtensionProfile(t, dialect, example.operation)
				f := newExtensionFixture(t, "/v1/"+example.path, example.native)
				h := newAccessHarness(t)
				owner := h.owner()
				unregistered := map[string]any{"name": "unregistered " + uuid.NewString(), "model": vendorModel,
					"configuration": map[string]any{"kind": "openai_compatible", "profile_id": "not-registered", "profile_revision": "1", "auth_mode": "api_key", "endpoint": f.URL + "/v1"}, "credential": vendorSecret}
				h.want(owner, "POST", "/api/v1/providers", unregistered, idem(uuid.NewString()), 422)
				if len(f.captured()) != 0 {
					t.Fatal("unregistered profile contacted a provider")
				}
				slug, key := publishExtension(t, h, owner, f, profileID, example.operation)
				source := strings.ReplaceAll(example.source, "ROUTE", slug)
				sink := make(strictOutcomeSink, 2)
				h.Gateway.Sink = sink
				status, result, _ := h.gatewayRaw("POST", "/native/"+dialect+"/models/"+slug, key, strings.NewReader(source), map[string]string{"Content-Type": "application/json"})
				expected := strings.ReplaceAll(example.native, `"model":"fixture-model"`, `"model":"`+slug+`"`)
				if status != 200 || fidelity.Compare([]byte(expected), result) != nil {
					t.Fatalf("fixture operation result changed: %d %s", status, result)
				}
				calls := f.captured()
				if len(calls) != 1 || fidelity.Compare([]byte(strings.ReplaceAll(source, slug, vendorModel)), calls[0]) != nil {
					t.Fatal("fixture dialect altered provider-bound native source")
				}
				event := <-sink
				if event.Operation != example.operation || len(event.Attempts) != 1 || event.Attempts[0].Interaction == nil || event.Usage != nil {
					t.Fatal("new operation bypassed shared attempt/accounting")
				}
				before := len(calls)
				status, rejected, _ := h.gatewayRaw("POST", "/native/unregistered-fixture/models/"+slug, key, strings.NewReader(source), map[string]string{"Content-Type": "application/json"})
				if status != 400 || !bytes.Contains(rejected, []byte(`"code":"target_capability"`)) || len(f.captured()) != before {
					t.Fatal("unregistered dialect reached provider")
				}
				f.result(`{"model":"fixture-model","` + example.resultField + `":[{"broken":true}]}`)
				status, result, _ = h.gatewayRaw("POST", "/native/"+dialect+"/models/"+slug, key, strings.NewReader(source), map[string]string{"Content-Type": "application/json"})
				if status != 502 || !bytes.Contains(result, []byte(`"code":"fidelity_protocol_violation"`)) || len(f.captured()) != before+1 {
					t.Fatalf("corrupt registered result was accepted: %d %s", status, result)
				}
			})
		}
	})
}

func registerFixtureDialect(t *testing.T, operation, field, resultField, relativePath string) string {
	t.Helper()
	id := "fixture-" + relativePath + "-" + uuid.NewString()[:8]
	operationID := oif.Identity{ID: operation, Revision: "1"}
	d := operations.Dialect{
		Identity: oif.Identity{ID: id, Revision: "1"}, Operation: operationID,
		Surface: "native", Label: "Fixture " + relativePath,
		Address:       operations.Address{RelativePath: relativePath},
		RequestSchema: operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}, "payload": map[string]any{"type": "object"}}, "model", "payload"),
		ResultSchema:  operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}, resultField: map[string]any{"type": "array"}}, "model", resultField),
		BindModel:     operations.ModelChanges, BindResultModel: operations.ModelChanges,
		Estimate: func(oif.View) int64 { return 1 }, Evidence: "fixture-registered-operation/1",
		Documentation: "fixture-registered-operation/1",
	}
	d.Request = func(source oif.Request) (oif.View, error) {
		root := source.Document().Root()
		model := operations.Member(root, "model")
		items := operations.Member(operations.Member(root, "payload"), field)
		if model.Kind() != oif.String || items.Kind() != oif.Array || len(items.Elements()) != 1 {
			return nil, operations.Invalid("payload", "The fixture native request needs one model and input item.")
		}
		if field == "texts" && items.Elements()[0].Kind() != oif.String || field == "items" && operations.Member(items.Elements()[0], "id").Kind() != oif.String {
			return nil, operations.Invalid("payload", "The fixture input item has the wrong native shape.")
		}
		return extensionView{schema: operationID, source: source.Document()}, nil
	}
	d.Result = func(request oif.Request, result oif.Result) (oif.View, error) {
		root := result.Source().Root()
		items := operations.Member(root, resultField)
		if operations.Member(root, "model").Kind() != oif.String || items.Kind() != oif.Array || len(items.Elements()) != 1 {
			return nil, operations.Violation("/result", "fixture result shape")
		}
		entry := items.Elements()[0]
		if resultField == "vectors" {
			if index, ok := operations.Int(operations.Member(entry, "index")); !ok || index != 0 || operations.Member(entry, "values").Kind() != oif.Array {
				return nil, operations.Violation("/vectors", "vector identity and coordinates")
			}
			for _, value := range operations.Member(entry, "values").Elements() {
				if !operations.Number(value) {
					return nil, operations.Violation("/vectors", "native numeric coordinates")
				}
			}
		} else {
			input := operations.Member(operations.Member(request.Document().Root(), "payload"), "items").Elements()[0]
			if operations.String(operations.Member(entry, "id")) != operations.String(operations.Member(input, "id")) || !operations.Number(operations.Member(entry, "value")) {
				return nil, operations.Violation("/scores", "native input identity and score")
			}
		}
		return extensionView{schema: operationID, source: result.Source()}, nil
	}
	d.Probe = func(model string) []byte {
		payload := map[string]any{field: []any{"certify"}}
		if field == "items" {
			payload[field] = []any{map[string]string{"id": "doc-a", "text": "certify"}}
		}
		body, _ := json.Marshal(map[string]any{"model": model, "payload": payload})
		return body
	}
	if err := operationregistry.Default.Register(d); err != nil {
		t.Fatal(err)
	}
	return id
}
