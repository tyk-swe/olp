package connectors

import "github.com/tyk-swe/olp/internal/vendors"

// Supports is the reviewed connector matrix, distinct from a live model
// certification. Generic endpoints never acquire native cross-surface
// privileges by declaring a tuple or impersonating an official vendor. A
// vendor with a reviewed contract is limited to the operations it lists.
// Media tuples mirror the reviewed matrix: image, audio, and video
// operations exist only on the OpenAI surface of the OpenAI connector
// families; no other connector may serve them.
func Supports(kind, vendor, operation, surface, mode string) bool {
	if surface != "openai" && surface != "anthropic" && surface != "gemini" && surface != "bedrock" {
		return false
	}
	if mode != "unary" && mode != "streaming" && mode != "async" && mode != "realtime" {
		return false
	}
	if kind == KindPlugin {
		// A plugin profile serves generation in its dialect, which other
		// surfaces reach through translation.
		return operation == "generation" && surface != "bedrock" && (mode == "unary" || mode == "streaming")
	}
	if kind == KindSageMaker {
		// An endpoint's container serves chat completions; like any generic
		// OpenAI-compatible server, it gains no native surface of its own.
		return operation == "generation" && surface == "openai" && (mode == "unary" || mode == "streaming") && vendors.Serves(vendor, operation)
	}
	if kind == KindWatsonx {
		// The chat API serves generation, which other surfaces reach
		// through translation.
		return operation == "generation" && surface != "bedrock" && (mode == "unary" || mode == "streaming") && vendors.Serves(vendor, operation)
	}
	if kind != "openai" && kind != "openai_compatible" && kind != "anthropic" && kind != "gemini" && kind != "vertex_ai" && kind != "azure_openai" && kind != "bedrock" {
		return false
	}
	if kind == "openai_compatible" && surface != "openai" {
		return false
	}
	if surface == "bedrock" && kind != "bedrock" {
		return false
	}
	if mode == "realtime" && (surface != "openai" || operation != "realtime") {
		return false
	}
	if !vendors.Serves(vendor, operation) {
		return false
	}
	openaiFamily := kind == "openai" || kind == "azure_openai" || kind == "openai_compatible"
	nativeEmbeddings := kind == "gemini" || kind == "vertex_ai" || kind == "bedrock"
	switch operation {
	case "generation":
		if surface == "bedrock" {
			return kind == "bedrock" && (mode == "unary" || mode == "streaming")
		}
		return surface != "bedrock" && (mode == "unary" || mode == "streaming")
	case "batch":
		return surface == "openai" && mode == "unary" && (kind == "openai" || kind == "azure_openai")
	case "realtime":
		return surface == "openai" && mode == "realtime" && (kind == "openai" || kind == "azure_openai")
	case "bedrock_invoke":
		return surface == "bedrock" && (mode == "unary" || mode == "streaming") && kind == "bedrock"
	case "token_count":
		return mode == "unary"
	case "embeddings":
		return surface == "openai" && mode == "unary" && (openaiFamily || nativeEmbeddings)
	case "rerank":
		// Rerank has no common wire: only a vendor whose contract reviews
		// its rerank shape serves it, through the connector it reviews.
		contract, reviewed := vendors.Lookup(vendor)
		return surface == "openai" && mode == "unary" && (kind == "openai_compatible" || kind == "bedrock") && reviewed && contract.Connector == kind && contract.Serves("rerank")
	case "moderation", "image_variation", "translation":
		return surface == "openai" && mode == "unary" && openaiFamily
	case "image_generation":
		if surface != "openai" {
			return false
		}
		if openaiFamily {
			return mode == "unary" || mode == "streaming"
		}
		return mode == "unary" && (kind == "vertex_ai" || kind == "bedrock")
	case "image_edit", "speech", "transcription":
		return surface == "openai" && (mode == "unary" || mode == "streaming") && openaiFamily
	case "video_create":
		return surface == "openai" && mode == "async" && openaiFamily
	case "video_list", "video_get", "video_content", "video_delete":
		return surface == "openai" && mode == "unary" && openaiFamily
	}
	return false
}
