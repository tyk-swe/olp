package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestAnthropicStateRoutesOverrideNativeFallback(t *testing.T) {
	server := &Server{Resources: resources.New(nil), Media: &MediaDeps{}}
	mux := http.NewServeMux()
	server.registerNative(mux)
	server.registerState(mux)
	for _, test := range []struct {
		method, path, pattern string
	}{
		{http.MethodPost, "/anthropic/v1/files", "POST /anthropic/v1/files"},
		{http.MethodGet, "/anthropic/v1/files/x", "GET /anthropic/v1/files/{id}"},
		{http.MethodPost, "/anthropic/v1/messages/batches", "POST /anthropic/v1/messages/batches"},
		{http.MethodGet, "/anthropic/v1/messages/batches/x/results", "GET /anthropic/v1/messages/batches/{id}/results"},
	} {
		_, pattern := mux.Handler(&http.Request{Method: test.method, URL: &url.URL{Path: test.path}})
		if pattern != test.pattern {
			t.Fatalf("%s %s resolved to %q, want %q", test.method, test.path, pattern, test.pattern)
		}
	}
}

func TestAnthropicStrictRouteRefusesBatchResources(t *testing.T) {
	h := newHarness(t, Config{})
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.Operations = append(route.Operations, "batch")
	route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
	h.rt.release.Snapshot.Routes[routeSlug] = route
	x := &call{request: request{release: h.rt.release}, family: wire.AnthropicBatch}
	authority := access.Authority{ID: h.keyID, Policy: access.KeyPolicy{Scopes: []string{"inference"}}}
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/batches", nil)
	req.Header.Set(stateRouteHeader, routeSlug)
	if _, err := h.gateway.anthropicRoute(x, authority, req, ""); err == nil || err.Type != "invalid_request_error" ||
		err.Code != "target_capability" || err.Status != http.StatusUnprocessableEntity {
		t.Fatalf("strict Anthropic batch refusal = %v", err)
	}
}

func TestValidateAnthropicBatchRequest(t *testing.T) {
	tests := []struct {
		name, body, route, target string
		wantErr                   bool
	}{
		{"valid", `{"requests":[{"custom_id":"a_1","params":{"model":"route","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}}]}`, "route", "upstream", false},
		{"nested file ref", `{"requests":[{"custom_id":"a_1","params":{"model":"route","max_tokens":16,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"anthropic_file_local"}}]}]}}]}`, "route", "upstream", false},
		{"unknown top-level", `{"requests":[],"metadata":{}}`, "route", "upstream", true},
		{"empty requests", `{"requests":[]}`, "route", "upstream", true},
		{"bad custom id", `{"requests":[{"custom_id":"a!","params":{"model":"route","max_tokens":1,"messages":[]}}]}`, "route", "upstream", true},
		{"long custom id", `{"requests":[{"custom_id":"` + strings.Repeat("a", 65) + `","params":{"model":"route","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}}]}`, "route", "upstream", true},
		{"extra request field", `{"requests":[{"custom_id":"a","params":{"model":"route","max_tokens":1,"messages":[]},"extra":true}]}`, "route", "upstream", true},
		{"duplicate custom id", `{"requests":[{"custom_id":"a","params":{"model":"route","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}},{"custom_id":"a","params":{"model":"route","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}}]}`, "route", "upstream", true},
		{"mixed models", `{"requests":[{"custom_id":"a","params":{"model":"route","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}},{"custom_id":"b","params":{"model":"other","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}}]}`, "route", "upstream", true},
		{"stream", `{"requests":[{"custom_id":"a","params":{"model":"route","stream":true,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}}]}`, "route", "upstream", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, _, _, err := validateAnthropicBatchRequest([]byte(test.body), test.route, test.target, test.target)
			if test.wantErr {
				if err == nil {
					t.Fatal("invalid Anthropic batch request was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Requests []struct {
					Params map[string]any `json:"params"`
				} `json:"requests"`
			}
			if json.Unmarshal(encoded, &request) != nil || request.Requests[0].Params["model"] != "upstream" {
				t.Fatalf("model was not rewritten: %s", encoded)
			}
		})
	}
	withoutRoute := `{"requests":[{"custom_id":"a","params":{"model":"route","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}}]}`
	if _, err := protocols.Parse(wire.Anthropic, []byte(`{"model":"route","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`), "route"); err != nil {
		t.Fatalf("direct Anthropic params parse: %v", err)
	}
	if _, _, _, err := validateAnthropicBatchRequest([]byte(withoutRoute), "", "", ""); err != nil {
		t.Fatalf("common params.model did not supply the route: %v", err)
	}
	mixedWithoutRoute := `{"requests":[{"custom_id":"a","params":{"model":"route","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}},{"custom_id":"b","params":{"model":"other","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}}]}`
	if _, _, _, err := validateAnthropicBatchRequest([]byte(mixedWithoutRoute), "", "", ""); err == nil {
		t.Fatal("mixed params.model values were accepted without X-OLP-Route")
	}
}

