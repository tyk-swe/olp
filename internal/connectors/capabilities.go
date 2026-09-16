package connectors

// Supports is the reviewed non-media connector matrix, distinct from a live
// model certification. Generic endpoints never acquire native cross-surface
// privileges by declaring a tuple or impersonating an official vendor.
func Supports(kind, vendor, operation, surface, mode string) bool {
	if surface != "openai" && surface != "anthropic" && surface != "gemini" {
		return false
	}
	if mode != "unary" && mode != "streaming" {
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
	switch operation {
	case "generation":
		return true
	case "token_count":
		return mode == "unary"
	case "embeddings", "moderation":
		return surface == "openai" && mode == "unary" && (kind == "openai" || kind == "azure_openai" || kind == "openai_compatible")
	}
	return false
}
