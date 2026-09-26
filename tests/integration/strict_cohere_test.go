//go:build integration

package integration_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
)

const cohereTinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// Cohere's native v2 endpoint is distinct from its existing /compatibility/v1
// preset. Provision through the same public management seam as other unary
// operations, with the native v2 base path and a separately registered profile.
func publishCohereOperation(t *testing.T, h *accessHarness, owner *browser, f *operationFixture, profileID, operation, probe string) (string, string) {
	t.Helper()
	profile, err := connectors.LookupProfile(profileID, "1")
	if err != nil {
		t.Fatal(err)
	}
	configuration := map[string]any{"kind": profile.Kind, "profile_id": profile.ID, "profile_revision": "1", "endpoint": f.URL + "/v2", "auth_mode": "api_key", "options": map[string]any{"vendor_id": "cohere-native-v2"}}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Cohere native " + uuid.NewString(), "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + provider["id"].(string)
	model := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "PATCH", path+"/models/"+model, map[string]any{"enabled": true, "capabilities": operationCapabilities(profileID, operation, "native")}, etagHeader(provider), 200)
	f.mu.Lock()
	original := f.response
	f.mu.Unlock()
	f.result(probe)
	certified := h.want(owner, "POST", path+"/models/"+model+"/certify", nil, etagHeader(provider), 200)
	if certified["status"] == "failed" {
		t.Fatalf("Cohere native certification: %v", certified)
	}
	provider = h.want(owner, "GET", path, nil, nil, 200)
	f.mu.Lock()
	f.response, f.calls = original, nil
	f.mu.Unlock()
	h.want(owner, "POST", path+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "cohere-" + uuid.NewString()
	draftInput := fidelityDraft(slug, provider["id"])
	draftInput["operations"] = []string{operation}
	draftInput["fidelity"] = map[string]any{"mode": "strict"}
	if f.policy != nil {
		draftInput["content_policy"] = f.policy
	}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Cohere native", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func TestStrictCohereNativeEmbedV2PreservesTypedStorageAndBilling(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	response := `{"id":"embed-native","embeddings":{"float":[[0.10000000000000001,-0,1,2,3,4,5,6],[1e-4,2,3,4,5,6,7,8]],"int8":[[-128,127,0,1,2,3,4,5],[0,1,2,3,4,5,6,7]],"ubinary":[[128],[255]]},"texts":["first","second"],"meta":{"api_version":{"version":"2"},"billed_units":{"input_tokens":7}},"opaque":{"counter":9007199254740993}}`
	f := newOperationFixture(t, "/v2/embed", response)
	probe := `{"id":"embed-probe","embeddings":{"float":[[1,-2]]},"texts":["embedding probe"],"meta":{"billed_units":{"input_tokens":2}}}`
	slug, key := publishCohereOperation(t, h, owner, f, "cohere-embed-v2", "embeddings", probe)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	request := fmt.Sprintf(`{"model":%q,"input_type":"search_document","texts":["first","second"],"embedding_types":["float","int8","ubinary"],"truncate":"NONE"}`, slug)
	path := "/native/cohere-embed-v2/models/" + slug
	status, raw, _ := h.gatewayRaw("POST", path, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 400 || len(f.snapshot()) != 0 || !bytes.Contains(raw, []byte(`"code":"state_carrier"`)) {
		t.Fatalf("typed native storage without raw client dispatched: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw("POST", path, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json", "X-OLP-Client-Contract": "raw-vector-storage/1"})
	if status != 200 || string(raw) != response {
		t.Fatalf("Cohere native embeddings changed: %d %s", status, raw)
	}
	calls := f.snapshot()
	if len(calls) != 1 || string(calls[0].body) != strings.ReplaceAll(request, slug, vendorModel) {
		t.Fatalf("Cohere native embed request changed: %+v", calls)
	}
	fact := sink.last().Attempts[0]
	if fact.Usage == nil || fact.Usage.InputTokens != 7 {
		t.Fatalf("native Cohere embed billed tokens lost: %+v", fact.Usage)
	}
	encodedResult := `{"id":"embed-base64","embeddings":{"base64":["AACAPwAAAMA="]},"meta":{"billed_units":{"input_tokens":2}}}`
	f.result(encodedResult)
	encodedRequest := fmt.Sprintf(`{"model":%q,"input_type":"search_query","texts":["first"],"embedding_types":["base64"]}`, slug)
	status, raw, _ = h.gatewayRaw("POST", path, key, strings.NewReader(encodedRequest), map[string]string{"Content-Type": "application/json", "X-OLP-Client-Contract": "raw-vector-storage/1"})
	if status != 200 || string(raw) != encodedResult || string(f.snapshot()[1].body) != strings.ReplaceAll(encodedRequest, slug, vendorModel) {
		t.Fatalf("Cohere native base64 source/storage changed: %d %s", status, raw)
	}
	for _, bad := range []string{
		`{"embeddings":{"float":[[1,2,3,4,5,6,7,8],[1,2,3,4,5,6,7,8]],"int8":[[128,0,0,0,0,0,0,0],[0,1,2,3,4,5,6,7]],"ubinary":[[1],[2]]},"texts":["first","second"]}`,
		`{"embeddings":{"float":[[1,2,3,4,5,6,7,8],[1,2,3,4,5,6,7,8]],"int8":[[1,2,3,4,5,6,7,8],[0,1,2,3,4,5,6,7]],"ubinary":[[-1],[2]]},"texts":["first","second"]}`,
		`{"embeddings":{"float":[[1,2,3,4,5,6,7,8],[1,2,3,4,5,6,7,8]],"int8":[[1,2,3,4,5,6,7,8],[0,1,2,3,4,5,6,7]],"ubinary":[[1],[2]]},"texts":["second","first"]}`,
		`{"embeddings":{"float":[[1,2],[3,4]],"int8":[[1,2],[3,4]],"ubinary":[[1],[2]]},"texts":["first","second"]}`,
	} {
		f.result(bad)
		before := len(f.snapshot())
		status, raw, _ := h.gatewayRaw("POST", path, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json", "X-OLP-Client-Contract": "raw-vector-storage/1"})
		if status != 502 || len(f.snapshot()) != before+1 || !bytes.Contains(raw, []byte(`"code":"fidelity_protocol_violation"`)) {
			t.Fatalf("invalid native dtype/correspondence became success: %d %s", status, raw)
		}
	}
}

func TestStrictCohereNativeRerankPreservesResultsAndBillsOnce(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	response := `{"id":"rank-native","results":[{"index":1,"relevance_score":0.8125000000000001},{"index":0,"relevance_score":0.8125000000000001}],"meta":{"api_version":{"version":"2"},"billed_units":{"search_units":1}},"opaque":{"counter":9007199254740993}}`
	f := newOperationFixture(t, "/v2/rerank", response)
	probe := `{"id":"rank-probe","results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}],"meta":{"billed_units":{"search_units":1}}}`
	slug, key := publishCohereOperation(t, h, owner, f, "cohere-rerank-v2", "rerank", probe)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	request := fmt.Sprintf(`{"model":%q,"query":"preserve query","documents":["first","second"],"top_n":2,"max_tokens_per_doc":128,"priority":7,"native_extension":{"nested":[{"counter":9007199254740993,"zero":-0}]}}`, slug)
	status, raw, _ := h.gatewayRaw("POST", "/native/cohere-rerank-v2/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 200 || string(raw) != response {
		t.Fatalf("Cohere native result changed: %d %s", status, raw)
	}
	calls := f.snapshot()
	if len(calls) != 1 || string(calls[0].body) != strings.ReplaceAll(request, slug, vendorModel) || calls[0].headers.Get("Authorization") != "Bearer "+vendorSecret {
		t.Fatalf("Cohere native request/authorization changed: %+v", calls)
	}
	fact := sink.last().Attempts[0]
	if fact.Usage == nil || fact.Usage.MediaUnits == nil || *fact.Usage.MediaUnits != "1" {
		t.Fatalf("native Cohere search-unit billing lost: %+v", fact.Usage)
	}
	nullUsage := `{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}],"meta":{"billed_units":{"search_units":null}}}`
	f.result(nullUsage)
	status, raw, _ = h.gatewayRaw("POST", "/native/cohere-rerank-v2/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 200 || string(raw) != nullUsage || sink.last().Attempts[0].Usage != nil {
		t.Fatalf("nullable native search units fabricated usage or failed: %d %s", status, raw)
	}
	f.result(`{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}],"meta":{"billed_units":{"search_units":-1}}}`)
	status, raw, _ = h.gatewayRaw("POST", "/native/cohere-rerank-v2/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 502 || !bytes.Contains(raw, []byte(`"code":"fidelity_protocol_violation"`)) {
		t.Fatalf("negative provider search units became successful usage: %d %s", status, raw)
	}
}

func TestStrictCohereNativeRejectsForeignControlsAndUninspectablePolicyFields(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newOperationFixture(t, "/v2/rerank", `{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}],"meta":{"billed_units":{"search_units":1}}}`)
	probe := `{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}],"meta":{"billed_units":{"search_units":1}}}`
	slug, key := publishCohereOperation(t, h, owner, f, "cohere-rerank-v2", "rerank", probe)
	for _, extra := range []string{`"top_k":1`, `"return_documents":true`, `"priority":1000`, `"max_tokens_per_doc":0`} {
		request := fmt.Sprintf(`{"model":%q,"query":"q","documents":["a","b"],%s}`, slug, extra)
		status, raw, _ := h.gatewayRaw("POST", "/native/cohere-rerank-v2/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
		if status != 400 || len(f.snapshot()) != 0 || !bytes.Contains(raw, []byte(`"code":"unsupported_parameter"`)) {
			t.Fatalf("foreign Cohere control dispatched: %d %s", status, raw)
		}
	}
	policy := newOperationFixture(t, "/v2/rerank", probe)
	policy.policy = fidelityPolicy("block", "input")
	slug, key = publishCohereOperation(t, h, owner, policy, "cohere-rerank-v2", "rerank", probe)
	request := fmt.Sprintf(`{"model":%q,"query":"q","documents":["a","b"],"native_extension":{"hidden":"private"}}`, slug)
	status, raw, _ := h.gatewayRaw("POST", "/native/cohere-rerank-v2/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 400 || len(policy.snapshot()) != 0 || !bytes.Contains(raw, []byte(`"code":"policy_conflict"`)) {
		t.Fatalf("uninspectable native extension bypassed input policy: %d %s", status, raw)
	}
	embedPolicy := newOperationFixture(t, "/v2/embed", `{"embeddings":{"float":[[1,-2]]},"meta":{"billed_units":{"input_tokens":2}}}`)
	embedPolicy.policy = fidelityPolicy("block", "input")
	slug, key = publishCohereOperation(t, h, owner, embedPolicy, "cohere-embed-v2", "embeddings", `{"embeddings":{"float":[[1,-2]]},"meta":{"billed_units":{"input_tokens":2}}}`)
	for _, input := range []string{
		`{"content":[{"type":"text","text":"safe"}],"native_extension":{"hidden":"private"}}`,
		`{"content":[{"type":"text","text":"safe","native_extension":{"hidden":"private"}}]}`,
	} {
		request := fmt.Sprintf(`{"model":%q,"input_type":"search_document","inputs":[%s],"embedding_types":["float"]}`, slug, input)
		status, raw, _ := h.gatewayRaw("POST", "/native/cohere-embed-v2/models/"+slug, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
		if status != 400 || len(embedPolicy.snapshot()) != 0 || !bytes.Contains(raw, []byte(`"code":"policy_conflict"`)) {
			t.Fatalf("nested native Cohere text bypassed restrictive policy: %d %s", status, raw)
		}
	}
}

func TestStrictCohereNativeEmbedV2RetainsMultimodalInputAndRejectsCorruption(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(cohereTinyPNG, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(pngBytes)); err != nil {
		t.Fatalf("the independently supplied image fixture is not a valid PNG: %v", err)
	}
	h := newAccessHarness(t)
	owner := h.owner()
	response := `{"id":"embed-image","embeddings":{"float":[[0.25,-0.5]]},"texts":[],"images":null,"meta":{"billed_units":{"input_tokens":1,"images":1},"tokens":null},"native":{"precision":9007199254740993}}`
	f := newOperationFixture(t, "/v2/embed", response)
	probe := `{"embeddings":{"float":[[1,-2]]},"meta":{"billed_units":{"input_tokens":2}}}`
	slug, key := publishCohereOperation(t, h, owner, f, "cohere-embed-v2", "embeddings", probe)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	path := "/native/cohere-embed-v2/models/" + slug
	request := fmt.Sprintf(`{"model":%q,"input_type":"search_document","inputs":[{"content":[{"type":"text","text":"diagram"},{"type":"image_url","image_url":{"url":%q}}]}],"embedding_types":["float"],"native_extension":{"zero":-0}}`, slug, cohereTinyPNG)
	status, raw, _ := h.gatewayRaw("POST", path, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 200 || string(raw) != response {
		t.Fatalf("Cohere multimodal source/result changed: %d %s", status, raw)
	}
	if calls := f.snapshot(); len(calls) != 1 || string(calls[0].body) != strings.ReplaceAll(request, slug, vendorModel) {
		t.Fatalf("original Cohere multimodal bytes changed: %+v", calls)
	}
	if used := sink.last().Attempts[0].Usage; used == nil || used.InputTokens != 1 || used.MediaUnits == nil || *used.MediaUnits != "1" {
		t.Fatalf("Cohere native text/image billing changed: %+v", used)
	}
	nestedExtension := strings.Replace(request, `"text":"diagram"`, `"text":"diagram","native_extension":{"hidden":"private"}`, 1)
	status, raw, _ = h.gatewayRaw("POST", path, key, strings.NewReader(nestedExtension), map[string]string{"Content-Type": "application/json"})
	if status != 200 || string(raw) != response || len(f.snapshot()) != 2 || string(f.snapshot()[1].body) != strings.ReplaceAll(nestedExtension, slug, vendorModel) {
		t.Fatalf("unrestricted native Cohere extension was not preserved: %d %s", status, raw)
	}
	imageRequest := fmt.Sprintf(`{"model":%q,"input_type":"image","images":[%q],"embedding_types":["float"]}`, slug, cohereTinyPNG)
	status, raw, _ = h.gatewayRaw("POST", path, key, strings.NewReader(imageRequest), map[string]string{"Content-Type": "application/json"})
	if status != 200 || string(raw) != response || len(f.snapshot()) != 3 || string(f.snapshot()[2].body) != strings.ReplaceAll(imageRequest, slug, vendorModel) {
		t.Fatalf("original Cohere image collection changed: %d %s", status, raw)
	}
	for _, body := range []string{
		fmt.Sprintf(`{"model":%q,"input_type":"image","images":["https://example.invalid/image.png"]}`, slug),
		fmt.Sprintf(`{"model":%q,"input_type":"search_document","texts":["a"],"images":[%q]}`, slug, cohereTinyPNG),
		fmt.Sprintf(`{"model":%q,"input_type":"search_document","texts":["a"],"embedding_types":["sparse"]}`, slug),
	} {
		before := len(f.snapshot())
		status, raw, _ := h.gatewayRaw("POST", path, key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != 400 || len(f.snapshot()) != before || !bytes.Contains(raw, []byte(`"code":"unsupported_parameter"`)) {
			t.Fatalf("unqualified Cohere form dispatched: %d %s", status, raw)
		}
	}
	for _, bad := range []string{
		`{"embeddings":{}}`,
		`{"embeddings":{"float":[[0.1],[0.2]]}}`,
		`{"embeddings":{"float":["not-an-array"]}}`,
	} {
		f.result(bad)
		before := len(f.snapshot())
		status, raw, _ := h.gatewayRaw("POST", path, key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
		if status != 502 || len(f.snapshot()) != before+1 || !bytes.Contains(raw, []byte(`"code":"fidelity_protocol_violation"`)) {
			t.Fatalf("corrupt Cohere vector became success/retry: %d %s", status, raw)
		}
	}
}

func TestStrictCohereNativeV2CatalogueKeepsCompatibilityPresetSeparate(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	vendors := h.list(owner, "GET", "/api/v1/provider-vendors", nil, nil, 200)
	found := map[string]map[string]any{}
	for _, raw := range vendors {
		entry := raw.(map[string]any)
		if id, ok := entry["id"].(string); ok && (id == "cohere" || id == "cohere-native-v2") {
			found[id] = entry
		}
	}
	if found["cohere-native-v2"] == nil || found["cohere-native-v2"]["discovery"] != false || found["cohere-native-v2"]["endpoint"] != "https://api.cohere.ai/v2" ||
		found["cohere"] == nil || found["cohere"]["endpoint"] != "https://api.cohere.ai/compatibility/v1" {
		t.Fatalf("native v2 discovery/preset conflated with compatibility: %v", found)
	}
	status, invalid, _ := h.request(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Wrong Cohere endpoint", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "profile_id": "cohere-embed-v2", "profile_revision": "1", "auth_mode": "api_key", "endpoint": "https://api.cohere.ai/compatibility/v1", "options": map[string]any{"vendor_id": "cohere-native-v2"}},
	}, idem(uuid.NewString()))
	if status < 400 || !strings.Contains(fmt.Sprint(invalid), "/v2 endpoint") {
		t.Fatalf("public configuration accepted the old compatibility endpoint as native v2: %d %v", status, invalid)
	}
	profiles := h.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any)
	for _, id := range []string{"cohere-embed-v2", "cohere-rerank-v2"} {
		var profile map[string]any
		for _, raw := range profiles {
			entry := raw.(map[string]any)
			if entry["id"] == id {
				profile = entry
				break
			}
		}
		if profile == nil || profile["dialect_revision"] != "v2" || profile["operation_dialects"] == nil || profile["default_schemas"] == nil {
			t.Fatalf("native Cohere v2 public profile missing: %s %v", id, profile)
		}
	}
}
