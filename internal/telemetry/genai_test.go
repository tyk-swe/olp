package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestGenAIOperationMapping(t *testing.T) {
	for _, tc := range []struct {
		operation, surface, family, want string
	}{
		{"generation", "openai", "chat", "chat"},
		{"generation", "openai", "responses", "chat"},
		{"generation", "anthropic", "anthropic", "chat"},
		{"generation", "bedrock", "bedrock", "chat"},
		{"generation", "gemini", "gemini", "generate_content"},
		{"generation", "native", "mistral_fim", "text_completion"},
		{"generation", "native", "cohere_chat", "chat"},
		{"embeddings", "openai", "embeddings", "embeddings"},
		{"rerank", "openai", "rerank", "rerank"},
		{"moderation", "openai", "moderation", "moderation"},
		{"token_count", "openai", "input_tokens", "token_count"},
	} {
		if got := genAIOperation(tc.operation, tc.surface, tc.family); got != tc.want {
			t.Errorf("genAIOperation(%q, %q, %q) = %q, want %q", tc.operation, tc.surface, tc.family, got, tc.want)
		}
	}
}

func TestGenAIProviderNameMapping(t *testing.T) {
	for kind, want := range map[string]string{
		"openai":            "openai",
		"anthropic":         "anthropic",
		"gemini":            "gcp.gemini",
		"bedrock":           "aws.bedrock",
		"azure_openai":      "azure.ai.openai",
		"vertex_ai":         "gcp.vertex_ai",
		"watsonx":           "ibm.watsonx.ai",
		"openai_compatible": "openai_compatible",
		"sagemaker":         "sagemaker",
		"plugin":            "plugin",
	} {
		if got := genAIProviderName(kind); got != want {
			t.Errorf("genAIProviderName(%q) = %q, want %q", kind, got, want)
		}
	}
}

func spanAttributeMap(span tracetest.SpanStub) map[string]attribute.Value {
	attrs := map[string]attribute.Value{}
	for _, kv := range span.Attributes {
		attrs[string(kv.Key)] = kv.Value
	}
	return attrs
}

func TestGenAISpanAttributesAndSchemaURL(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	tracer := provider.Tracer("openllmproxy", trace.WithSchemaURL(SchemaURL))
	handler := AdmittedRequest(RequestConfig{InstallationID: "installation"}, tracer, "openai", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := RequestFromContext(r.Context())
		request.RecordInferenceContext("openai", "generation", "chat", "safe-route", "safe-key-id", "generation")
		_, attempt := request.Attempt(r.Context(), "gemini", "revision", "model")
		input, output := int64(7), int64(11)
		attempt.RecordUsage(&input, &output, nil, nil)
		attempt.RecordResponse("gemini-observed", []string{"stop"})
		attempt.Finish("success", 200)
		_, omitted := request.Attempt(r.Context(), "bedrock", "revision", "model")
		omitted.RecordResponse("", nil)
		omitted.Finish("success", 200)
		request.RecordResponse("gemini", "gemini-observed", &input, &output, []string{"stop"})
		request.RecordTerminal(200, "", 2, time.Millisecond, 2*time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	var attempts []tracetest.SpanStub
	var requestSpan *tracetest.SpanStub
	for i, span := range spans {
		switch span.Name {
		case "request":
			requestSpan = &spans[i]
		case "attempt":
			attempts = append(attempts, span)
		}
		if span.InstrumentationScope.SchemaURL != SchemaURL {
			t.Fatalf("span %s schema %q", span.Name, span.InstrumentationScope.SchemaURL)
		}
	}
	if requestSpan == nil || len(attempts) != 2 {
		t.Fatalf("spans %v", spans)
	}
	attrs := spanAttributeMap(*requestSpan)
	if got := attrs["gen_ai.operation.name"].AsString(); got != "chat" {
		t.Fatalf("request operation %q", got)
	}
	if got := attrs["gen_ai.request.model"].AsString(); got != "safe-route" {
		t.Fatalf("request model %q", got)
	}
	if got := attrs["gen_ai.provider.name"].AsString(); got != "gcp.gemini" {
		t.Fatalf("request provider %q", got)
	}
	if got := attrs["gen_ai.response.model"].AsString(); got != "gemini-observed" {
		t.Fatalf("request response model %q", got)
	}
	if got := attrs["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("request finish reasons %v", got)
	}
	if got := attrs["gen_ai.usage.input_tokens"].AsInt64(); got != 7 {
		t.Fatalf("request input tokens %d", got)
	}
	first := spanAttributeMap(attempts[0])
	if got := first["gen_ai.provider.name"].AsString(); got != "gcp.gemini" {
		t.Fatalf("attempt provider %q", got)
	}
	if got := first["gen_ai.operation.name"].AsString(); got != "chat" {
		t.Fatalf("attempt operation %q", got)
	}
	if got := first["gen_ai.request.model"].AsString(); got != "safe-route" {
		t.Fatalf("attempt request model %q", got)
	}
	if got := first["gen_ai.response.model"].AsString(); got != "gemini-observed" {
		t.Fatalf("attempt response model %q", got)
	}
	if got := first["gen_ai.usage.output_tokens"].AsInt64(); got != 11 {
		t.Fatalf("attempt output tokens %d", got)
	}
	if got := first["olp.usage.input_tokens"].AsInt64(); got != 7 {
		t.Fatalf("attempt olp input tokens %d", got)
	}
	second := spanAttributeMap(attempts[1])
	if got := second["gen_ai.provider.name"].AsString(); got != "aws.bedrock" {
		t.Fatalf("second attempt provider %q", got)
	}
	for _, key := range []string{"gen_ai.response.model", "gen_ai.response.finish_reasons", "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens"} {
		if _, ok := second[key]; ok {
			t.Fatalf("unobserved %s recorded on second attempt", key)
		}
	}
}
