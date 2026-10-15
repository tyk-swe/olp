// Package wire owns shared request and result envelopes. Dialect codecs own validation and encoding.
package wire

// OperationGeneration is the gateway operation both codecs map to.
const OperationGeneration = "generation"

// Format selects the wire format of a generation request.
type Format string

const (
	Chat               Format = "chat"
	Responses          Format = "responses"
	InputTokens        Format = "input_tokens"
	Embeddings         Format = "embeddings"
	Moderation         Format = "moderation"
	Anthropic          Format = "anthropic"
	AnthropicCount     Format = "anthropic_count"
	Gemini             Format = "gemini"
	GeminiStream       Format = "gemini_stream"
	GeminiCount        Format = "gemini_count"
	GeminiInteractions Format = "gemini_interactions"
	GeminiLive         Format = "gemini_live"

	GeminiEmbeddings      Format = "gemini_embeddings"
	GeminiEmbeddingsBatch Format = "gemini_embeddings_batch"
	VertexEmbeddings      Format = "vertex_embeddings"
	BedrockEmbeddings     Format = "bedrock_embeddings"
	Rerank                Format = "rerank"
	BedrockRerank         Format = "bedrock_rerank"
	// Media families name their operation tag directly so the accounting
	// envelope and selection registry agree on the operation string.
	ImageGeneration Format = "image_generation"
	ImageEdit       Format = "image_edit"
	ImageVariation  Format = "image_variation"
	Speech          Format = "speech"
	Transcription   Format = "transcription"
	Translation     Format = "translation"
	VideoCreate     Format = "video_create"
	VideoList       Format = "video_list"
	VideoGet        Format = "video_get"
	VideoContent    Format = "video_content"
	VideoDelete     Format = "video_delete"

	File          Format = "file"
	Batch         Format = "batch"
	Realtime      Format = "realtime"
	Bedrock       Format = "bedrock"
	BedrockInvoke Format = "bedrock_invoke"

	// Native generation families are vendor dialects a client speaks as
	// they are, at /native/{dialect}/models/{route}: OLP binds the route's
	// model and meters the result, and never translates them.
	MistralFIM Format = "mistral_fim"
	CohereChat Format = "cohere_chat"
)

func (f Format) Operation() string {
	switch f {
	case InputTokens, AnthropicCount, GeminiCount:
		return "token_count"
	case Embeddings, GeminiEmbeddings, GeminiEmbeddingsBatch,
		VertexEmbeddings, BedrockEmbeddings:
		return "embeddings"
	case Moderation:
		return "moderation"
	case Rerank, BedrockRerank:
		return "rerank"
	case ImageGeneration, ImageEdit, ImageVariation, Speech,
		Transcription, Translation, VideoCreate, VideoList, VideoGet,
		VideoContent, VideoDelete, File, Batch,
		Realtime, GeminiLive, BedrockInvoke:
		if f == GeminiLive {
			return "realtime"
		}
		return string(f)
	}
	return OperationGeneration
}

func (f Format) Surface() string {
	switch f {
	case Anthropic, AnthropicCount:
		return "anthropic"
	case Gemini, GeminiStream, GeminiCount, GeminiInteractions, GeminiLive:
		return "gemini"
	case Bedrock, BedrockInvoke:
		return "bedrock"
	case MistralFIM, CohereChat:
		return "native"
	}
	return "openai"
}
