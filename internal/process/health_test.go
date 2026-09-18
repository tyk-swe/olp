package process

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRejectPublicShapesSurfaceError(t *testing.T) {
	w := httptest.NewRecorder()
	rejectPublic(w, httptest.NewRequest("POST", "/api/v3/routes", nil), "management")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("management rejection: %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("management rejection must be a problem document: %s", w.Header().Get("Content-Type"))
	}
	var problem map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem body: %v", err)
	}
	if problem["type"] != "https://openllmproxy.dev/problems/request_admission_overloaded" {
		t.Fatalf("problem code: %v", problem["type"])
	}

	w = httptest.NewRecorder()
	rejectPublic(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), "openai")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("inference rejection: %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("inference body: %v", err)
	}
	failure, ok := body["error"].(map[string]any)
	if !ok || failure["type"] != "server_error" || failure["code"] != "request_admission_overloaded" {
		t.Fatalf("inference rejection must reuse the gateway envelope: %v", body)
	}
}
