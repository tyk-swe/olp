package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/platform/concurrency"
	"github.com/tyk-swe/olp/internal/platform/panics"
)

func TestControlNeverAnswersConsumerProtocolPathsWithTheConsole(t *testing.T) {
	public := http.NewServeMux()
	public.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("console")) })
	reservePublicPaths(public, Control)
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/mcp?toolset_id=ts"},
		{http.MethodGet, "/mcp/sse?toolset_id=ts"},
		{http.MethodPost, "/mcp/messages?session_id=s"},
		{http.MethodPost, "/a2a/agent"},
		{http.MethodGet, "/a2a/agent/.well-known/agent-card.json"},
	} {
		recorder := httptest.NewRecorder()
		public.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
		if recorder.Code != http.StatusNotFound || recorder.Body.String() == "console" {
			t.Errorf("%s %s answered %d %q, want a reserved 404", request.method, request.path, recorder.Code, recorder.Body.String())
		}
	}
	console := httptest.NewRecorder()
	Perimeter("https://example.invalid", public).ServeHTTP(console, httptest.NewRequest(http.MethodGet, "/mcp-servers", nil))
	if console.Body.String() != "console" || console.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("MCP Servers did not receive the console page and its perimeter headers")
	}
}

func TestReencryptDryRunAcceptsConfigurationFlagsBeforeIt(t *testing.T) {
	for _, options := range [][]string{
		{"--dry-run", "--database-url", "invalid"},
		{"--database-url", "invalid", "--dry-run"},
		{"--database-url", "invalid", "--dry-run=true"},
		{"--database-url", "invalid", "--dry-run=false"},
	} {
		args := append([]string{"master-key", "reencrypt"}, options...)
		if err := run(t.Context(), append(args, "--help")); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("reencrypt %v: %v, want help after accepted flags", options, err)
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestListenerLogsHandlerPanicsAsStructuredErrors(t *testing.T) {
	var output syncBuffer
	log := slog.New(slog.NewJSONHandler(&output, nil))
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("handler exploded") })
	listeners, err := bindListeners(t.Context(), []listenerConfig{{name: "public", address: "127.0.0.1:0", handler: handler}}, Config{MaxConnections: 4, ConnectionMaxAgeSeconds: 60, ConnectionDrainTimeoutSeconds: 1}, t.Context(), log)
	if err != nil {
		t.Fatal(err)
	}
	listener := listeners[0]
	go listener.server.Serve(listener.socket)
	t.Cleanup(func() { _ = listener.server.Shutdown(t.Context()) })
	if response, err := http.Get("http://" + listener.socket.Addr().String()); err == nil {
		response.Body.Close()
		t.Fatalf("a panicking handler answered %d", response.StatusCode)
	}
	for line := range strings.Lines(output.String()) {
		var record struct{ Level, Msg string }
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "ERROR" && strings.Contains(record.Msg, "handler exploded") {
			return
		}
	}
	t.Fatalf("no structured error record for the handler panic in %q", output.String())
}

func TestPanickingWorkerCancelsTheProcessWithItsCause(t *testing.T) {
	ctx, stop := context.WithCancelCause(t.Context())
	defer stop(nil)
	var logs syncBuffer
	var workers sync.WaitGroup
	workers.Go(supervisor(slog.New(slog.NewJSONHandler(&logs, nil)), stop)(func() { panic("ledger corrupted") }))
	workers.Wait()
	<-ctx.Done()
	crash, ok := errors.AsType[*panics.Error](context.Cause(ctx))
	if !ok {
		t.Fatalf("cause = %v, want the worker panic", context.Cause(ctx))
	}
	if crash.Value != "ledger corrupted" || !strings.Contains(logs.String(), `"panic":"ledger corrupted"`) {
		t.Fatalf("panic %v was not reported, logs: %s", crash.Value, logs.String())
	}
}

func TestA2AUsesInferenceAdmissionAndPerimeter(t *testing.T) {
	management := concurrency.NewPool(1)
	occupied := management.AcquirePermit()
	defer occupied.Release()
	inference := concurrency.NewPool(1)
	public := &concurrency.Public{Inference: inference, Management: management, InferenceEnabled: true, Reject: rejectPublic}
	handler := Perimeter("https://example.invalid", public.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	for path, method := range map[string]string{"/a2a/agent": http.MethodPost, "/a2a/agent/.well-known/agent-card.json": http.MethodGet} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Cross-Origin-Resource-Policy") != "" || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("A2A %s status=%d headers=%v", path, recorder.Code, recorder.Header())
		}
	}
	publicGateway := http.NewServeMux()
	publicGateway.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("console")) })
	reservePublicPaths(publicGateway, Gateway)
	unknown := httptest.NewRecorder()
	publicGateway.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/a2a/missing", nil))
	if unknown.Code != http.StatusNotFound || unknown.Body.String() == "console" {
		t.Fatal("unknown A2A endpoint reached the console")
	}

	managementResponse := httptest.NewRecorder()
	handler.ServeHTTP(managementResponse, httptest.NewRequest(http.MethodGet, "/api/v1/a2a-tasks", nil))
	if managementResponse.Code != http.StatusServiceUnavailable {
		t.Fatal("operator A2A tasks bypassed the occupied management pool")
	}
	busyInference := inference.AcquirePermit()
	defer busyInference.Release()
	inferenceResponse := httptest.NewRecorder()
	handler.ServeHTTP(inferenceResponse, httptest.NewRequest(http.MethodPost, "/a2a/agent", nil))
	if inferenceResponse.Code != http.StatusServiceUnavailable {
		t.Fatal("A2A bypassed the occupied inference pool")
	}
	filtered := managementNetwork([]netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for path, want := range map[string]int{"/a2a/agent": http.StatusOK, "/api/v1/a2a-tasks": http.StatusForbidden} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.RemoteAddr = "198.51.100.1:1234"
		filtered.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("network filter %s status=%d, want %d", path, response.Code, want)
		}
	}
}
