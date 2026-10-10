package gateway

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func tracedHarness(t *testing.T) (*harness, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(t.Context()) })
	h := newHarnessWrapped(t, Config{}, func(next http.Handler) http.Handler {
		return telemetry.AdmittedRequest(telemetry.RequestConfig{InstallationID: "test"}, provider.Tracer("openllmproxy", trace.WithSchemaURL(telemetry.SchemaURL)), "openai", next)
	})
	return h, exporter
}

func spanAttrs(t *testing.T, exporter *tracetest.InMemoryExporter, name string) map[string]attribute.Value {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, span := range exporter.GetSpans() {
			if span.Name == name {
				attrs := map[string]attribute.Value{}
				for _, kv := range span.Attributes {
					attrs[string(kv.Key)] = kv.Value
				}
				if span.InstrumentationScope.SchemaURL != telemetry.SchemaURL {
					t.Fatalf("%s scope schema %q", name, span.InstrumentationScope.SchemaURL)
				}
				return attrs
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("span %q never exported", name)
		}
		time.Sleep(time.Millisecond)
	}
}

func checkGenAISpans(t *testing.T, exporter *tracetest.InMemoryExporter, responseModel string, finishReason string, input, output int64) {
	t.Helper()
	attempt := spanAttrs(t, exporter, "attempt")
	if got := attempt["gen_ai.provider.name"].AsString(); got != "openai_compatible" {
		t.Fatalf("attempt provider %q", got)
	}
	if got := attempt["gen_ai.operation.name"].AsString(); got != "chat" {
		t.Fatalf("attempt operation %q", got)
	}
	if got := attempt["gen_ai.request.model"].AsString(); got != routeSlug {
		t.Fatalf("attempt request model %q", got)
	}
	if got := attempt["gen_ai.response.model"].AsString(); got != responseModel {
		t.Fatalf("attempt response model %q", got)
	}
	if got := attempt["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 1 || got[0] != finishReason {
		t.Fatalf("attempt finish reasons %v", got)
	}
	if got := attempt["gen_ai.usage.input_tokens"].AsInt64(); got != input {
		t.Fatalf("attempt input tokens %d", got)
	}
	if got := attempt["gen_ai.usage.output_tokens"].AsInt64(); got != output {
		t.Fatalf("attempt output tokens %d", got)
	}
	if got := attempt["olp.model"].AsString(); got != modelA {
		t.Fatalf("attempt upstream model %q", got)
	}
	if got := attempt["olp.usage.input_tokens"].AsInt64(); got != input {
		t.Fatalf("attempt olp input tokens %d", got)
	}
	request := spanAttrs(t, exporter, "request")
	if got := request["gen_ai.operation.name"].AsString(); got != "chat" {
		t.Fatalf("request operation %q", got)
	}
	if got := request["gen_ai.request.model"].AsString(); got != routeSlug {
		t.Fatalf("request model %q", got)
	}
	if got := request["gen_ai.provider.name"].AsString(); got != "openai_compatible" {
		t.Fatalf("request provider %q", got)
	}
	if got := request["gen_ai.response.model"].AsString(); got != responseModel {
		t.Fatalf("request response model %q", got)
	}
	if got := request["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 1 || got[0] != finishReason {
		t.Fatalf("request finish reasons %v", got)
	}
	if got := request["gen_ai.usage.input_tokens"].AsInt64(); got != input {
		t.Fatalf("request input tokens %d", got)
	}
	if got := request["gen_ai.usage.output_tokens"].AsInt64(); got != output {
		t.Fatalf("request output tokens %d", got)
	}
	for _, span := range exporter.GetSpans() {
		for _, kv := range span.Attributes {
			if strings.Contains(kv.Value.AsString(), answerText) || strings.Contains(kv.Value.AsString(), "hi") {
				t.Fatalf("span %s carries content in %s", span.Name, kv.Key)
			}
		}
	}
}

func TestTracedUnaryCompletionRecordsGenAIAttributes(t *testing.T) {
	h, exporter := tracedHarness(t)
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	resp.Body.Close()
	h.sink.last(t)
	checkGenAISpans(t, exporter, modelA, "stop", 2, 3)
}

func TestTracedStreamCompletionRecordsGenAIAttributes(t *testing.T) {
	h, exporter := tracedHarness(t)
	data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if err := testutil.Stream(w, r, data, 1, 0); err != nil {
			t.Error(err)
		}
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
	}
	h.sink.last(t)
	checkGenAISpans(t, exporter, "gpt-fixture", "stop", 3, 2)
}

func TestTracedResponseUnknownOmitsGenAIResponseAttributes(t *testing.T) {
	h, exporter := tracedHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	resp.Body.Close()
	h.sink.last(t)
	for _, name := range []string{"attempt", "request"} {
		attrs := spanAttrs(t, exporter, name)
		for _, key := range []string{"gen_ai.response.model", "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
			if _, ok := attrs[key]; ok {
				t.Fatalf("%s span set %s without observed data", name, key)
			}
		}
		if got := attrs["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 1 || got[0] != "stop" {
			t.Fatalf("%s finish reasons %v", name, got)
		}
	}
}
