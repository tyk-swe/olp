package wire

import "github.com/tyk-swe/olp/internal/oif"

// Descriptor adapts family names to independent operation/dialect identities.
// Wire-v1 describes the checked-in codec contract, not a claim that a
// provider's moving API or model alias is immutable. Format descriptors carry
// the transformed codec profile until a provider profile binds the destination.
var wireDialects = map[Format]string{
	Completions: "openai-completions", Chat: "openai-chat", Responses: "openai-responses", InputTokens: "openai-input-tokens",
	Anthropic: "anthropic-messages", AnthropicCount: "anthropic-count-tokens",
	Gemini: "gemini-generate-content", GeminiStream: "gemini-generate-content", GeminiCount: "gemini-count-tokens",
	Embeddings: "openai-embeddings", Moderation: "openai-moderation", Rerank: "rerank", BedrockRerank: "bedrock-rerank",
	GeminiEmbeddings: "gemini-embeddings", GeminiEmbeddingsBatch: "gemini-embeddings-batch",
	VertexEmbeddings: "vertex-embeddings", BedrockEmbeddings: "bedrock-embeddings",
	Bedrock: "bedrock-converse", "bedrock_count": "bedrock-count-tokens",
	MistralFIM: "mistral-fim", CohereChat: "cohere-chat-v2",
}

func Descriptor(family Format, stream bool) oif.Descriptor {
	dialect := wireDialects[family]
	if dialect == "" {
		dialect = string(family)
	}
	delivery := "unary"
	if stream {
		delivery = "incremental"
	}
	operation := family.Operation()
	if family == "bedrock_count" {
		operation = "token_count"
	}
	return oif.Descriptor{Operation: oif.Identity{ID: operation, Revision: "1"}, Dialect: oif.Identity{ID: dialect, Revision: "wire-v1"}, Profile: oif.Identity{ID: "transformed", Revision: "1"}, Execution: oif.Execution{Delivery: delivery, Lifetime: "request", Submission: "immediate", Effects: []string{"inference"}}}
}
