package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/openapi"
)

func TestUnknownOperationAnswersProblem(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected problem+json, got %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("expected no-store, got %q", cc)
	}
	var body struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(body.Type, "operation_not_found") {
		t.Fatalf("expected an operation_not_found problem type, got %q", body.Type)
	}
	if body.Detail != "The requested management operation does not exist." {
		t.Fatalf("unexpected problem detail %q", body.Detail)
	}
}

func TestOpenAPIDocumentServed(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), openapi.Document) {
		t.Fatal("expected the embedded OpenAPI document")
	}
}
