package vendors

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
)

// GenerationParameters are the generation request parameters the catalogue
// lists for a vendor that documents the OpenAI generation dialects.
var GenerationParameters = []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "max_output_tokens", "stop", "tools", "tool_choice", "response_format", "seed", "presence_penalty", "frequency_penalty", "logit_bias", "n", "user", "reasoning", "reasoning_effort"}

var (
	openAIDialects = []string{"openai-chat", "openai-responses"}
	chatDialect    = []string{"openai-chat"}

	mediaOperations  = []string{"image_generation", "image_edit", "image_variation", "speech", "transcription", "translation", "video_create", "video_list", "video_get", "video_content", "video_delete"}
	openAIOperations = slices.Concat([]string{"generation", "token_count", "embeddings", "moderation", "batch", "realtime"}, mediaOperations)
	// compatibleOperations are what the OpenAI-compatible kind serves for an
	// endpoint no contract describes.
	compatibleOperations = slices.Concat([]string{"generation", "token_count", "embeddings", "moderation"}, mediaOperations)

	// chatTokenLimit renames the OpenAI token limit for vendors that document
	// only the older Chat Completions spelling.
	chatTokenLimit = map[string]RequestShape{"generation": {Rewrites: []Rewrite{{From: "max_completion_tokens", To: "max_tokens"}}}}
)

