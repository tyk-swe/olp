// Package wire owns shared request and result envelopes. Dialect codecs own validation and encoding.
package wire

// OperationGeneration is the gateway operation both codecs map to.
const OperationGeneration = "generation"

// Format selects the wire format of a generation request.
type Format string

const (
	Chat               Format = "chat"
	Completions        Format = "completions"
	Decisions          Format = "decisions"
	SystemOne          Format = "systemone"
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
	OCR                   Format = "ocr"
	DocumentAIProcess     Format = "documentai_process"
	Search                Format = "search"
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

	File           Format = "file"
	Batch          Format = "batch"
	AnthropicFile  Format = "anthropic_file"
	AnthropicBatch Format = "anthropic_batch"
	GeminiFile     Format = "gemini_file"
	GeminiBatch    Format = "gemini_batch"
	CloudBatch     Format = "cloud_batch"
	Realtime       Format = "realtime"
	NativeDuplex   Format = "native_duplex"
	Bedrock        Format = "bedrock"
	BedrockInvoke  Format = "bedrock_invoke"

	// Native generation families are vendor dialects a client speaks as
	// they are, at /native/{dialect}/models/{route}: OLP binds the route's
	// model and meters the result, and never translates them.
	MistralFIM  Format = "mistral_fim"
	CohereChat  Format = "cohere_chat"
	Passthrough Format = "passthrough"
)

func (f Format) Operation() string {
	switch f {
	case Decisions, SystemOne:
		return "decision"
	case InputTokens, AnthropicCount, GeminiCount:
		return "token_count"
	case Embeddings, GeminiEmbeddings, GeminiEmbeddingsBatch,
		VertexEmbeddings, BedrockEmbeddings:
		return "embeddings"
	case Moderation:
		return "moderation"
	case Rerank, BedrockRerank:
		return "rerank"
	case DocumentAIProcess:
		return "ocr"
	case AnthropicFile, GeminiFile:
		return "file"
	case AnthropicBatch, GeminiBatch, CloudBatch:
		return "batch"
	case ImageGeneration, ImageEdit, ImageVariation, Speech,
		Transcription, Translation, VideoCreate, VideoList, VideoGet,
		VideoContent, VideoDelete, File, Batch,
		Realtime, GeminiLive, NativeDuplex, BedrockInvoke, OCR, Search, Passthrough:
		if f == GeminiLive || f == NativeDuplex {
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
	case AnthropicFile, AnthropicBatch:
		return "anthropic"
	case Gemini, GeminiStream, GeminiCount, GeminiInteractions, GeminiLive:
		return "gemini"
	case GeminiFile, GeminiBatch:
		return "gemini"
	case Bedrock, BedrockInvoke:
		return "bedrock"
	case MistralFIM, CohereChat, CloudBatch, NativeDuplex, Passthrough:
		return "native"
	}
	return "openai"
}