func TestAnthropicResourceRoutesOverrideNativeFallback(t *testing.T) {
	server := &Server{Resources: resources.New(nil), Media: &MediaDeps{}}
	mux := http.NewServeMux()
	server.registerNative(mux)
	server.registerState(mux)
	for _, test := range []struct {
		method, path, pattern string
	}{
		{http.MethodPost, "/anthropic/v1/files", "POST /anthropic/v1/files"},
		{http.MethodGet, "/anthropic/v1/files/file", "GET /anthropic/v1/files/{id}"},
		{http.MethodPost, "/anthropic/v1/messages/batches", "POST /anthropic/v1/messages/batches"},
		{http.MethodGet, "/anthropic/v1/messages/batches/batch/results", "GET /anthropic/v1/messages/batches/{id}/results"},
	} {
		_, pattern := mux.Handler(&http.Request{Method: test.method, URL: &url.URL{Path: test.path}})
		if pattern != test.pattern {
			t.Fatalf("%s %s matched %q, want %q", test.method, test.path, pattern, test.pattern)
		}
	}
}

func TestAnthropicFileReferencesAndAffinity(t *testing.T) {
	params := []byte(`{"system":[{"type":"document","source":{"type":"file","file_id":"anthropic_file_a"}}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"anthropic_file_b"}}]}]}`)
	ids, err := anthropicFileReferences(params)
	if err != nil || len(ids) != 2 || !slices.Contains(ids, "anthropic_file_a") || !slices.Contains(ids, "anthropic_file_b") {
		t.Fatalf("file references = %v", ids)
	}
	rewritten, err := replaceAnthropicFileReferences(params, map[string]string{"anthropic_file_a": "file_a", "anthropic_file_b": "file_b"})
	if err != nil || strings.Contains(string(rewritten), "anthropic_file_") || !strings.Contains(string(rewritten), `"file_id":"file_a"`) {
		t.Fatalf("file reference rewrite = %s, %v", rewritten, err)
	}
	batchBody := []byte(`{"requests":[{"custom_id":"a","params":` + string(params) + `}]}`)
	rewritten, err = replaceAnthropicFileReferences(batchBody, map[string]string{"anthropic_file_a": "file_a", "anthropic_file_b": "file_b"})
	if err != nil || strings.Contains(string(rewritten), "anthropic_file_") {
		t.Fatalf("nested batch file reference rewrite = %s, %v", rewritten, err)
	}
	if sameAnthropicAffinity([]*resources.Resource{
		{ProviderID: "p", TargetID: "t", SlotID: "s"},
		{ProviderID: "p", TargetID: "other", SlotID: "s"},
	}) {
		t.Fatal("mixed file affinities were accepted")
	}
	if !sameAnthropicAffinity([]*resources.Resource{
		{ProviderID: "p", TargetID: "t", SlotID: "s"},
		{ProviderID: "p", TargetID: "t", SlotID: "s"},
	}) {
		t.Fatal("identical file affinity was rejected")
	}
	credential := "credential"
	if sameAnthropicAffinity([]*resources.Resource{
		{ProviderID: "p", TargetID: "t", SlotID: "s", CredentialID: &credential},
		{ProviderID: "p", TargetID: "t", SlotID: "s"},
	}) {
		t.Fatal("credential affinity mismatch was accepted")
	}
	if _, err := replaceAnthropicFileReferences(params, map[string]string{"anthropic_file_a": "file_a"}); err == nil {
		t.Fatal("unmapped file reference was accepted")
	}
	batch := []byte(`{"requests":[{"custom_id":"a","params":` + string(params) + `}]}`)
	batchRefs, err := anthropicBatchFileReferences(batch)
	if err != nil || len(batchRefs) != 2 {
		t.Fatalf("batch file references = %v, %v", batchRefs, err)
	}
}

func TestAnthropicFilesBetaAndListCursors(t *testing.T) {
	for value, valid := range map[string]bool{
		"": true, "files-api-2025-04-14": true, "other": false, "files-api-2025-04-14, other": false,
	} {
		header := http.Header{}
		if value != "" {
			header.Set("Anthropic-Beta", value)
		}
		if err := validateAnthropicFilesBeta(header); (err == nil) != valid {
			t.Fatalf("beta %q validity mismatch: %v", value, err)
		}
	}
	if _, _, _, err := anthropicListCursor(url.Values{"before_id": {"a"}, "after_id": {"b"}}, resources.KindAnthropicFile); err == nil {
		t.Fatal("both Anthropic list cursors were accepted")
	}
	if _, _, _, err := anthropicListCursor(url.Values{"limit": {"1001"}}, resources.KindAnthropicFile); err == nil {
		t.Fatal("Anthropic list limit above 1000 was accepted")
	}
	id := resources.LocalID(resources.KindAnthropicFile, uuid.New())
	if limit, before, after, err := anthropicListCursor(url.Values{"before_id": {id}, "limit": {"1"}}, resources.KindAnthropicFile); err != nil ||
		limit != 1 || before != id || after != id {
		t.Fatalf("Anthropic before cursor = %d/%q/%q, %v", limit, before, after, err)
	}
	if limit, before, after, err := anthropicListCursor(url.Values{"after_id": {id}, "limit": {"1000"}}, resources.KindAnthropicFile); err != nil ||
		limit != 1000 || before != "" || after != id {
		t.Fatalf("Anthropic list cursor = %d/%q/%q, %v", limit, before, after, err)
	}
	if _, _, _, err := anthropicListCursor(url.Values{"after_id": {"anthropic_file_invalid"}}, resources.KindAnthropicFile); err == nil {
		t.Fatal("invalid local cursor was accepted")
	}
}

