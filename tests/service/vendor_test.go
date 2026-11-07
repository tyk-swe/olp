//go:build service

package service_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	vendorSecret       = "vendor-secret"
	vendorModel        = "fixture-model"
	vendorAnswer       = "hello from the fixture vendor"
	vendorInputTokens  = 4
	vendorOutputTokens = 6
)

func newVendor(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authorized := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+vendorSecret }
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+vendorModel+`","object":"model"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != vendorModel {
			http.Error(w, `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`, http.StatusNotFound)
			return
		}
		usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, vendorInputTokens, vendorOutputTokens, vendorInputTokens+vendorOutputTokens)
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":%s}`, vendorModel, vendorAnswer, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", vendorModel, vendorAnswer)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\n", vendorModel, usage)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != vendorModel {
			http.Error(w, `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`, http.StatusNotFound)
			return
		}
		response := map[string]any{
			"id": "resp_fixture", "object": "response", "created_at": 1, "model": vendorModel, "status": "completed", "error": nil, "incomplete_details": nil,
			"output": []any{map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": vendorAnswer, "annotations": []any{}}}}},
			"usage":  map[string]int{"input_tokens": vendorInputTokens, "output_tokens": vendorOutputTokens, "total_tokens": vendorInputTokens + vendorOutputTokens},
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": vendorAnswer})
		terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", delta, terminal)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// activeProvider creates a provider against the fixture vendor and takes it
// through probe, certification and activation.
func (s *service) activeProvider(owner *caller, name, endpoint string, projectID string) map[string]any {
	s.t.Helper()
	provider := s.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          name,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": endpoint + "/v1"},
		"credential":    vendorSecret,
		"model":         vendorModel,
		"project_id":    projectID,
	}, idempotent(), http.StatusCreated)
	path := "/api/v1/providers/" + provider["id"].(string)
	probe := s.want(owner, "POST", path+"/probe", nil, etag(provider), http.StatusOK)
	if probe["succeeded"] != true {
		s.t.Fatalf("probe failed: %v", probe)
	}
	models := s.want(owner, "GET", path+"/models", nil, nil, http.StatusOK)["items"].([]any)
	modelID := ""
	for _, item := range models {
		model := item.(map[string]any)
		if model["upstream_model"] == vendorModel {
			modelID = model["id"].(string)
		}
	}
	if modelID == "" {
		s.t.Fatalf("probe did not discover %s: %v", vendorModel, models)
	}
	detail := s.want(owner, "GET", path, nil, nil, http.StatusOK)
	detail = s.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{
		"enabled": true,
		"capabilities": []any{
			map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
			map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
		},
	}, etag(detail), http.StatusOK)
	certified := s.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etag(detail), http.StatusOK)
	if certified["status"] != "certified" {
		s.t.Fatalf("certification failed: %v", certified)
	}
	detail = s.want(owner, "GET", path, nil, nil, http.StatusOK)
	s.want(owner, "POST", path+"/activate", nil, idempotent(etag(detail)), http.StatusOK)
	return provider
}

// publishedRoute drafts and activates a route onto provider, returning the
// activation response.
func (s *service) publishedRoute(owner *caller, slug string, provider map[string]any, projectID string) (map[string]any, map[string]any) {
	s.t.Helper()
	draft := s.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":    []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		"project_id": projectID,
	}, idempotent(), http.StatusCreated)
	activated := s.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, idempotent(etag(draft)), http.StatusOK)
	return draft, activated
}

func uniqueName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func newCodeVendorProxy(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"chatgpt.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "unsupported fixture destination", http.StatusBadGateway)
			return
		}
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		secured := tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
		defer secured.Close()
		request, err := http.ReadRequest(bufio.NewReader(secured))
		if err != nil {
			return
		}
		defer request.Body.Close()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		response := recorder.Result()
		defer response.Body.Close()
		_ = response.Write(secured)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
