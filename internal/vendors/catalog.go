package vendors

import (
	"encoding/json"
	"fmt"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
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

	// maxTokens renames the OpenAI token limit for vendors that document only
	// the older Chat Completions spelling.
	maxTokens      = Rewrite{From: "max_completion_tokens", To: "max_tokens"}
	chatTokenLimit = map[string]RequestShape{"generation": {Rewrites: []Rewrite{maxTokens}}}

	// exactChat is the profile of a preset whose vendor speaks Chat
	// Completions exactly, so it can serve strict routes.
	exactChat = &ProfileRef{ID: "compatible-chat", Revision: "1"}
)

// compatiblePreset is an OpenAI-compatible vendor onboarding offers with an
// API key at its reviewed endpoint.
func compatiblePreset(id, name, maintainer, description, endpoint string, docs Link, c Contract, profile *ProfileRef) Contract {
	c.ID, c.Name, c.Maintainer, c.Description, c.Connector, c.Endpoint, c.Documentation = id, name, maintainer, description, "openai_compatible", endpoint, docs
	if c.Parameters == nil {
		c.Parameters = GenerationParameters
	}
	c.Preset = &Preset{AuthMode: "api_key", Profile: profile}
	return c
}

// accountPreset is a vendor whose host or path names the operator's account,
// so its endpoint is a placeholder.
func accountPreset(id, name, description, endpoint string, docs Link, c Contract) Contract {
	c = compatiblePreset(id, name, name, description, endpoint, docs, c, nil)
	c.Preset.Placeholder = true
	return c
}

// selfHostedPreset is a runtime the operator runs: unauthenticated at a
// placeholder address until the operator says otherwise.
func selfHostedPreset(id, name, description, endpoint string, docs Link, c Contract) Contract {
	c = compatiblePreset(id, name, name, description, endpoint, docs, c, nil)
	c.Preset.AuthMode, c.Preset.Placeholder = "none", true
	return c
}

