//go:build integration

package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderCataloguePaginationUsesRecordIdentifiers(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	paths := []string{"/api/v3/provider-models", "/api/v3/provider-health"}
	for _, path := range paths {
		empty := h.want(owner, "GET", path+"?limit=1", nil, nil, 200)
		if len(empty["items"].([]any)) != 0 || empty["next_cursor"] != nil {
			t.Fatalf("empty catalogue: %v", empty)
		}
	}
	up := newVendor(t)
	for i := range 2 {
		h.want(owner, "POST", "/api/v3/providers", map[string]any{
			"name": fmt.Sprintf("Paginated provider %d", i), "model": vendorModel, "credential": vendorSecret,
			"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
		}, map[string]string{"Idempotency-Key": fmt.Sprintf("provider-%d", i)}, 201)
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			full := h.want(owner, "GET", path+"?limit=2", nil, nil, 200)
			items := full["items"].([]any)
			if len(items) != 2 || full["next_cursor"] != nil {
				t.Fatalf("complete page: %v", full)
			}
			cursor := ""
			for i, expected := range items {
				page := h.want(owner, "GET", path+"?limit=1&cursor="+cursor, nil, nil, 200)
				if got := page["items"].([]any); len(got) != 1 || !reflect.DeepEqual(got[0], expected) {
					t.Fatalf("page %d: %v, want %v", i, page, expected)
				}
				item := expected.(map[string]any)
				id := item["provider_id"].(string)
				if path == "/api/v3/provider-models" {
					id = item["model"].(map[string]any)["id"].(string)
				}
				cursor = base64.RawURLEncoding.EncodeToString([]byte(id))
				if i == 0 && page["next_cursor"] != cursor || i == 1 && page["next_cursor"] != nil {
					t.Fatalf("wrong cursor on page %d: %v", i, page)
				}
				if path == "/api/v3/provider-health" && page["window_minutes"] != float64(15) {
					t.Fatalf("health window lost: %v", page)
				}
			}
			empty := h.want(owner, "GET", path+"?limit=1&cursor="+cursor, nil, nil, 200)
			if len(empty["items"].([]any)) != 0 || empty["next_cursor"] != nil {
				t.Fatalf("page beyond the last record: %v", empty)
			}
		})
	}
}

func TestProviderDiagnosticsDoNotPersistUpstreamCredentialEchoes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			t.Error("probe did not use the stored credential")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
			"code": r.Header.Get("Authorization"), "message": r.Header.Get("Authorization"),
		}})
	}))
	defer up.Close()
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "Credential echo", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	path := "/api/v3/providers/" + created["id"].(string)
	const safeDetail = "The upstream rejected the credential (HTTP 401)."
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(created), 200)
	if probe["succeeded"] != false || probe["detail"] != safeDetail {
		t.Fatalf("unsafe probe response: %v", probe)
	}
	discovery := h.want(owner, "POST", path+"/discovery", map[string]any{"models": []any{}}, etagHeader(created), 422)
	if discovery["detail"] != safeDetail {
		t.Fatalf("unsafe discovery response: %v", discovery)
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if detail["last_probe_status"] != "failed" || detail["last_probe_detail"] != safeDetail {
		t.Fatalf("unsafe stored probe diagnostic: %v", detail)
	}
	health := h.want(owner, "GET", "/api/v3/provider-health", nil, nil, 200)
	if health["items"].([]any)[0].(map[string]any)["last_probe_detail"] != safeDetail {
		t.Fatalf("unsafe health diagnostic: %v", health)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
	if certified["status"] != "failed" || certified["attempted_count"] != float64(2) || calls.Load() != 4 {
		t.Fatalf("certification results: %v; upstream calls=%d", certified, calls.Load())
	}
	for _, raw := range certified["results"].([]any) {
		result := raw.(map[string]any)
		if result["error_code"] != "upstream_authentication_failed" || result["detail"] != safeDetail {
			t.Fatalf("unsafe certification diagnostic: %v", result)
		}
	}
}

func TestCertificationAllowsBothProbeBudgetsAndPersistsEvidence(t *testing.T) {
	t.Parallel()
	h := newAccessHarness(t)
	owner := h.owner()
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		select {
		case <-time.After(8 * time.Second):
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/v1/responses" {
			writeResponsesFixture(w, vendorModel, "OK", input.Stream)
			return
		}
		if input.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
		}
	}))
	defer up.Close()
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "Slow certification", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	path := "/api/v3/providers/" + created["id"].(string)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
	if certified["status"] != "certified" || certified["certified_count"] != float64(2) || calls.Load() != 4 {
		t.Fatalf("both endpoint and mode probes should succeed: %v; upstream calls=%d", certified, calls.Load())
	}
	models = h.want(owner, "GET", path+"/models", nil, nil, 200)
	capabilities := models["items"].([]any)[0].(map[string]any)["capabilities"].([]any)
	if len(capabilities) != 2 {
		t.Fatalf("persisted capabilities: %v", capabilities)
	}
	for _, raw := range capabilities {
		capability := raw.(map[string]any)
		if capability["source"] != "certified" || capability["certified_at"] == nil {
			t.Fatalf("certification evidence was not persisted: %v", capability)
		}
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if detail["last_probe_status"] != "succeeded" || detail["etag"] == created["etag"] {
		t.Fatalf("certification did not commit provider state: %v", detail)
	}
}