// contracts is the reviewed vendor catalogue. Kind defaults come first, then
// each connector kind's presets in onboarding order.
var contracts = []Contract{
	{ID: "openai", Name: "OpenAI", Maintainer: "OpenAI", Connector: "openai", KindDefault: true, Endpoint: "https://api.openai.com/v1", Documentation: Link{"OpenAI API reference", "https://platform.openai.com/docs/api-reference"}, Discovery: true, Operations: openAIOperations, Dialects: openAIDialects, Parameters: GenerationParameters},
	{ID: "openai_compatible", Name: "OpenAI-compatible", Connector: "openai_compatible", KindDefault: true, Documentation: Link{"OpenAI Chat Completions", "https://platform.openai.com/docs/api-reference/chat"}, Discovery: true, Operations: compatibleOperations, Dialects: openAIDialects, Parameters: GenerationParameters},
	{ID: "anthropic", Name: "Anthropic", Maintainer: "Anthropic", Connector: "anthropic", KindDefault: true, Endpoint: "https://api.anthropic.com/v1", Documentation: Link{"Anthropic API", "https://docs.anthropic.com"}, Discovery: true, Operations: []string{"generation", "token_count"}, Dialects: []string{"anthropic-messages"}, Parameters: GenerationParameters},
	{ID: "google", Name: "Google Gemini", Maintainer: "Google", Connector: "gemini", KindDefault: true, Endpoint: "https://generativelanguage.googleapis.com/v1beta", Documentation: Link{"Gemini API", "https://ai.google.dev"}, Discovery: true, Operations: []string{"generation", "token_count", "embeddings"}, Dialects: []string{"gemini-generate-content"}, Parameters: GenerationParameters},
	{ID: "google-vertex", Name: "Google Vertex AI", Maintainer: "Google", Connector: "vertex_ai", KindDefault: true, Documentation: Link{"Vertex AI", "https://cloud.google.com/vertex-ai"}, Operations: []string{"generation", "token_count", "embeddings", "image_generation"}, Dialects: []string{"gemini-generate-content", "anthropic-messages"}, Parameters: GenerationParameters},
	{ID: "amazon-bedrock", Name: "Amazon Bedrock", Maintainer: "Amazon Web Services", Connector: "bedrock", KindDefault: true, Documentation: Link{"Amazon Bedrock", "https://docs.aws.amazon.com/bedrock"}, Discovery: true, Operations: []string{"generation", "token_count", "embeddings", "bedrock_invoke", "image_generation"}, Dialects: []string{"bedrock-converse", "anthropic-messages"}, Parameters: GenerationParameters},
	{ID: "azure", Name: "Azure OpenAI", Maintainer: "Microsoft", Connector: "azure_openai", KindDefault: true, Documentation: Link{"Azure OpenAI", "https://learn.microsoft.com/azure/ai-services/openai"}, Discovery: true, Operations: openAIOperations, Dialects: openAIDialects, Parameters: GenerationParameters},

	// OpenAI-compatible presets.
	{ID: "groq", Name: "Groq", Maintainer: "Groq", Description: "Groq OpenAI-compatible endpoint.", Connector: "openai_compatible", Endpoint: "https://api.groq.com/openai/v1", Documentation: Link{"Groq OpenAI compatibility", "https://console.groq.com/docs/openai"}, Discovery: true, Operations: []string{"generation"}, Dialects: openAIDialects, Parameters: GenerationParameters, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "mistral", Name: "Mistral", Maintainer: "Mistral AI", Description: "Mistral La Plateforme.", Connector: "openai_compatible", Endpoint: "https://api.mistral.ai/v1", Documentation: Link{"Mistral API", "https://docs.mistral.ai/api/"}, Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects, Parameters: GenerationParameters, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "openrouter", Name: "OpenRouter", Maintainer: "OpenRouter", Description: "OpenRouter unified API.", Connector: "openai_compatible", Endpoint: "https://openrouter.ai/api/v1", Documentation: Link{"OpenRouter API", "https://openrouter.ai/docs"}, Discovery: true, Operations: []string{"generation"}, Dialects: openAIDialects, Parameters: GenerationParameters, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "together", Name: "Together AI", Maintainer: "Together AI", Description: "Together AI inference.", Connector: "openai_compatible", Endpoint: "https://api.together.xyz/v1", Documentation: Link{"Together OpenAI compatibility", "https://docs.together.ai/docs/openai-api-compatibility"}, Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects, Parameters: GenerationParameters, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "vllm", Name: "vLLM", Maintainer: "vLLM", Description: "Self-hosted vLLM OpenAI server.", Connector: "openai_compatible", Endpoint: "https://vllm.example.internal/v1", Documentation: Link{"vLLM OpenAI server", "https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html"}, Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects, Parameters: GenerationParameters, Preset: &Preset{AuthMode: "none", Placeholder: true}},
	{ID: "deepseek", Name: "DeepSeek", Maintainer: "DeepSeek", Description: "DeepSeek compatible API.", Connector: "openai_compatible", Endpoint: "https://api.deepseek.com/v1", Documentation: Link{"DeepSeek API", "https://api-docs.deepseek.com"}, Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Parameters: GenerationParameters, Requests: chatTokenLimit, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "fireworks", Name: "Fireworks", Maintainer: "Fireworks", Description: "Fireworks compatible API.", Connector: "openai_compatible", Endpoint: "https://api.fireworks.ai/inference/v1", Documentation: Link{"Fireworks API", "https://docs.fireworks.ai"}, Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Parameters: GenerationParameters, Requests: chatTokenLimit, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "deepinfra", Name: "DeepInfra", Maintainer: "DeepInfra", Description: "DeepInfra compatible API.", Connector: "openai_compatible", Endpoint: "https://api.deepinfra.com/v1/openai", Documentation: Link{"DeepInfra API", "https://deepinfra.com/docs"}, Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Parameters: GenerationParameters, Requests: chatTokenLimit, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "huggingface", Name: "Hugging Face", Maintainer: "Hugging Face", Description: "Hugging Face compatible API.", Connector: "openai_compatible", Endpoint: "https://router.huggingface.co/v1", Documentation: Link{"Hugging Face API", "https://huggingface.co/docs/inference-providers"}, Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Parameters: GenerationParameters, Requests: chatTokenLimit, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "perplexity", Name: "Perplexity", Maintainer: "Perplexity", Description: "Perplexity compatible API.", Connector: "openai_compatible", Endpoint: "https://api.perplexity.ai", Documentation: Link{"Perplexity API", "https://docs.perplexity.ai"}, Operations: []string{"generation"}, Dialects: chatDialect, Parameters: GenerationParameters, Requests: chatTokenLimit, Preset: &Preset{AuthMode: "api_key"}},
	{ID: "cohere", Name: "Cohere", Maintainer: "Cohere", Description: "Cohere compatible API.", Connector: "openai_compatible", Endpoint: "https://api.cohere.ai/compatibility/v1", Documentation: Link{"Cohere API", "https://docs.cohere.com"},
		Operations: []string{"generation", "embeddings", "rerank"}, Dialects: chatDialect,
		Parameters:  []string{"temperature", "max_output_tokens", "top_p", "stop", "seed", "tools", "response_format", "encoding_format"},
		Unsupported: []string{"n", "parallel_tool_calls", "dimensions", "user", "store", "metadata", "logit_bias", "top_logprobs", "modalities", "prediction", "audio", "service_tier", "input_type", "truncate"},
		Requests: map[string]RequestShape{
			"generation": chatTokenLimit["generation"],
			// Cohere v2 rerank has no return_documents control and never
			// echoes documents; results restore them from the request.
			"rerank": {Unsupported: []string{"truncation"}, Drop: []string{"return_documents"}},
		},
		OperationEndpoints: map[string]string{"rerank": "https://api.cohere.ai/v2/rerank"},
		Preset:             &Preset{AuthMode: "api_key"}},
	{ID: "cohere-native-v2", Name: "Cohere native v2", Maintainer: "Cohere", Description: "Cohere native v2 compatible API.", Connector: "openai_compatible", Endpoint: "https://api.cohere.ai/v2", Documentation: Link{"Cohere native v2 API", "https://docs.cohere.com/reference/embed"},
		Operations: []string{"embeddings", "rerank"}, ProbeOperation: "embeddings",
		Parameters: []string{"input_type", "texts", "images", "inputs", "embedding_types", "output_dimension", "truncate", "max_tokens", "top_n", "max_tokens_per_doc", "priority"},
		Preset:     &Preset{AuthMode: "api_key"}},
	{ID: "voyage", Name: "Voyage AI", Maintainer: "Voyage AI", Description: "Voyage AI compatible API.", Connector: "openai_compatible", Endpoint: "https://api.voyageai.com/v1", Documentation: Link{"Voyage AI API", "https://docs.voyageai.com"},
		Operations: []string{"embeddings", "rerank"}, ProbeOperation: "embeddings",
		Parameters: []string{"dimensions", "input_type", "truncation", "output_dtype", "encoding_format"},
		Requests: map[string]RequestShape{
			"embeddings": {TextInput: true, Rewrites: []Rewrite{{From: "dimensions", To: "output_dimension"}, {From: "encoding_format", To: "output_dtype", Value: json.RawMessage(`"float"`)}}},
			"rerank":     {Rewrites: []Rewrite{{From: "top_n", To: "top_k"}}},
		},
		Preset: &Preset{AuthMode: "api_key"}},
}

