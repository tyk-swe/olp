package connectors

// Supports is the reviewed connector matrix, distinct from a live model
// certification. Generic endpoints never acquire native cross-surface
// privileges by declaring a tuple or impersonating an official vendor.
// Media tuples mirror the reviewed matrix: image, audio, and video
// operations exist only on the OpenAI surface of the OpenAI connector
// families; no other connector may serve them.
func Supports(kind, vendor, operation, surface, mode string) bool {
	if surface != "openai" && surface != "anthropic" && surface != "gemini" {
		return false
	}
	if mode != "unary" && mode != "streaming" && mode != "async" {
		return false
	}
	if kind != "openai" && kind != "openai_compatible" && kind != "anthropic" && kind != "gemini" && kind != "vertex_ai" && kind != "azure_openai" && kind != "bedrock" {
		return false
	}
	if kind == "openai_compatible" && surface != "openai" {
		return false
	}
	switch vendor {
	case "voyage":
		if operation != "embeddings" {
			return false
		}
	case "cohere":
		if operation != "generation" && operation != "embeddings" {
			return false
		}
	case "deepseek", "fireworks", "deepinfra", "huggingface", "perplexity":
		if operation != "generation" {
			return false
		}
	}
	openaiFamily := kind == "openai" || kind == "azure_openai" || kind == "openai_compatible"
	switch operation {
	case "generation":
		return mode == "unary" || mode == "streaming"
	case "token_count":
		return mode == "unary"
	case "embeddings", "moderation", "image_variation":
		return surface == "openai" && mode == "unary" && openaiFamily
	case "image_generation", "image_edit", "speech", "transcription":
		return surface == "openai" && (mode == "unary" || mode == "streaming") && openaiFamily
	case "video_create":
		return surface == "openai" && mode == "async" && openaiFamily
	case "video_list", "video_get", "video_content", "video_delete":
		return surface == "openai" && mode == "unary" && openaiFamily
	}
	return false
}
