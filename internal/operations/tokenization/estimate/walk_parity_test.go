package estimate

import (
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// walkerFamilies is every request family the gateway can hand to admission.
// The walker must charge each as the old walker did, including the ones whose
// prompt it does not read.
var walkerFamilies = []wire.Format{
	wire.Chat, wire.Responses, wire.InputTokens,
	wire.Embeddings, wire.Moderation, wire.Rerank,
	wire.Anthropic, wire.AnthropicCount,
	wire.Gemini, wire.GeminiStream, wire.GeminiCount,
	wire.GeminiInteractions, wire.GeminiEmbeddings,
	wire.Bedrock, wire.BedrockEmbeddings, wire.BedrockInvoke,
	wire.VertexEmbeddings, wire.ImageGeneration, wire.Speech,
}
