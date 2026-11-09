package connectors

import (
	"fmt"
	"strings"
)

// SupportsRetainedResponses identifies the native resource-addressing contracts
// implemented by the existing Responses resource owner. A compatible JSON
// generation dialect alone does not establish provider resource lifecycle APIs.
func (c Config) SupportsRetainedResponses() bool {
	switch c.Kind {
	case "azure_openai":
		return true
	case "openai":
		endpoint := strings.TrimSuffix(strings.TrimSpace(c.Endpoint), "/")
		return endpoint == "" || endpoint == "https://api.openai.com/v1"
	default:
		return false
	}
}

// SupportsFilesBatches identifies provider configurations with qualified
// owner-scoped file and batch lifecycle endpoints.
func (c Config) SupportsFilesBatches() bool {
	switch c.Kind {
	case "azure_openai":
		return true
	case "openai":
		return c.SupportsRetainedResponses()
	case "openai_compatible":
		if c.ProfileRevision != ProfileRevision {
			return false
		}
		switch c.ProfileID {
		case "groq-files-batches":
			return c.VendorID == "groq"
		case "together-files-batches":
			return c.VendorID == "together"
		case "mistral-files-batches":
			return c.VendorID == "mistral"
		}
	case "gemini":
		if c.ProfileID == "" {
			return c.ProfileRevision == "" && c.Hosting() == ""
		}
		return c.ProfileID == "gemini-generation" && c.ProfileRevision == ProfileRevision && c.Hosting() == "direct-gemini"
	}
	return false
}

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
	if c.Kind == "gemini" {
		return BatchGeminiManaged, true
	}
	switch c.ProfileID {
	case "together-files-batches":
		return BatchTogether, true
	case "mistral-files-batches":
		return BatchMistral, true
	default:
		return BatchOpenAI, true
	}
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
