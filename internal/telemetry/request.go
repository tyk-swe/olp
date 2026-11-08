package telemetry

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/tyk-swe/olp/internal/surface"
)

var canonicalRequestID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type contextKey struct{}

// RequestTrace methods accept a nil receiver because internal calls (probes,
// shadows, classifiers) run without a request span. owned is false when the
// span came from someone else's context, so request-level fields stay off it.
type RequestTrace struct {
	span              trace.Span
	owned             bool
	propagateUpstream bool
	operation         attribute.KeyValue
	requestModel      string
}

func RequestFromContext(ctx context.Context) *RequestTrace {
	if t, ok := ctx.Value(contextKey{}).(*RequestTrace); ok && t != nil {
		return t
	}
	return &RequestTrace{span: trace.SpanFromContext(ctx)}
}

func (t *RequestTrace) PropagateUpstream() bool { return t != nil && t.propagateUpstream }

func (t *RequestTrace) SpanContext() trace.SpanContext {
	if t == nil {
		return trace.SpanContext{}
	}
	return t.span.SpanContext()
}

func (t *RequestTrace) record(attrs ...attribute.KeyValue) {
	if t == nil || !t.owned {
		return
	}
	t.span.SetAttributes(attrs...)
}

func (t *RequestTrace) RecordInferenceContext(surface, operation, family, routeSlug, keyID, generation string) {
	t.RecordSessionContext(surface, operation, family, routeSlug, generation)
	t.record(attribute.String("olp.surface", surface), attribute.String("olp.key_id", keyID))
}

func (t *RequestTrace) RecordSessionContext(surface, operation, family, routeSlug, generation string) {
	if t == nil {
		return
	}
	t.operation = genAIOperation(operation, surface, family)
	t.requestModel = routeSlug
	t.record(
		attribute.String("olp.operation", operation),
		attribute.String("olp.route_slug", routeSlug),
		attribute.String("olp.generation", generation),
	)
	t.record(t.genAIRequestAttributes()...)
}

func (t *RequestTrace) genAIRequestAttributes() []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	if t.operation.Valid() {
		attrs = append(attrs, t.operation)
	}
	if t.requestModel != "" {
		attrs = append(attrs, semconv.GenAIRequestModelKey.String(t.requestModel))
	}
	return attrs
}

func (t *RequestTrace) RecordResponse(providerKind, model string, inputTokens, outputTokens *int64, finishReasons []string) {
	if providerKind != "" {
		t.record(genAIProvider(providerKind))
	}
	t.record(responseAttributes(model, inputTokens, outputTokens, finishReasons)...)
}

// RecordTerminal overrides the status and latencies the writer recorded,
// because the gateway knows the attempt-level values the writer cannot see.
func (t *RequestTrace) RecordTerminal(status int, errorClass string, attemptCount int, firstByte, total time.Duration) {
	if status > 0 {
		t.record(attrStatus.Int(status))
	}
	if firstByte > 0 {
		t.record(attrFirstByteMS.Int64(firstByte.Milliseconds()))
	}
	t.record(errorClassAttributes(errorClass)...)
	t.record(attribute.Int("olp.attempt_count", attemptCount), attrTotalMS.Int64(total.Milliseconds()))
}

func admittedRequest(tracer trace.Tracer, installationID string, propagation Propagation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parent := r.Context()
		if propagation.AcceptInbound && r.Header.Get("traceparent") != "" {
			parent = extractRemoteParent(r)
		}
		classified := surface.Of(r.URL.Path)
		attrs := []attribute.KeyValue{
			gatewayObserved,
			attribute.String("olp.installation_id", installationID),
			attribute.String("olp.surface", classified.Name),
		}
		if !classified.Inference {
			attrs = append(attrs, attribute.String("olp.operation", "management"))
		}
		if id := r.Header.Get("X-Request-Id"); canonicalRequestID.MatchString(id) {
			attrs = append(attrs, attribute.String("olp.request_id", id))
		}
		ctx, span := tracer.Start(parent, "request", trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		defer span.End()
		request := &RequestTrace{span: span, owned: true, propagateUpstream: propagation.Upstream}
		writer := &tracedWriter{ResponseWriter: w, span: span, started: time.Now()}
		next.ServeHTTP(writer, r.WithContext(context.WithValue(ctx, contextKey{}, request)))
		writer.finish(r.Context().Err() != nil)
	})
}

// Caller tracestate is dropped because it is an opaque bag a caller could use
// to smuggle data into our traces.
func extractRemoteParent(r *http.Request) context.Context {
	extracted := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	remote := trace.SpanContextFromContext(extracted)
	if !remote.IsValid() {
		return r.Context()
	}
	return trace.ContextWithRemoteSpanContext(r.Context(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    remote.TraceID(),
		SpanID:     remote.SpanID(),
		TraceFlags: remote.TraceFlags(),
		Remote:     true,
	}))
}

type tracedWriter struct {
	http.ResponseWriter
	span      trace.Span
	started   time.Time
	status    int
	firstByte bool
}

func (w *tracedWriter) recordFirstByte() {
	if w.firstByte {
		return
	}
	w.firstByte = true
	w.span.SetAttributes(attrFirstByteMS.Int64(time.Since(w.started).Milliseconds()))
}

func (w *tracedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.span.SetAttributes(attrStatus.Int(status))
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *tracedWriter) Write(p []byte) (int, error) {
	w.recordFirstByte()
	return w.ResponseWriter.Write(p)
}

func (w *tracedWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func (w *tracedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *tracedWriter) finish(cancelled bool) {
	if w.status != 0 {
		w.recordFirstByte()
	}
	w.span.SetAttributes(attrTotalMS.Int64(time.Since(w.started).Milliseconds()))
	if cancelled {
		w.span.SetAttributes(errorClassAttributes(errorClassClientCancelled)...)
	}
}
