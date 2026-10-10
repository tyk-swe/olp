package telemetry

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	GenAISemconvVersion = "1.41.0"
	SchemaURL           = "https://opentelemetry.io/schemas/" + GenAISemconvVersion
)

// Span attribute allowlists. Every recorded attribute is metadata: no prompts,
// no file names, no provider payloads, and no header values ever appear on a
// span.
var RequestAttributeKeys = []string{
	"olp.request_id",
	"olp.surface",
	"olp.operation",
	"olp.route_slug",
	"olp.key_id",
	"olp.installation_id",
	"olp.generation",
	"olp.status",
	"olp.error_class",
	"olp.attempt_count",
	"olp.time_to_first_byte_ms",
	"olp.total_duration_ms",
	"olp.cancelled",
	"gen_ai.operation.name",
	"gen_ai.provider.name",
	"gen_ai.request.model",
	"gen_ai.response.model",
	"gen_ai.response.finish_reasons",
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
}

var AttemptAttributeKeys = []string{
	"olp.provider_kind",
	"olp.provider_revision",
	"olp.model",
	"olp.outcome_class",
	"olp.upstream_status_class",
	"olp.usage.input_tokens",
	"olp.usage.output_tokens",
	"olp.usage.cached_input_tokens",
	"olp.usage.media_units",
	"olp.pricing_provenance",
	"gen_ai.operation.name",
	"gen_ai.provider.name",
	"gen_ai.request.model",
	"gen_ai.response.model",
	"gen_ai.response.finish_reasons",
	"gen_ai.usage.input_tokens",
	"gen_ai.usage.output_tokens",
}

// ErrorClassClientCancelled is the error class a span records when the client
// went away before the response finished.
const ErrorClassClientCancelled = "client_cancelled"

func genAIOperation(operation, surface, family string) string {
	if operation != "generation" {
		return operation
	}
	switch {
	case surface == "gemini":
		return "generate_content"
	case family == "mistral_fim":
		return "text_completion"
	}
	return "chat"
}

func genAIProviderName(kind string) string {
	switch kind {
	case "anthropic":
		return "anthropic"
	case "azure_openai":
		return "azure.ai.openai"
	case "bedrock":
		return "aws.bedrock"
	case "gemini":
		return "gcp.gemini"
	case "openai":
		return "openai"
	case "vertex_ai":
		return "gcp.vertex_ai"
	case "watsonx":
		return "ibm.watsonx.ai"
	}
	return kind
}

type contextKey struct{}

// RequestTrace is the request span plus the propagation policy the request
// carries. When tracing is disabled the span is a no-op and every method is
// cheap.
type RequestTrace struct {
	span              trace.Span
	propagateUpstream bool
	recordRequest     bool
	operation         string
	requestModel      string
}

// RequestFromContext returns the trace a middleware installed, or a no-op one.
func RequestFromContext(ctx context.Context) *RequestTrace {
	if t, ok := ctx.Value(contextKey{}).(*RequestTrace); ok && t != nil {
		return t
	}
	return &RequestTrace{span: trace.SpanFromContext(ctx)}
}

func (t *RequestTrace) withContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, t)
}

// AttemptsOnly keeps propagation and attempt spans but stops request-level
// recording, for call sites that own the request span themselves.
func (t *RequestTrace) AttemptsOnly() *RequestTrace {
	return &RequestTrace{span: t.span, propagateUpstream: t.propagateUpstream, operation: t.operation, requestModel: t.requestModel}
}

// PropagateUpstream reports whether attempt spans inject their context into
// provider requests.
func (t *RequestTrace) PropagateUpstream() bool { return t != nil && t.propagateUpstream }

func (t *RequestTrace) SpanContext() trace.SpanContext {
	if t == nil {
		return trace.SpanContext{}
	}
	return t.span.SpanContext()
}

// Context returns a context carrying the request span, for attempt parenting.
func (t *RequestTrace) Context(ctx context.Context) context.Context {
	return trace.ContextWithSpan(ctx, t.span)
}

// RecordInferenceContext records the identity settled by authentication and
// route resolution.
func (t *RequestTrace) RecordInferenceContext(surface, operation, family, routeSlug, keyID, generation string) {
	if t == nil {
		return
	}
	t.RecordSessionContext(surface, operation, family, routeSlug, generation)
	if !t.recordRequest {
		return
	}
	t.span.SetAttributes(
		attribute.String("olp.surface", surface),
		attribute.String("olp.key_id", keyID),
	)
}

// RecordSessionContext records operation, route, and runtime generation.
func (t *RequestTrace) RecordSessionContext(surface, operation, family, routeSlug, generation string) {
	if t == nil {
		return
	}
	t.operation = genAIOperation(operation, surface, family)
	t.requestModel = routeSlug
	if !t.recordRequest {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("olp.operation", operation),
		attribute.String("olp.route_slug", routeSlug),
		attribute.String("olp.generation", generation),
	}
	if t.operation != "" {
		attrs = append(attrs, attribute.String("gen_ai.operation.name", t.operation))
	}
	if routeSlug != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", routeSlug))
	}
	t.span.SetAttributes(attrs...)
}

func (t *RequestTrace) RecordResponse(providerKind, model string, inputTokens, outputTokens *int64, finishReasons []string) {
	if t == nil || !t.recordRequest {
		return
	}
	if providerKind != "" {
		t.span.SetAttributes(attribute.String("gen_ai.provider.name", genAIProviderName(providerKind)))
	}
	if model != "" {
		t.span.SetAttributes(attribute.String("gen_ai.response.model", model))
	}
	if inputTokens != nil {
		t.span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", *inputTokens))
	}
	if outputTokens != nil {
		t.span.SetAttributes(attribute.Int64("gen_ai.usage.output_tokens", *outputTokens))
	}
	if len(finishReasons) != 0 {
		t.span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", finishReasons))
	}
}

