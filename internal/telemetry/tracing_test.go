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
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
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
	inputTokens, outputTokens, cachedTokens := int64(12), int64(7), int64(3)
	model := "observed-model"
	mediaUnits := "audio_ms=240"
	finishReasons := []string{"stop"}
	AdmittedRequest(h.Runtime().ForInstallation("installation"), h.Tracer(), "openai", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		root := trace.SpanContextFromContext(r.Context())
		if root.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || root.TraceState().String() != "" {
			t.Error("inbound parent or tracestate policy changed")
		}
		request := RequestFromContext(r.Context())
		request.RecordInferenceContext("openai", "generation", "chat", "safe-route", "safe-key-id", "generation")
		_, attempt := request.Attempt(r.Context(), "openai", "revision", "model")
		attempt.RecordUsage(&inputTokens, &outputTokens, &cachedTokens, &mediaUnits)
		attempt.RecordResponse(model, finishReasons)
		request.RecordResponse("openai", model, &inputTokens, &outputTokens, finishReasons)
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
	spans := map[string]*tracepb.Span{}
	for part := range payloads {
		payload = append(payload, part...)
		batch := &collectortrace.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(part, batch); err != nil {
			t.Fatalf("decode exported spans: %v", err)
		}
		for _, resource := range batch.GetResourceSpans() {
			for _, scope := range resource.GetScopeSpans() {
				for _, span := range scope.GetSpans() {
					if spans[span.GetName()] != nil {
						t.Fatalf("duplicate exported span %q", span.GetName())
					}
					spans[span.GetName()] = span
				}
			}
		}
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
	for _, name := range []string{"request", "attempt"} {
		span := spans[name]
		if span == nil {
			t.Fatalf("missing %s span", name)
		}
		attrs := map[string]*commonpb.AnyValue{}
		for _, attr := range span.GetAttributes() {
			attrs[attr.GetKey()] = attr.GetValue()
		}
		if got := attrs["olp.evidence_origin"].GetStringValue(); got != "gateway_observed" {
			t.Errorf("%s origin = %q, want gateway_observed", name, got)
		}
		if got := attrs["gen_ai.response.model"].GetStringValue(); got != model {
			t.Errorf("%s response model = %q, want %q", name, got, model)
		}
		if got := attrs["gen_ai.usage.input_tokens"].GetIntValue(); got != inputTokens {
			t.Errorf("%s input tokens = %d, want %d", name, got, inputTokens)
		}
		if got := attrs["gen_ai.usage.output_tokens"].GetIntValue(); got != outputTokens {
			t.Errorf("%s output tokens = %d, want %d", name, got, outputTokens)
		}
		if got := attrs["gen_ai.response.finish_reasons"].GetArrayValue().GetValues(); len(got) != len(finishReasons) || got[0].GetStringValue() != finishReasons[0] {
			t.Errorf("%s finish reasons = %v, want %v", name, got, finishReasons)
		}
		if name == "attempt" {
			if got := attrs["olp.usage.cached_input_tokens"].GetIntValue(); got != cachedTokens {
				t.Errorf("%s cached input tokens = %d, want %d", name, got, cachedTokens)
			}
			if got := attrs["olp.usage.media_units"].GetStringValue(); got != mediaUnits {
				t.Errorf("%s media units = %q, want %q", name, got, mediaUnits)
			}
		}
	}
	if !bytes.Equal(spans["attempt"].GetParentSpanId(), spans["request"].GetSpanId()) {
		t.Error("attempt span is not parented to the request span")
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
