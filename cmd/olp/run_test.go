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
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/panics"
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
