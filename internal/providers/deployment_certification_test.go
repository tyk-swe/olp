package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
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
	cfg.Normalize()
	err := New(nil, policy, nil).certifyTuple(t.Context(), &cfg, []byte("fixture-secret"), "logical-a", CapabilityInput{Operation: "generation", Surface: "openai", Mode: ModeUnary}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/openai/deployments/wire-b/chat/completions" || paths[1] != "/openai/deployments/wire-b/responses" {
		t.Fatalf("both native endpoints must certify the same deployment: %v", paths)
	}
}

func TestCertificationFailsClosedWhenAuthenticationStorageIsUnavailable(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://127.0.0.1:1/olp?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	mux := http.NewServeMux()
	New(&access.Server{Pool: pool, Auth: secrets.NewAuthKey(make([]byte, 32), "test")}, &egress.Policy{}, nil).Register(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	providerID, modelID := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name       string
		providerID string
		modelID    string
		token      string
		status     int
	}{
		{"valid identifiers", providerID, modelID, "olpm_" + uuid.NewString() + "_token", http.StatusServiceUnavailable},
		{"invalid provider identifier", "invalid", modelID, "olpm_" + uuid.NewString() + "_token", http.StatusServiceUnavailable},
		{"invalid model identifier", providerID, "invalid", "olpm_" + uuid.NewString() + "_token", http.StatusServiceUnavailable},
		{"malformed credential", providerID, modelID, "olpm_invalid", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/providers/"+tc.providerID+"/models/"+tc.modelID+"/certify", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+tc.token)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("certification returned %d, want %d", response.StatusCode, tc.status)
			}
		})
	}
}
