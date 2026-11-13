package connectors

import "fmt"

type BatchAdapter string

const (
	BatchOpenAI        BatchAdapter = "openai"
	BatchTogether      BatchAdapter = "together"
	BatchMistral       BatchAdapter = "mistral"
	BatchGeminiManaged BatchAdapter = "gemini_managed"
	BatchAnthropic     BatchAdapter = "anthropic"
	BatchGemini        BatchAdapter = "gemini"
	BatchVertex        BatchAdapter = "vertex"
	BatchBedrock       BatchAdapter = "bedrock"
)

func ParseBatchAdapter(value string) (BatchAdapter, error) {
	switch adapter := BatchAdapter(value); adapter {
	case BatchOpenAI, BatchTogether, BatchMistral, BatchGeminiManaged, BatchAnthropic, BatchGemini, BatchVertex, BatchBedrock:
		return adapter, nil
	default:
		return "", fmt.Errorf("unknown batch adapter %q", value)
	}
}

func (a BatchAdapter) Managed() bool {
	return a == BatchTogether || a == BatchMistral || a == BatchGeminiManaged
}

func (c Config) FilesBatchesAdapter() (BatchAdapter, bool) {
	if !c.SupportsFilesBatches() {
		return "", false
	}
	if c.Kind == KindGemini {
		return BatchGeminiManaged, true
	}
	if service, ok := c.filesBatchesService(); ok {
		return service.adapter, true
	}
	return BatchOpenAI, true
}

const (
	VertexBatchProfile  = "vertex-batch-prediction"
	BedrockBatchProfile = "bedrock-batch-inference"
)

func CloudBatchAdapter(profileID string) (BatchAdapter, bool) {
	switch profileID {
	case VertexBatchProfile:
		return BatchVertex, true
	case BedrockBatchProfile:
		return BatchBedrock, true
	default:
		return "", false
	}
}

func (a BatchAdapter) CloudDialect() string {
	switch a {
	case BatchVertex:
		return VertexBatchProfile
	case BatchBedrock:
		return BedrockBatchProfile
	default:
		return ""
	}
}
