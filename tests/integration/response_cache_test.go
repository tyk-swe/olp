//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestResponseCacheStreamCapture(t *testing.T) {
	h := newAccessHarness(t)
	installResponseCache(t, h)
	fixture := newOpenAIFixture(t, "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			if strings.Contains(string(raw), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"id\":\"chat\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n", vendorModel)
				return
			}
		}
		fixture.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	owner, _, slug, secret := provisionOpenAIWith(t, h, upstream.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, map[string]any{"cache": map[string]any{"mode": "exact"}})
	detail := h.want(owner, "GET", "/api/v1/observability/capture", nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/observability/capture", map[string]any{"enabled": true}, etagHeader(detail), 200)
	receiver, server := newExportReceiver(t, "", 204)
	sink := h.want(owner, "POST", "/api/v1/observability/sinks", map[string]any{
		"name": "cached-stream-capture", "type": "https", "destination": server.URL, "streams": []string{"requests"}, "format": "json",
	}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/observability/capture-policies", map[string]any{
		"sink": sink["export_sink_id"], "sample_ratio": "1", "include": []string{"output", "tool_calls"},
		"max_bytes": 1048576, "route_slug": slug,
	}, idem(uuid.NewString()), 201)
	manager := &export.Manager{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.Gateway.Capture = manager
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.After(5 * time.Second)
	for {
		if collector := manager.Resolve("", slug, "", "", uuid.NewString()); collector != nil {
			collector.Close()
			break
		}
		select {
		case <-deadline:
			t.Fatal("capture policy was not loaded")
		case <-time.After(10 * time.Millisecond):
		}
	}
	for _, want := range []string{"miss", "hit"} {
		status, raw, headers := h.gatewayRaw(http.MethodPost, "/v1/chat/completions", secret,
			strings.NewReader(`{"model":"`+slug+`","messages":[{"role":"user","content":"capture cached stream"}],"stream":true}`), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusOK || headers.Get("X-OLP-Cache") != want || !strings.Contains(string(raw), "lookup") {
			t.Fatalf("stream %s: status=%d cache=%s body=%s", want, status, headers.Get("X-OLP-Cache"), raw)
		}
	}
	for _, record := range awaitEnvelope(t, receiver, 2) {
		data, _ := record["data"].(map[string]any)
		tools, _ := data["tool_calls"].([]any)
		if data["output"] != "OK" || len(tools) != 1 {
			t.Fatalf("stream capture lost delivered output or tools: %v", data)
		}
	}
	var captured int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.requests WHERE payload_captured AND cache_result IN ('miss','hit')`).Scan(&captured); err != nil || captured != 2 {
		t.Fatalf("captured requests=%d, want 2: %v", captured, err)
	}
}

func installResponseCache(t *testing.T, h *accessHarness) {
	t.Helper()
	c := client(t, required(t, "OLP_TEST_CACHE_VALKEY_URL"), time.Second)
	h.Gateway.Cache = &cache.Store{Client: c, Keys: h.Server.Keys, Auth: h.Server.Auth, Installation: h.Server.Installation}
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, e *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, e, payload)
		if err != nil {
			t.Errorf("persist cache event: %v", err)
		}
		return err
	}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// The first observation installs this new account's historical balance.
