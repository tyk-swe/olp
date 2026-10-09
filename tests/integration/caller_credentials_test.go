//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestCallerCredentialsStayOutOfDurableStateAndTelemetry(t *testing.T) {
	h := newAccessHarness(t)
	upstream := newOpenAIFixture(t, "")
	var expected atomic.Value
	expected.Store("")
	var probeCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential := r.Header.Get("api-key")
		if r.Header.Get("X-OLP-Provider-Credential") != "" {
			t.Error("local credential header forwarded")
		}
		want := expected.Load().(string)
		if want == "" {
			if credential != "" {
				probeCalls.Add(1)
			}
		} else if credential != want {
			t.Error("serving substituted the probe or another caller credential")
		}
		if want == "caller-private-rejected" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "refused " + credential, "type": "authentication_error"}})
			return
		}
		upstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	_, provider, slug, key := provisionOpenAIWith(t, h, proxy.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, nil, map[string]any{"credential_source": "caller"})
	if probeCalls.Load() == 0 {
		t.Fatal("caller connection was not certified with the operator credential")
	}
	var logs endUserAuditLog
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL, logger)
	replica.refresh()
	exporter := tracetest.NewInMemoryExporter()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tracer.Shutdown(context.Background()) })
	server := httptest.NewServer(telemetry.AdmittedRequest(telemetry.RequestConfig{}, tracer.Tracer("caller-credentials"), "openai", replica.HTTP.Config.Handler))
	t.Cleanup(server.Close)
	valkey, _, stream := acctKeyspace(t)
	emitter := usage.NewEmitter(32)
	replica.Gateway.Sink = &gateway.AccountingSink{Emitter: emitter, Log: logger}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); emitter.RunWriter(ctx, valkey, stream, logger) }()
	t.Cleanup(func() { emitter.Close(); <-done; cancel() })
	stop := acctConsumer(t, h.Pool, acctValkey(t), stream, "caller-credentials", nil, 30*time.Second)
	defer stop()
	secrets := []string{"caller-private-one", "caller-private-rejected", "caller-private-two"}
	for _, secret := range secrets {
		expected.Store(secret)
		r, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+slug+`","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-OLP-Provider-Credential", secret)
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("upstream error leaked caller credential")
		}
		if (response.StatusCode == 200) != (secret != "caller-private-rejected") {
			t.Fatalf("unexpected response %d: %s", response.StatusCode, body)
		}
	}
	glEventually(t, "caller accounting", func() bool {
		var n int
		_ = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempts WHERE provider_id=$1 AND routing->>'credential_source'='caller'`, provider["id"]).Scan(&n)
		return n == 3
	})
	for _, table := range []string{"requests", "attempts", "attempt_usage_facts", "api_keys", "providers", "provider_revisions", "audit", "media_jobs", "provider_resources"} {
		var raw string
		if err := h.Pool.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(row))::text,'[]') FROM olp.`+table+` row`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(raw, secret) {
				t.Fatalf("caller secret persisted in %s", table)
			}
		}
	}
	entries, err := valkey.Do(t.Context(), "XRANGE", stream, "-", "+")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(entries)
	spans, _ := json.Marshal(exporter.GetSpans())
	logs.mu.Lock()
	logData := append([]byte(nil), logs.data...)
	logs.mu.Unlock()
	for _, secret := range secrets {
		if bytes.Contains(raw, []byte(secret)) || bytes.Contains(spans, []byte(secret)) || bytes.Contains(logData, []byte(secret)) {
			t.Fatal("caller secret retained by telemetry")
		}
	}
}
