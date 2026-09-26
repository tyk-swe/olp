package telemetry

import (
	"bytes"
	"context"
	"errors"
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
	h, err := Install(Config{Endpoint: collector.URL + "/collect", HeadersFile: file, SampleRatio: 1, AcceptInbound: true, PropagateUpstream: true, Mode: "gateway", Version: "0.1.0"})
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
	for _, required := range []string{"request", "attempt", "safe-route", "safe-key-id", "openllmproxy", "0.1.0"} {
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

func TestForceFlushExportsPartialBatch(t *testing.T) {
	e := &stalledExporter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := NewBoundedSpanProcessor(e, 8, 4, time.Hour)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	tracer := provider.Tracer("flush-test")
	for range 3 {
		_, span := tracer.Start(context.Background(), "partial")
		span.End()
	}
	flushed := make(chan error, 1)
	go func() { flushed <- p.ForceFlush(context.Background()) }()
	select {
	case <-e.entered:
	case <-time.After(time.Second):
		t.Fatal("flush never reached the exporter")
	}
	close(e.release)
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush never returned")
	}
	if got := e.exported.Load(); got != 3 {
		t.Errorf("exported=%d, want 3", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

type failingExporter struct {
	calls chan struct{}
}

func (e *failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	e.calls <- struct{}{}
	return errors.New("export failed")
}
func (e *failingExporter) Shutdown(context.Context) error { return nil }

func TestFailedExportCountsDroppedSpans(t *testing.T) {
	e := &failingExporter{calls: make(chan struct{}, 4)}
	p := NewBoundedSpanProcessor(&countingExporter{inner: e}, 8, 2, time.Hour)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	tracer := provider.Tracer("drop-test")
	before := ExportDroppedTotal()
	for range 3 {
		_, span := tracer.Start(context.Background(), "dropped")
		span.End()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	if dropped := ExportDroppedTotal() - before; dropped != 3 {
		t.Errorf("dropped=%d, want 3", dropped)
	}
	if calls := len(e.calls); calls != 2 {
		t.Errorf("export calls=%d, want a full batch and a partial flush", calls)
	}
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