// RecordTerminal records the request's terminal accounting: status, error
// class, attempt count, and the latency fields the writer did not already set.
func (t *RequestTrace) RecordTerminal(status int, errorClass string, attemptCount int, firstByte, total time.Duration) {
	if t == nil || !t.recordRequest {
		return
	}
	if status > 0 {
		t.span.SetAttributes(attribute.Int("olp.status", status))
	}
	if errorClass != "" {
		t.span.SetAttributes(attribute.String("olp.error_class", errorClass))
		if errorClass == ErrorClassClientCancelled {
			t.span.SetAttributes(attribute.Bool("olp.cancelled", true))
		}
	}
	t.span.SetAttributes(
		attribute.Int("olp.attempt_count", attemptCount),
		attribute.Int64("olp.total_duration_ms", total.Milliseconds()),
	)
	if firstByte > 0 {
		t.span.SetAttributes(attribute.Int64("olp.time_to_first_byte_ms", firstByte.Milliseconds()))
	}
}

// Attempt opens a client span for one upstream attempt, parented on the
// request span. The returned context carries the attempt span so propagation
// injection can parent on it.
type AttemptTrace struct {
	span trace.Span
	ctx  context.Context
	done bool
}

// Attempt starts an upstream attempt span. plan fields are metadata only.
func (t *RequestTrace) Attempt(ctx context.Context, providerKind, providerRevision, model string) (context.Context, *AttemptTrace) {
	var tracer trace.Tracer
	if t != nil && t.span != nil {
		tracer = t.span.TracerProvider().Tracer("openllmproxy", trace.WithSchemaURL(SchemaURL))
	} else {
		tracer = noopTracer()
	}
	parent := ctx
	if t != nil {
		parent = trace.ContextWithSpan(ctx, t.span)
	}
	attrs := []attribute.KeyValue{
		attribute.String("olp.provider_kind", providerKind),
		attribute.String("olp.provider_revision", providerRevision),
		attribute.String("olp.model", model),
	}
	if providerKind != "" {
		attrs = append(attrs, attribute.String("gen_ai.provider.name", genAIProviderName(providerKind)))
	}
	if t != nil {
		if t.operation != "" {
			attrs = append(attrs, attribute.String("gen_ai.operation.name", t.operation))
		}
		if t.requestModel != "" {
			attrs = append(attrs, attribute.String("gen_ai.request.model", t.requestModel))
		}
	}
	actx, span := tracer.Start(parent, "attempt",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return actx, &AttemptTrace{span: span, ctx: actx}
}

func noopTracer() trace.Tracer {
	return noop.NewTracerProvider().Tracer("openllmproxy", trace.WithSchemaURL(SchemaURL))
}

// Context returns the context carrying the attempt span.
func (a *AttemptTrace) Context() context.Context { return a.ctx }

// SpanContext returns the attempt span's context for header injection.
func (a *AttemptTrace) SpanContext() trace.SpanContext { return a.span.SpanContext() }

// RecordUsage records the accounting counters the attempt observed.
func (a *AttemptTrace) RecordUsage(inputTokens, outputTokens, cachedInputTokens *int64, mediaUnits *string) {
	if inputTokens != nil {
		a.span.SetAttributes(
			attribute.Int64("olp.usage.input_tokens", *inputTokens),
			attribute.Int64("gen_ai.usage.input_tokens", *inputTokens),
		)
	}
	if outputTokens != nil {
		a.span.SetAttributes(
			attribute.Int64("olp.usage.output_tokens", *outputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", *outputTokens),
		)
	}
	if cachedInputTokens != nil {
		a.span.SetAttributes(attribute.Int64("olp.usage.cached_input_tokens", *cachedInputTokens))
	}
	if mediaUnits != nil {
		a.span.SetAttributes(attribute.String("olp.usage.media_units", *mediaUnits))
	}
}

func (a *AttemptTrace) RecordResponse(model string, finishReasons []string) {
	if a == nil {
		return
	}
	if model != "" {
		a.span.SetAttributes(attribute.String("gen_ai.response.model", model))
	}
	if len(finishReasons) != 0 {
		a.span.SetAttributes(attribute.StringSlice("gen_ai.response.finish_reasons", finishReasons))
	}
}

// Finish records the outcome class and upstream status class and ends the
// span. An unfinished attempt ends as "cancelled" — it was dropped without a
// verdict.
func (a *AttemptTrace) Finish(outcome string, upstreamStatus int) {
	if a.done {
		return
	}
	a.done = true
	a.span.SetAttributes(attribute.String("olp.outcome_class", outcome))
	if upstreamStatus > 0 {
		a.span.SetAttributes(attribute.String("olp.upstream_status_class", statusClass(upstreamStatus)))
	}
	if outcome == "success" {
		a.span.SetStatus(codes.Ok, "")
	} else {
		a.span.SetStatus(codes.Error, outcome)
	}
	a.span.End()
}

func statusClass(code int) string {
	switch {
	case code >= 100 && code < 200:
		return "1xx"
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	}
	return "invalid"
}

// InjectUpstream writes the attempt span's W3C context into upstream headers
// when the request allows propagation.
func (a *AttemptTrace) InjectUpstream(header http.Header, propagate bool) {
	if a == nil || a.ctx == nil {
		return
	}
	Inject(a.ctx, header, propagate)
}
