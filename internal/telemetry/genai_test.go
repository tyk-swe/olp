package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func spanAttributeMap(span tracetest.SpanStub) map[string]attribute.Value {
	attrs := map[string]attribute.Value{}
	for _, kv := range span.Attributes {
		attrs[string(kv.Key)] = kv.Value
	}
	return attrs
}
