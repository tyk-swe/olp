package telemetry

import (
	"context"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const outcomeSuccess = "success"

type AttemptTrace struct {
	span              trace.Span
	ctx               context.Context
	propagateUpstream bool
	done              bool
}

func (t *RequestTrace) Attempt(ctx context.Context, providerKind, providerRevision, model string) (context.Context, *AttemptTrace) {
	attrs := []attribute.KeyValue{
		gatewayObserved,
		attribute.String("olp.provider_kind", providerKind),
		attribute.String("olp.provider_revision", providerRevision),
		attribute.String("olp.model", model),
	}
	if providerKind != "" {
		attrs = append(attrs, genAIProvider(providerKind))
	}
	if t == nil {
		actx, span := tracerFrom(noop.NewTracerProvider()).Start(ctx, "attempt")
		return actx, &AttemptTrace{span: span, ctx: actx}
	}
	attrs = append(attrs, t.genAIRequestAttributes()...)
	actx, span := tracerFrom(t.span.TracerProvider()).Start(trace.ContextWithSpan(ctx, t.span), "attempt",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return actx, &AttemptTrace{span: span, ctx: actx, propagateUpstream: t.propagateUpstream}
}

func (a *AttemptTrace) SpanContext() trace.SpanContext { return a.span.SpanContext() }

func (a *AttemptTrace) InjectUpstream(header http.Header) {
	if !a.propagateUpstream {
		return
	}
	propagation.TraceContext{}.Inject(a.ctx, propagation.HeaderCarrier(header))
}

func (a *AttemptTrace) RecordUsage(inputTokens, outputTokens, cachedInputTokens *int64, mediaUnits *string) {
	a.span.SetAttributes(responseAttributes("", inputTokens, outputTokens, nil)...)
	if inputTokens != nil {
		a.span.SetAttributes(attribute.Int64("olp.usage.input_tokens", *inputTokens))
	}
	if outputTokens != nil {
		a.span.SetAttributes(attribute.Int64("olp.usage.output_tokens", *outputTokens))
	}
	if cachedInputTokens != nil {
		a.span.SetAttributes(attribute.Int64("olp.usage.cached_input_tokens", *cachedInputTokens))
	}
	if mediaUnits != nil {
		a.span.SetAttributes(attribute.String("olp.usage.media_units", *mediaUnits))
	}
}

func (a *AttemptTrace) RecordResponse(model string, finishReasons []string) {
	a.span.SetAttributes(responseAttributes(model, nil, nil, finishReasons)...)
}

func (a *AttemptTrace) Finish(outcome string, upstreamStatus int) {
	if a.done {
		return
	}
	a.done = true
	a.span.SetAttributes(attribute.String("olp.outcome_class", outcome))
	if upstreamStatus > 0 {
		a.span.SetAttributes(attribute.String("olp.upstream_status_class", statusClass(upstreamStatus)))
	}
	if outcome == outcomeSuccess {
		a.span.SetStatus(codes.Ok, "")
	} else {
		a.span.SetStatus(codes.Error, outcome)
	}
	a.span.End()
}

func statusClass(code int) string {
	if code < 100 || code >= 600 {
		return "invalid"
	}
	return strconv.Itoa(code/100) + "xx"
}
