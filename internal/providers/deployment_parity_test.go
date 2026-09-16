package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestDeploymentCertificationKeepsBodyAndPathOnTheSameSingleMapping(t *testing.T) {
	paths := []string{}
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid probe body")
			w.WriteHeader(400)
			return
		}
		if body["model"] != "wire-b" || !strings.HasPrefix(r.URL.Path, "/openai/deployments/wire-b/") {
			t.Errorf("mapping changed the body/path relationship: %v %s", body["model"], r.URL.Path)
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/responses") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "response", "object": "response", "model": "wire-b", "status": "completed", "output": []any{}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "chat", "object": "chat.completion", "model": "wire-b", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		}
	}))
	defer upstream.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	deployment, version := "fallback", "2024-10-21"
	cfg := Configuration{Kind: KindAzure, AuthMode: AuthAPIKey, Endpoint: &upstream.URL, Deployment: &deployment, APIVersion: &version, Options: Options{Models: map[string]json.RawMessage{"logical-a": json.RawMessage(`{"deployment":"wire-b"}`), "wire-b": json.RawMessage(`{"deployment":"wire-c"}`)}}}
	cfg.normalize()
	err := New(nil, policy).certifyTuple(t.Context(), &cfg, []byte("fixture-secret"), "logical-a", capabilityInput{Operation: "generation", Surface: "openai", Mode: ModeUnary}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/openai/deployments/wire-b/chat/completions" || paths[1] != "/openai/deployments/wire-b/responses" {
		t.Fatalf("both native endpoints must certify the same deployment: %v", paths)
	}
}
