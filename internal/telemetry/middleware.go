package telemetry

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var canonicalRequestID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// tracedWriter records the response's terminal accounting on the request
// span: status, first-byte latency, total duration, and client cancellation.
type tracedWriter struct {
	http.ResponseWriter
	span      trace.Span
	started   time.Time
	status    int
	firstByte bool
	finished  bool
}

func (w *tracedWriter) recordFirstByte() {
	if w.firstByte {
		return
	}
	w.firstByte = true
	w.span.SetAttributes(attribute.Int64("olp.time_to_first_byte_ms", time.Since(w.started).Milliseconds()))
}

func (w *tracedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.span.SetAttributes(attribute.Int("olp.status", status))
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *tracedWriter) Write(p []byte) (int, error) {
	w.recordFirstByte()
	return w.ResponseWriter.Write(p)
}

func (w *tracedWriter) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *tracedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish records the terminal fields once the handler returns.
func (w *tracedWriter) finish(cancelled bool) {
	if w.finished {
		return
	}
	w.finished = true
	if !w.firstByte && w.status != 0 {
		w.recordFirstByte()
	}
	w.span.SetAttributes(attribute.Int64("olp.total_duration_ms", time.Since(w.started).Milliseconds()))
	if cancelled {
		w.span.SetAttributes(
			attribute.String("olp.error_class", ErrorClassClientCancelled),
			attribute.Bool("olp.cancelled", true),
		)
	}
}

// AdmittedRequest wraps one admitted public request in a server span. The span
// records only allowlisted metadata; the request context carries the
// RequestTrace the gateway uses for context, terminal fields, and attempt
// spans. surface is the admission classification; the handler records the
// operation once routing settles it.
func AdmittedRequest(cfg RequestConfig, tracer trace.Tracer, surface string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parent := r.Context()
		if cfg.AcceptInbound && r.Header.Get("traceparent") != "" {
			parent = extractRemoteParent(r)
		}
		started := time.Now()
		attrs := []attribute.KeyValue{
			attribute.String("olp.installation_id", cfg.InstallationID),
			attribute.String("olp.surface", surface),
		}
		if surface == "management" {
			attrs = append(attrs, attribute.String("olp.operation", "management"))
		}
		if id := r.Header.Get("X-Request-Id"); canonicalRequestID.MatchString(id) {
			attrs = append(attrs, attribute.String("olp.request_id", id))
		}
		ctx, span := tracer.Start(parent, "request",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attrs...),
		)
		request := &RequestTrace{span: span, propagateUpstream: cfg.PropagateUpstream, recordRequest: true}
		ctx = request.withContext(ctx)
		writer := &tracedWriter{ResponseWriter: w, span: span, started: started}
		next.ServeHTTP(writer, r.WithContext(ctx))
		writer.finish(r.Context().Err() != nil)
		span.End()
	})
}

// extractRemoteParent parses the inbound W3C context into a remote parent. The
// caller-supplied tracestate is deliberately dropped: it is an opaque bag a
// caller could use to smuggle data into our traces.
func extractRemoteParent(r *http.Request) context.Context {
	extracted := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	remote := trace.SpanContextFromContext(extracted)
	if !remote.IsValid() {
		return r.Context()
	}
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    remote.TraceID(),
		SpanID:     remote.SpanID(),
		TraceFlags: remote.TraceFlags(),
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(r.Context(), parent)
}
