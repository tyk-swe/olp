package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	attrStatus                = attribute.Key("olp.status")
	attrFirstByteMS           = attribute.Key("olp.time_to_first_byte_ms")
	attrTotalMS               = attribute.Key("olp.total_duration_ms")
	attrErrorClass            = attribute.Key("olp.error_class")
	errorClassClientCancelled = "client_cancelled"
)

var gatewayObserved = attribute.String("olp.evidence_origin", "gateway_observed")

func tracerFrom(provider trace.TracerProvider) trace.Tracer {
	return provider.Tracer("openllmproxy", trace.WithSchemaURL(semconv.SchemaURL))
}

func errorClassAttributes(errorClass string) []attribute.KeyValue {
	if errorClass == "" {
		return nil
	}
	if errorClass == errorClassClientCancelled {
		return []attribute.KeyValue{attrErrorClass.String(errorClass), attribute.Bool("olp.cancelled", true)}
	}
	return []attribute.KeyValue{attrErrorClass.String(errorClass)}
}

func genAIOperation(operation, surface, family string) attribute.KeyValue {
	switch {
	case operation == "":
		return attribute.KeyValue{}
	case operation != "generation":
		return semconv.GenAIOperationNameKey.String(operation)
	case surface == "gemini":
		return semconv.GenAIOperationNameGenerateContent
	case family == "mistral_fim":
		return semconv.GenAIOperationNameTextCompletion
	}
	return semconv.GenAIOperationNameChat
}

func genAIProvider(kind string) attribute.KeyValue {
	switch kind {
	case "anthropic":
		return semconv.GenAIProviderNameAnthropic
	case "azure_openai":
		return semconv.GenAIProviderNameAzureAIOpenAI
	case "bedrock":
		return semconv.GenAIProviderNameAWSBedrock
	case "gemini":
		return semconv.GenAIProviderNameGCPGemini
	case "openai":
		return semconv.GenAIProviderNameOpenAI
	case "vertex_ai":
		return semconv.GenAIProviderNameGCPVertexAI
	case "watsonx":
		return semconv.GenAIProviderNameIBMWatsonxAI
	}
	return semconv.GenAIProviderNameKey.String(kind)
}

func responseAttributes(model string, inputTokens, outputTokens *int64, finishReasons []string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	if model != "" {
		attrs = append(attrs, semconv.GenAIResponseModelKey.String(model))
	}
	if inputTokens != nil {
		attrs = append(attrs, semconv.GenAIUsageInputTokensKey.Int64(*inputTokens))
	}
	if outputTokens != nil {
		attrs = append(attrs, semconv.GenAIUsageOutputTokensKey.Int64(*outputTokens))
	}
	if len(finishReasons) != 0 {
		attrs = append(attrs, semconv.GenAIResponseFinishReasonsKey.StringSlice(finishReasons))
	}
	return attrs
}
