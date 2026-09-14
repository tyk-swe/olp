// sdkfixture serves the Go inference surface against an in-process mock
// upstream so the official SDKs exercise the real gateway: bounded ingress,
// key authentication, route selection, credential injection, model
// rewriting, and native streaming. Nothing here fabricates a success the
// gateway did not produce.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/runtime"
)

const (
	routeSlug     = "sdk-smoke-route"
	upstreamModel = "fixture-model"
	apiKey        = "olp_go_fixture_key"
	conflictKey   = "olp_go_fixture_conflict_key"
	credential    = "fixture-upstream-credential"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// staticRuntime pins one release and two keys without a database.
type staticRuntime struct {
	release *runtime.Release
	keys    map[string]access.Authority
}

func (s *staticRuntime) Release() *runtime.Release { return s.release }

func (s *staticRuntime) Authenticate(secret string) (access.Authority, error) {
	authority, ok := s.keys[secret]
	if !ok {
		return access.Authority{}, runtime.ErrInvalidKey
	}
	return authority, nil
}

func (s *staticRuntime) Revoked(string) bool { return false }

func run() error {
	path := os.Getenv("OLP_SDK_SMOKE_METADATA")
	if path == "" {
		return fmt.Errorf("OLP_SDK_SMOKE_METADATA is required")
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer upstream.Close()
	upstreamServer := &http.Server{Handler: mockUpstream(), ReadHeaderTimeout: 5 * time.Second}
	go upstreamServer.Serve(upstream)
	defer upstreamServer.Close()

	release, err := fixtureRelease("http://" + upstream.Addr().String() + "/v1")
	if err != nil {
		return err
	}
	keyID := uuid.NewString()
	rt := &staticRuntime{release: release, keys: map[string]access.Authority{
		apiKey:      {ID: keyID, Issuer: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}},
		conflictKey: {ID: uuid.NewString(), Issuer: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowedRoutes: []string{"another-route"}}},
	}}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	gw := gateway.New(rt, &policy, gateway.Config{MaxInFlight: 64, MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 1 << 16}, log)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	mux := http.NewServeMux()
	management.Register(mux)
	gw.Register(mux)
	mux.HandleFunc("/", http.NotFound)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	defer server.Close()
	metadata, err := json.Marshal(map[string]string{"origin": "http://" + listener.Addr().String(), "api_key": apiKey, "conflict_api_key": conflictKey, "route_slug": routeSlug})
	if err != nil {
		return err
	}
	// Publish atomically only after binding; consumers never see partial JSON.
	if err := os.WriteFile(path+".tmp", metadata, 0600); err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

// fixtureRelease publishes one active provider and one route to it.
func fixtureRelease(endpoint string) (*runtime.Release, error) {
	providerID, credentialID, slotID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	routeID, targetID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	version := 1
	snapshot := &runtime.Snapshot{
		Generation: runtime.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: now},
		Providers: map[string]runtime.Provider{providerID: {
			ID:               providerID,
			Name:             "fixture",
			Kind:             "openai_compatible",
			Enabled:          true,
			ActiveCredential: &credentialID,
			Capabilities: []runtime.Capability{
				{Model: upstreamModel, Operation: "generation", Surface: "openai", Mode: "unary"},
				{Model: upstreamModel, Operation: "generation", Surface: "openai", Mode: "streaming"},
			},
			RevisionID: uuid.NewString(),
			Endpoint:   endpoint,
			AuthMode:   "api_key",
			Slots:      []runtime.Slot{{ID: slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &credentialID, CredentialVersion: &version}},
		}},
		Routes: map[string]runtime.Route{routeSlug: {
			ID:             routeID,
			Slug:           routeSlug,
			Operations:     []string{"generation"},
			OverallTimeout: 5000,
			MaxAttempts:    2,
			Targets:        []runtime.Target{{ID: targetID, ProviderID: providerID, ProviderModel: upstreamModel, Weight: 1, Timeout: 4000, RoutingID: targetID}},
			RoutingID:      routeID,
			RevisionID:     uuid.NewString(),
			Revision:       1,
			PublishedAt:    now,
		}},
	}
	return runtime.NewRelease(uuid.NewString(), 1, snapshot, map[string][]byte{credentialID: []byte(credential)})
}

// mockUpstream answers like an OpenAI-compatible vendor for the fixture
// model only, and only with the fixture credential.
func mockUpstream() http.Handler {
	text := "official openai sdk reached " + routeSlug
	mux := http.NewServeMux()
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+credential {
			http.Error(w, `{"error":{"message":"bad credential","type":"invalid_request_error","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{{"id": upstreamModel, "object": "model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != upstreamModel {
			http.Error(w, `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`, http.StatusNotFound)
			return
		}
		usage := map[string]any{"prompt_tokens": 4, "completion_tokens": 5, "total_tokens": 9}
		if !body.Stream {
			writeJSON(w, map[string]any{
				"id": "chatcmpl-fixture", "object": "chat.completion", "created": time.Now().Unix(), "model": upstreamModel,
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
				"usage":   usage,
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunk := func(delta map[string]any, finish any, withUsage bool) {
			event := map[string]any{"id": "chatcmpl-fixture", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": upstreamModel,
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
			if withUsage {
				event["usage"] = usage
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk(map[string]any{"role": "assistant", "content": ""}, nil, false)
		for i, word := range strings.SplitAfter(text, " ") {
			_ = i
			chunk(map[string]any{"content": word}, nil, false)
		}
		chunk(map[string]any{}, "stop", true)
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != upstreamModel {
			http.Error(w, `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{
			"id": "resp_fixture", "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": upstreamModel,
			"output": []map[string]any{{"type": "message", "id": "msg_fixture", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}},
			"usage": map[string]any{"input_tokens": 4, "output_tokens": 5, "total_tokens": 9},
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}