func TestAnthropicBatchProjectionResultsURL(t *testing.T) {
	body := []byte(`{"id":"msgbatch_upstream","results_url":"/v1/messages/batches/msgbatch_upstream/results","status":"ended"}`)
	projected, err := projectAnthropicBatch(body, "anthropic_batch_local", "https://gateway.example")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(projected, &result) != nil || result["id"] != "anthropic_batch_local" ||
		result["results_url"] != "https://gateway.example/anthropic/v1/messages/batches/anthropic_batch_local/results" ||
		result["status"] != "ended" {
		t.Fatalf("Anthropic batch projection = %s", projected)
	}
}

func TestAnthropicBetaRejectionReleasesAdmission(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 1})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = true
	h.rt.keys[fullKey] = authority
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/files", nil)
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Anthropic-Beta", "unsupported-beta")
		response := httptest.NewRecorder()
		_, _, done := h.gateway.anthropicBegin(response, req, wire.AnthropicFile)
		if !done || response.Code != http.StatusBadRequest || h.gateway.admission.Admitted() != 0 {
			t.Fatalf("beta rejection leaked admission: status=%d admitted=%d", response.Code, h.gateway.admission.Admitted())
		}
	}
}

func TestAnthropicBackwardPageDropsFarthestLookahead(t *testing.T) {
	rows := []*resources.Resource{{ID: "8"}, {ID: "7"}, {ID: "6"}}
	page, more := anthropicListPage(rows, 2, true)
	if !more || len(page) != 2 || page[0].ID != "7" || page[1].ID != "6" {
		t.Fatalf("backward page skipped the nearest resource: %+v more=%v", page, more)
	}
	page, more = anthropicListPage(rows, 2, false)
	if !more || page[0].ID != "8" || page[1].ID != "7" {
		t.Fatalf("forward page changed: %+v more=%v", page, more)
	}
}

func TestAnthropicFileBindingPreservesOpaqueToolInput(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tool","name":"save","input":{"source":{"type":"file","file_id":"local-data"},"large":9007199254740993,"tiny":-0}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool","content":[{"type":"document","source":{"type":"file","file_id":"anthropic_file_owned"}}]}]}]}`)
	refs, err := anthropicFileReferences(body)
	if err != nil || len(refs) != 1 || refs[0] != "anthropic_file_owned" {
		t.Fatalf("application tool JSON was treated as a provider reference: %v err=%v", refs, err)
	}
	rewritten, err := replaceAnthropicFileReferences(body, map[string]string{"anthropic_file_owned": "file_upstream"})
	if err != nil || !strings.Contains(string(rewritten), `"file_id":"local-data"`) ||
		!strings.Contains(string(rewritten), `9007199254740993`) || !strings.Contains(string(rewritten), `"tiny":-0`) ||
		!strings.Contains(string(rewritten), `"file_id":"file_upstream"`) {
		t.Fatalf("file rebinding changed unrelated native tool data: %s err=%v", rewritten, err)
	}
}

func TestAnthropicBatchBindsServingModelSeparatelyFromLogicalModel(t *testing.T) {
	body := []byte(`{"requests":[{"custom_id":"one","params":{"model":"route","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}}]}`)
	bound, _, _, err := validateAnthropicBatchRequest(body, "route", "logical-model", "claude-serving-model")
	if err != nil || !strings.Contains(string(bound), `"model":"claude-serving-model"`) {
		t.Fatalf("Anthropic serving binding was ignored: %s %v", bound, err)
	}
}

func TestNewRetainedFamiliesRequireStatePermissionAndReleaseAdmission(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 1})
	for _, family := range []wire.Format{wire.AnthropicFile, wire.AnthropicBatch, wire.GeminiFile, wire.GeminiBatch, wire.CloudBatch} {
		req := httptest.NewRequest(http.MethodGet, "/retained", nil)
		req.Header.Set("Authorization", "Bearer "+fullKey)
		response := httptest.NewRecorder()
		_, _, done := h.gateway.stateBegin(response, req, family)
		if !done || response.Code != http.StatusForbidden || h.gateway.admission.Admitted() != 0 {
			t.Fatalf("retained family %s bypassed permission or leaked admission: status=%d admitted=%d", family, response.Code, h.gateway.admission.Admitted())
		}
	}
	for _, family := range []wire.Format{wire.File, wire.Batch} {
		req := httptest.NewRequest(http.MethodGet, "/legacy", nil)
		req.Header.Set("Authorization", "Bearer "+fullKey)
		response := httptest.NewRecorder()
		_, _, done := h.gateway.stateBegin(response, req, family)
		if done {
			t.Fatalf("legacy %s permission changed: %d %s", family, response.Code, response.Body.String())
		}
		h.gateway.release(req.Context())
	}
}
