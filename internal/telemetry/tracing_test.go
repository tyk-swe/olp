package telemetry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestOTLPExportsMetadataAndPreservesTraceParentWithoutSecrets(t *testing.T) {
	payloads := make(chan []byte, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collect" || r.Header.Get("Authorization") != "Bearer collector-secret" {
			t.Error("collector endpoint or mounted authentication was lost")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
		}
		payloads <- body
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	file := filepath.Join(t.TempDir(), "headers.json")
	if err := os.WriteFile(file, []byte(`{"Authorization":"Bearer collector-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := Install(Config{Endpoint: collector.URL + "/collect", HeadersFile: file, SampleRatio: 1, AcceptInbound: true, PropagateUpstream: true, Mode: "gateway", Version: "3.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/responses?secret=query-secret", strings.NewReader(`{"input":"prompt-secret"}`))
	r.Header.Set("Authorization", "Bearer client-secret")
	r.Header.Set("Cookie", "session=session-secret")
	r.Header.Set("Tracestate", "vendor=tracestate-secret")
	r.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	r.Header.Set("X-Request-Id", "01a00000-0000-7000-8000-000000000001")
	AdmittedRequest(h.Runtime().ForInstallation("installation"), h.Tracer(), "openai", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		root := trace.SpanContextFromContext(r.Context())
		if root.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || root.TraceState().String() != "" {
			t.Error("inbound parent or tracestate policy changed")
		}
		request := RequestFromContext(r.Context())
		request.RecordInferenceContext("openai", "generation", "safe-route", "safe-key-id", "generation")
		_, attempt := request.Attempt(r.Context(), "openai", "revision", "model")
		headers := http.Header{}
		attempt.InjectUpstream(headers, true)
		if !strings.Contains(headers.Get("Traceparent"), root.TraceID().String()) || headers.Get("Tracestate") != "" {
			t.Error("outbound trace parent or privacy policy changed")
		}
		attempt.Finish("success", 200)
		request.RecordTerminal(200, "", 1, time.Millisecond, 2*time.Millisecond)
		_, _ = w.Write([]byte("response-secret"))
	})).ServeHTTP(httptest.NewRecorder(), r)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	close(payloads)
	var payload []byte
	for part := range payloads {
		payload = append(payload, part...)
	}
	for _, required := range []string{"request", "attempt", "safe-route", "safe-key-id", "openllmproxy", "3.0.0"} {
		if !bytes.Contains(payload, []byte(required)) {
			t.Errorf("export omitted %s", required)
		}
	}
	for _, secret := range []string{"collector-secret", "client-secret", "session-secret", "prompt-secret", "query-secret", "tracestate-secret", "response-secret"} {
		if bytes.Contains(payload, []byte(secret)) {
			t.Errorf("export included private %s", secret)
		}
	}
}

type stalledExporter struct {
	entered  chan struct{}
	release  chan struct{}
	exported atomic.Int64
}

func (e *stalledExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	select {
	case e.entered <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
		e.exported.Add(int64(len(spans)))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (*stalledExporter) Shutdown(context.Context) error { return nil }

func TestFullTraceQueueNeverBlocksInferenceAndCountsLoss(t *testing.T) {
	e := &stalledExporter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := NewBoundedSpanProcessor(e, 2, 1, time.Hour)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	tracer := provider.Tracer("bounded-test")
	before := ExportDroppedTotal()
	_, span := tracer.Start(context.Background(), "first")
	span.End()
	select {
	case <-e.entered:
	case <-time.After(time.Second):
		t.Fatal("export never started")
	}
	done := make(chan struct{})
	go func() {
		for range 100 {
			_, span := tracer.Start(context.Background(), "queued")
			span.End()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(e.release)
		t.Fatal("full telemetry queue blocked inference")
	}
	if dropped := ExportDroppedTotal() - before; dropped != 98 {
		t.Errorf("dropped=%d, want 98", dropped)
	}
	close(e.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.exported.Load(); got != 3 {
		t.Errorf("exported=%d, want 3", got)
	}
}