// contracts is the reviewed vendor catalogue. Kind defaults come first, then
// each connector kind's presets in onboarding order.
var contracts = []Contract{
	{ID: "openai", Name: "OpenAI", Maintainer: "OpenAI", Connector: "openai", KindDefault: true, Endpoint: "https://api.openai.com/v1", Documentation: Link{"OpenAI API reference", "https://platform.openai.com/docs/api-reference"}, Discovery: true, Operations: openAIOperations, Dialects: openAIDialects, Parameters: GenerationParameters},
	{ID: "openai_compatible", Name: "OpenAI-compatible", Connector: "openai_compatible", KindDefault: true, Documentation: Link{"OpenAI Chat Completions", "https://platform.openai.com/docs/api-reference/chat"}, Discovery: true, Operations: compatibleOperations, Dialects: openAIDialects, Parameters: GenerationParameters},
	{ID: "anthropic", Name: "Anthropic", Maintainer: "Anthropic", Connector: "anthropic", KindDefault: true, Endpoint: "https://api.anthropic.com/v1", Documentation: Link{"Anthropic API", "https://docs.anthropic.com"}, Discovery: true, Operations: []string{"generation", "token_count"}, Dialects: []string{"anthropic-messages"}, Parameters: GenerationParameters},
	{ID: "google", Name: "Google Gemini", Maintainer: "Google", Connector: "gemini", KindDefault: true, Endpoint: "https://generativelanguage.googleapis.com/v1beta", Documentation: Link{"Gemini API", "https://ai.google.dev"}, Discovery: true, Operations: []string{"generation", "token_count", "embeddings", "image_generation", "speech", "transcription"}, Dialects: []string{"gemini-generate-content"}, Parameters: GenerationParameters,
		MediaWires: map[string]string{"image_generation": "gemini", "speech": "gemini", "transcription": "gemini"}},
	{ID: "google-vertex", Name: "Google Vertex AI", Maintainer: "Google", Connector: "vertex_ai", KindDefault: true, Documentation: Link{"Vertex AI", "https://cloud.google.com/vertex-ai"}, Operations: []string{"generation", "token_count", "embeddings", "image_generation", "speech", "transcription"}, Dialects: []string{"gemini-generate-content", "anthropic-messages", "openai-chat"}, Parameters: GenerationParameters,
		MediaWires: map[string]string{"image_generation": "gemini", "speech": "gemini", "transcription": "gemini"}},
	{ID: "amazon-bedrock", Name: "Amazon Bedrock", Maintainer: "Amazon Web Services", Connector: "bedrock", KindDefault: true, Documentation: Link{"Amazon Bedrock", "https://docs.aws.amazon.com/bedrock"}, Discovery: true, Operations: []string{"generation", "token_count", "embeddings", "bedrock_invoke", "image_generation", "rerank", "speech"}, Dialects: []string{"bedrock-converse", "anthropic-messages"}, Parameters: GenerationParameters,
		Requests: map[string]RequestShape{"rerank": {Unsupported: []string{"truncation"}}}, MediaWires: map[string]string{"speech": "polly"}},
	{ID: "amazon-sagemaker", Name: "Amazon SageMaker AI", Maintainer: "Amazon Web Services", Connector: "sagemaker", KindDefault: true, Documentation: Link{"SageMaker AI OpenAI-compatible endpoints", "https://docs.aws.amazon.com/sagemaker/latest/dg/realtime-endpoints-openai-compatible.html"}, Operations: []string{"generation"}, Dialects: []string{"openai-chat"}, Parameters: GenerationParameters},
	{ID: "ibm-watsonx", Name: "IBM watsonx.ai", Maintainer: "IBM", Connector: "watsonx", KindDefault: true, Documentation: Link{"watsonx.ai chat API", "https://dataplatform.cloud.ibm.com/docs/content/wsj/analyze-data/fm-api-chat.html?context=wx"}, Discovery: true,
		Operations: []string{"generation"}, Dialects: []string{"openai-chat"},
		Parameters:  []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "stop", "tools", "tool_choice", "response_format", "seed", "presence_penalty", "frequency_penalty", "n", "reasoning_effort"},
		Unsupported: []string{"logit_bias", "user", "parallel_tool_calls", "store", "metadata", "service_tier", "prediction", "modalities", "audio", "web_search_options"}},
	{ID: "azure", Name: "Azure OpenAI", Maintainer: "Microsoft", Connector: "azure_openai", KindDefault: true, Documentation: Link{"Azure OpenAI", "https://learn.microsoft.com/azure/ai-services/openai"}, Discovery: true, Operations: openAIOperations, Dialects: openAIDialects, Parameters: GenerationParameters},

	// OpenAI-compatible presets, each reviewed against the documentation it
	// links on the date its evidence in tests/fixtures/vendors records.
	compatiblePreset("groq", "Groq", "Groq", "Groq OpenAI-compatible endpoint.", "https://api.groq.com/openai/v1", Link{"Groq OpenAI compatibility", "https://console.groq.com/docs/openai"}, Contract{Discovery: true, Operations: []string{"generation", "transcription", "translation"}, Dialects: openAIDialects, Unsupported: []string{"logit_bias", "logprobs", "top_logprobs"},
		MediaWires: map[string]string{"transcription": "groq-audio", "translation": "groq-audio"}, AccountProbe: "models"}, exactChat),
	compatiblePreset("mistral", "Mistral", "Mistral AI", "Mistral La Plateforme.", "https://api.mistral.ai/v1", Link{"Mistral chat API", "https://docs.mistral.ai/api/endpoint/chat"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: []string{"openai-chat", "mistral-fim"},
		Requests: map[string]RequestShape{"generation": {Rewrites: []Rewrite{maxTokens, {From: "seed", To: "random_seed"}}}}}, nil),
	compatiblePreset("openrouter", "OpenRouter", "OpenRouter", "OpenRouter unified API.", "https://openrouter.ai/api/v1", Link{"OpenRouter API reference", "https://openrouter.ai/docs/api/reference/overview"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects}, exactChat),
	compatiblePreset("together", "Together AI", "Together AI", "Together AI inference.", "https://api.together.ai/v1", Link{"Together OpenAI compatibility", "https://docs.together.ai/docs/openai-api-compatibility"}, Contract{Operations: []string{"generation", "embeddings", "rerank"}, Dialects: chatDialect, Unsupported: []string{"logit_bias", "metadata", "prediction", "service_tier", "store"},
		Requests: map[string]RequestShape{"generation": chatTokenLimit["generation"], "rerank": {Unsupported: []string{"truncation"}}}}, nil),
	selfHostedPreset("vllm", "vLLM", "Self-hosted vLLM OpenAI server.", "https://vllm.example.internal/v1", Link{"vLLM OpenAI-compatible server", "https://docs.vllm.ai/en/latest/serving/online_serving/openai_compatible_server/"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects, Unsupported: []string{"user"}}),
	compatiblePreset("deepseek", "DeepSeek", "DeepSeek", "DeepSeek compatible API.", "https://api.deepseek.com", Link{"DeepSeek chat completion API", "https://api-docs.deepseek.com/api/create-chat-completion"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Unsupported: []string{"frequency_penalty", "presence_penalty"}, Requests: chatTokenLimit}, nil),
	compatiblePreset("fireworks", "Fireworks", "Fireworks", "Fireworks compatible API.", "https://api.fireworks.ai/inference/v1", Link{"Fireworks OpenAI compatibility", "https://docs.fireworks.ai/tools-sdks/openai-compatibility"}, Contract{Operations: []string{"generation", "embeddings"}, Dialects: chatDialect}, exactChat),
	compatiblePreset("deepinfra", "DeepInfra", "DeepInfra", "DeepInfra compatible API.", "https://api.deepinfra.com/v1/openai", Link{"DeepInfra chat API", "https://docs.deepinfra.com/chat/overview"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),
	compatiblePreset("huggingface", "Hugging Face", "Hugging Face", "Hugging Face Inference Providers.", "https://router.huggingface.co/v1", Link{"Hugging Face chat completion", "https://huggingface.co/docs/inference-providers/tasks/chat-completion"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),
	compatiblePreset("cohere", "Cohere", "Cohere", "Cohere compatible API.", "https://api.cohere.ai/compatibility/v1", Link{"Cohere Compatibility API", "https://docs.cohere.com/docs/compatibility-api"}, Contract{
		Operations: []string{"generation", "embeddings", "rerank"}, Dialects: chatDialect,
		Parameters:  []string{"temperature", "max_output_tokens", "top_p", "stop", "seed", "tools", "response_format", "encoding_format"},
		Unsupported: []string{"n", "parallel_tool_calls", "dimensions", "user", "store", "metadata", "logit_bias", "top_logprobs", "modalities", "prediction", "audio", "service_tier", "input_type", "truncate"},
		Requests: map[string]RequestShape{
			"generation": chatTokenLimit["generation"],
			// Cohere v2 rerank has no return_documents control and never
			// echoes documents; results restore them from the request.
			"rerank": {Unsupported: []string{"truncation"}, Drop: []string{"return_documents"}},
		},
		OperationEndpoints: map[string]string{"rerank": "https://api.cohere.ai/v2/rerank"}}, nil),
	compatiblePreset("cohere-native-v2", "Cohere native v2", "Cohere", "Cohere native v2 compatible API.", "https://api.cohere.ai/v2", Link{"Cohere native v2 API", "https://docs.cohere.com/reference/embed"}, Contract{
		Operations: []string{"embeddings", "rerank", "generation"}, Dialects: []string{"cohere-chat-v2"}, ProbeOperation: "embeddings",
		Parameters: []string{"input_type", "texts", "images", "inputs", "embedding_types", "output_dimension", "truncate", "max_tokens", "top_n", "max_tokens_per_doc", "priority",
			"messages", "documents", "citation_options", "tools", "tool_choice", "strict_tools", "response_format", "safety_mode", "temperature", "p", "k", "seed", "stop_sequences", "frequency_penalty", "presence_penalty", "logprobs", "thinking"}}, &ProfileRef{ID: "cohere-v2", Revision: "1"}),
	compatiblePreset("jina", "Jina AI", "Jina AI", "Jina AI embeddings and reranking.", "https://api.jina.ai/v1", Link{"Jina AI API", "https://api.jina.ai/openapi.json"}, Contract{
		Operations: []string{"embeddings", "rerank"}, ProbeOperation: "embeddings",
		Parameters: []string{"task", "dimensions", "embedding_type", "normalized", "late_chunking", "truncate", "top_n", "return_documents", "max_doc_length"},
		Requests: map[string]RequestShape{
			"embeddings": {Rewrites: []Rewrite{{From: "encoding_format", To: "embedding_type"}}},
			"rerank":     {Unsupported: []string{"truncation"}},
		}}, nil),
	compatiblePreset("elevenlabs", "ElevenLabs", "ElevenLabs", "ElevenLabs speech synthesis and transcription.", "https://api.elevenlabs.io/v1", Link{"ElevenLabs API reference", "https://elevenlabs.io/docs/api-reference/introduction"}, Contract{
		Operations: []string{"speech", "transcription"}, ProbeOperation: "speech", Parameters: []string{"voice", "speed", "response_format", "language"},
		MediaWires: map[string]string{"speech": "elevenlabs", "transcription": "elevenlabs"}, Credential: &Credential{Header: "Xi-Api-Key"}, AccountProbe: "user"}, nil),
	compatiblePreset("deepgram", "Deepgram", "Deepgram", "Deepgram speech recognition and synthesis.", "https://api.deepgram.com/v1", Link{"Deepgram API reference", "https://developers.deepgram.com/reference/deepgram-api-overview"}, Contract{
		Operations: []string{"speech", "transcription"}, ProbeOperation: "transcription", Parameters: []string{"voice", "response_format", "language"},
		MediaWires: map[string]string{"speech": "deepgram", "transcription": "deepgram"}, Credential: &Credential{Header: "Authorization", Scheme: "Token "}, AccountProbe: "projects"}, nil),
	compatiblePreset("assemblyai", "AssemblyAI", "AssemblyAI", "AssemblyAI speech recognition.", "https://api.assemblyai.com", Link{"AssemblyAI transcript API", "https://www.assemblyai.com/docs/pre-recorded-audio/api-reference/transcripts/submit"}, Contract{
		Operations: []string{"transcription"}, ProbeOperation: "transcription", Parameters: []string{"language", "response_format"},
		MediaWires: map[string]string{"transcription": "assemblyai"}, Credential: &Credential{Header: "Authorization"}, AccountProbe: "v2/transcript?limit=1"}, nil),
	compatiblePreset("stability", "Stability AI", "Stability AI", "Stability AI image generation and inpainting.", "https://api.stability.ai", Link{"Stability AI API reference", "https://platform.stability.ai/docs/api-reference"}, Contract{
		Operations: []string{"image_generation", "image_edit"}, ProbeOperation: "image_generation", Parameters: []string{"size", "output_format", "response_format"},
		MediaWires: map[string]string{"image_generation": "stability", "image_edit": "stability"}, AccountProbe: "v1/user/balance",
		// Stability refuses moderated content with 403, which is no
		// credential failure.
		ErrorClasses: []ErrorClass{{Status: 403, Type: "content_moderation", Class: "upstream_client"}}}, nil),
	compatiblePreset("recraft", "Recraft", "Recraft", "Recraft raster image generation.", "https://external.api.recraft.ai/v1", Link{"Recraft API endpoints", "https://www.recraft.ai/docs/api-reference/endpoints"}, Contract{
		Operations: []string{"image_generation"}, ProbeOperation: "image_generation", Parameters: []string{"n", "size", "output_format", "response_format"},
		MediaWires: map[string]string{"image_generation": "recraft"}, AccountProbe: "users/me"}, nil),
	compatiblePreset("bfl", "Black Forest Labs", "Black Forest Labs", "Black Forest Labs FLUX image generation.", "https://api.bfl.ai/v1", Link{"BFL integration guidelines", "https://docs.bfl.ai/api_integration/integration_guidelines"}, Contract{
		Operations: []string{"image_generation"}, ProbeOperation: "image_generation", Parameters: []string{"size", "output_format"},
		MediaWires: map[string]string{"image_generation": "bfl"}, Credential: &Credential{Header: "X-Key"}, AccountProbe: "credits"}, nil),
	compatiblePreset("voyage", "Voyage AI", "Voyage AI", "Voyage AI compatible API.", "https://api.voyageai.com/v1", Link{"Voyage AI embeddings API", "https://docs.voyageai.com/reference/embeddings-api"}, Contract{
		Operations: []string{"embeddings", "rerank"}, ProbeOperation: "embeddings",
		Parameters: []string{"dimensions", "input_type", "truncation", "output_dtype", "encoding_format"},
		Requests: map[string]RequestShape{
			"embeddings": {TextInput: true, Rewrites: []Rewrite{{From: "dimensions", To: "output_dimension"}, {From: "encoding_format", To: "output_dtype", Value: json.RawMessage(`"float"`)}}},
			"rerank":     {Rewrites: []Rewrite{{From: "top_n", To: "top_k"}}},
		}}, nil),

	// Frontier and fast inference.
	compatiblePreset("xai", "xAI", "xAI", "xAI Grok API.", "https://api.x.ai/v1", Link{"xAI chat completions", "https://docs.x.ai/developers/rest-api-reference/inference/chat-completions"}, Contract{Discovery: true, Operations: []string{"generation", "image_generation"}, Dialects: openAIDialects, Unsupported: []string{"logit_bias"},
		MediaWires: map[string]string{"image_generation": "xai-images"}, AccountProbe: "api-key"}, exactChat),
	compatiblePreset("cerebras", "Cerebras", "Cerebras", "Cerebras Inference.", "https://api.cerebras.ai/v1", Link{"Cerebras OpenAI compatibility", "https://inference-docs.cerebras.ai/resources/openai"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Unsupported: []string{"prediction", "service_tier"}}, exactChat),
	compatiblePreset("sambanova", "SambaNova", "SambaNova", "SambaNova Cloud.", "https://api.sambanova.ai/v1", Link{"SambaNova OpenAI compatibility", "https://docs.sambanova.ai/docs/en/features/openai-compatibility"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects, Unsupported: []string{"frequency_penalty", "presence_penalty"}}, exactChat),
	compatiblePreset("nebius", "Nebius Token Factory", "Nebius", "Nebius Token Factory, formerly AI Studio.", "https://api.tokenfactory.nebius.com/v1", Link{"Nebius Token Factory chat completions", "https://docs.tokenfactory.nebius.com/api-reference/inference/create-chat-completion"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects}, exactChat),
	compatiblePreset("novita", "Novita AI", "Novita AI", "Novita AI model APIs.", "https://api.novita.ai/openai/v1", Link{"Novita AI chat completions", "https://docs.novita.ai/api-reference/model-apis-llm-create-chat-completion"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),
	compatiblePreset("nvidia-nim", "NVIDIA NIM", "NVIDIA", "NVIDIA-hosted NIM API catalog.", "https://integrate.api.nvidia.com/v1", Link{"NVIDIA NIM chat completions", "https://docs.api.nvidia.com/nim/reference/create_chat_completion_v1_chat_completions_post"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),
	compatiblePreset("featherless", "Featherless", "Featherless", "Featherless serverless inference.", "https://api.featherless.ai/v1", Link{"Featherless completions", "https://featherless.ai/docs/completions"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),
	compatiblePreset("baseten", "Baseten", "Baseten", "Baseten Model APIs.", "https://inference.baseten.co/v1", Link{"Baseten chat completions", "https://docs.baseten.co/reference/inference-api/chat-completions"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),

	// Regional and open-model platforms. Keys are bound to a region, so each
	// region the vendor documents is its own preset.
	compatiblePreset("moonshot", "Moonshot AI", "Moonshot AI", "Moonshot AI Kimi platform.", "https://api.moonshot.ai/v1", Link{"Kimi chat API", "https://platform.kimi.ai/docs/api/chat"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect}, exactChat),
	compatiblePreset("moonshot-cn", "Moonshot AI (China)", "Moonshot AI", "Moonshot AI Kimi platform in mainland China.", "https://api.moonshot.cn/v1", Link{"Kimi chat API (China)", "https://platform.kimi.com/docs/api/chat"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: chatDialect}, exactChat),
	compatiblePreset("dashscope", "Alibaba Cloud Model Studio", "Alibaba Cloud", "Alibaba Cloud Model Studio, international.", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", Link{"Model Studio OpenAI Chat Completions", "https://www.alibabacloud.com/help/en/model-studio/qwen-api-via-openai-chat-completions"}, Contract{Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects}, exactChat),
	compatiblePreset("dashscope-cn", "Alibaba Cloud Model Studio (China)", "Alibaba Cloud", "Alibaba Cloud Bailian in mainland China.", "https://dashscope.aliyuncs.com/compatible-mode/v1", Link{"Bailian OpenAI Chat Completions", "https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-chat-completions"}, Contract{Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects}, exactChat),
	compatiblePreset("zai", "Z.ai", "Z.ai", "Z.ai GLM platform.", "https://api.z.ai/api/paas/v4", Link{"Z.ai chat completion", "https://docs.z.ai/api-reference/llm/chat-completion"}, Contract{Operations: []string{"generation"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),
	compatiblePreset("zhipu", "Zhipu BigModel", "Zhipu AI", "Zhipu BigModel GLM platform in mainland China.", "https://open.bigmodel.cn/api/paas/v4", Link{"BigModel OpenAI compatibility", "https://docs.bigmodel.cn/cn/guide/develop/openai/introduction"}, Contract{Operations: []string{"generation", "embeddings"}, Dialects: chatDialect, Requests: chatTokenLimit}, nil),
	compatiblePreset("minimax", "MiniMax", "MiniMax", "MiniMax platform, international.", "https://api.minimax.io/v1", Link{"MiniMax OpenAI chat", "https://platform.minimax.io/docs/api-reference/text-chat-openai"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: openAIDialects, Unsupported: []string{"frequency_penalty", "logit_bias", "presence_penalty"}}, exactChat),
	compatiblePreset("minimax-cn", "MiniMax (China)", "MiniMax", "MiniMax platform in mainland China.", "https://api.minimax.cn/v1", Link{"MiniMax OpenAI chat (China)", "https://platform.minimax.cn/docs/api-reference/text-chat-openai"}, Contract{Discovery: true, Operations: []string{"generation"}, Dialects: openAIDialects, Unsupported: []string{"frequency_penalty", "logit_bias", "presence_penalty"}}, exactChat),
	compatiblePreset("volcengine-ark", "Volcengine Ark", "Volcengine", "Volcengine Ark in mainland China. Models may be named by endpoint ID.", "https://ark.cn-beijing.volces.com/api/v3", Link{"Ark chat API", "https://www.volcengine.com/docs/82379/1494384"}, Contract{Operations: []string{"generation"}, Dialects: openAIDialects}, exactChat),
	compatiblePreset("byteplus-modelark", "BytePlus ModelArk", "BytePlus", "BytePlus ModelArk, Southeast Asia. Models may be named by endpoint ID.", "https://ark.ap-southeast.bytepluses.com/api/v3", Link{"ModelArk chat API", "https://docs.byteplus.com/en/docs/ModelArk/1494384"}, Contract{Operations: []string{"generation"}, Dialects: openAIDialects}, exactChat),
	compatiblePreset("scaleway", "Scaleway Generative APIs", "Scaleway", "Scaleway Generative APIs.", "https://api.scaleway.ai/v1", Link{"Scaleway Chat API", "https://www.scaleway.com/en/docs/generative-apis/api-cli/using-chat-api/"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects,
		Unsupported: []string{"audio", "metadata", "modalities", "prediction", "service_tier", "store", "user", "web_search_options"}}, exactChat),
	compatiblePreset("ovhcloud", "OVHcloud AI Endpoints", "OVHcloud", "OVHcloud AI Endpoints.", "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1", Link{"AI Endpoints capabilities", "https://docs.ovhcloud.com/en/guides/public-cloud/ai-machine-learning/ai-endpoints-capabilities"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects}, exactChat),
	compatiblePreset("nscale", "Nscale", "Nscale", "Nscale serverless inference.", "https://inference.api.nscale.com/v1", Link{"Nscale chat completions", "https://docs.nscale.com/api-reference/inference/create-chat-completion"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: chatDialect}, exactChat),

	// Data platforms and aggregators. Account-scoped hosts are placeholders
	// the operator replaces.
	accountPreset("databricks", "Databricks", "Databricks Foundation Model APIs through the AI Gateway.", "https://your-workspace.cloud.databricks.com/ai-gateway/mlflow/v1", Link{"Foundation Model APIs reference", "https://docs.databricks.com/aws/en/machine-learning/foundation-model-apis/api-reference"}, Contract{Operations: []string{"generation", "embeddings"}, Dialects: chatDialect, Requests: chatTokenLimit}),
	accountPreset("snowflake-cortex", "Snowflake Cortex", "Snowflake Cortex REST API.", "https://your-account.snowflakecomputing.com/api/v2/cortex/v1", Link{"Cortex REST API", "https://docs.snowflake.com/en/user-guide/snowflake-cortex/cortex-rest-api"}, Contract{Operations: []string{"generation"}, Dialects: chatDialect, Unsupported: []string{"service_tier", "store"},
		// Cortex refuses max_tokens and documents only max_completion_tokens.
		Requests: map[string]RequestShape{"generation": {Rewrites: []Rewrite{{From: "max_tokens", To: "max_completion_tokens"}}}}}),
	accountPreset("cloudflare-workers-ai", "Cloudflare Workers AI", "Cloudflare Workers AI OpenAI-compatible endpoints.", "https://api.cloudflare.com/client/v4/accounts/your-account-id/ai/v1", Link{"Workers AI OpenAI compatibility", "https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/"}, Contract{Operations: []string{"generation", "embeddings"}, Dialects: chatDialect}),
	compatiblePreset("vercel-ai-gateway", "Vercel AI Gateway", "Vercel", "Vercel AI Gateway.", "https://ai-gateway.vercel.sh/v1", Link{"AI Gateway Chat Completions", "https://vercel.com/docs/ai-gateway/sdks-and-apis/openai-chat-completions"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects, Requests: chatTokenLimit}, nil),

	// Self-hosted runtimes start unauthenticated at a placeholder address and
	// rely on the egress allowlists for private addresses.
	selfHostedPreset("ollama", "Ollama", "Self-hosted Ollama; the server listens on port 11434 by default.", "https://ollama.example.internal/v1", Link{"Ollama OpenAI compatibility", "https://docs.ollama.com/api/openai-compatibility"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects,
		Unsupported: []string{"logit_bias", "logprobs", "n", "tool_choice", "top_logprobs", "user"}, Requests: chatTokenLimit}),
	selfHostedPreset("lmstudio", "LM Studio", "Self-hosted LM Studio server; port 1234 in its examples.", "https://lmstudio.example.internal/v1", Link{"LM Studio OpenAI compatibility", "https://lmstudio.ai/docs/developer/openai-compat"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects, Requests: chatTokenLimit}),
	selfHostedPreset("llamacpp", "llama.cpp server", "Self-hosted llama-server; port 8080 by default.", "https://llamacpp.example.internal/v1", Link{"llama.cpp server API", "https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md"}, Contract{Discovery: true, Operations: []string{"generation", "embeddings"}, Dialects: openAIDialects}),
	selfHostedPreset("infinity", "Infinity", "Self-hosted Infinity embedding and reranking server; port 7997 by default.", "https://infinity.example.internal", Link{"Infinity API", "https://github.com/michaelfeil/infinity/blob/main/docs/assets/openapi.json"}, Contract{
		Discovery: true, Operations: []string{"embeddings", "rerank"}, ProbeOperation: "embeddings", Parameters: []string{"dimensions", "encoding_format", "top_n", "return_documents", "raw_scores"},
		Requests: map[string]RequestShape{"rerank": {Unsupported: []string{"truncation"}}}}),
	selfHostedPreset("docker-model-runner", "Docker Model Runner", "Docker Model Runner; port 12434 on the host by default.", "https://model-runner.example.internal/engines/v1", Link{"Docker Model Runner API", "https://docs.docker.com/ai/model-runner/api-reference/"}, Contract{Operations: []string{"generation", "embeddings"}, Dialects: chatDialect, Requests: chatTokenLimit}),
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
	if c.Credential != nil && (c.Credential.Header == "" || textproto.CanonicalMIMEHeaderKey(c.Credential.Header) != c.Credential.Header || strings.ContainsAny(c.Credential.Scheme, "\r\n")) {
		return fmt.Errorf("a credential placement needs a canonical header")
	}
	for operation, wire := range c.MediaWires {
		if !c.Serves(operation) || wire == "" {
			return fmt.Errorf("media wire %q names an unserved operation %q", wire, operation)
		}
	}
	for _, rule := range c.ErrorClasses {
		if rule.Status < 400 || rule.Status > 599 || !slices.Contains([]string{"credential", "rate_limit", "upstream_server", "upstream_client"}, rule.Class) {
			return fmt.Errorf("an error class needs an unsuccessful status and a failover class")
		}
	}
	if c.AccountProbe != "" && (strings.HasPrefix(c.AccountProbe, "/") || strings.Contains(c.AccountProbe, "..") || strings.ContainsAny(c.AccountProbe, "#\\")) {
		return fmt.Errorf("the account probe must be a relative path")
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