// index maps vendor identifiers to their position in contracts.
var index = map[string]int{}

func init() {
	for i, c := range contracts {
		if err := c.validate(); err != nil {
			panic(fmt.Sprintf("vendor contract %q: %v", c.ID, err))
		}
		if _, duplicate := index[c.ID]; duplicate {
			panic(fmt.Sprintf("vendor contract %q is declared twice", c.ID))
		}
		index[c.ID] = i
	}
}

// presetAuthModes are the authentications a preset may select.
var presetAuthModes = []string{"api_key", "headers", "none"}

func (c Contract) validate() error {
	if c.ID == "" || c.Name == "" || c.Connector == "" || len(c.Operations) == 0 {
		return fmt.Errorf("identity, name, connector and operations are required")
	}
	if u, err := url.Parse(c.Documentation.URL); err != nil || u.Scheme != "https" || c.Documentation.Label == "" {
		return fmt.Errorf("a labelled HTTPS documentation URL is required")
	}
	if c.ProbeOperation != "" && !c.Serves(c.ProbeOperation) {
		return fmt.Errorf("the probe operation %q is not served", c.ProbeOperation)
	}
	for operation := range c.Requests {
		if !c.Serves(operation) {
			return fmt.Errorf("a request shape names unserved operation %q", operation)
		}
	}
	for operation, endpoint := range c.OperationEndpoints {
		if u, err := url.Parse(endpoint); err != nil || u.Scheme != "https" || !c.Serves(operation) {
			return fmt.Errorf("operation endpoint %q must be an HTTPS URL of a served operation", operation)
		}
	}
	if c.Preset == nil {
		return nil
	}
	if c.KindDefault {
		return fmt.Errorf("a kind's default vendor is not a preset")
	}
	if u, err := url.Parse(c.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("a preset endpoint must be an HTTPS base URL")
	}
	if c.Description == "" || c.Maintainer == "" || !slices.Contains(presetAuthModes, c.Preset.AuthMode) {
		return fmt.Errorf("a preset needs a description, a maintainer and a supported authentication")
	}
	if profile := c.Preset.Profile; profile != nil && (profile.ID == "" || profile.Revision == "") {
		return fmt.Errorf("a preset profile needs an identity and revision")
	}
	return nil
}
