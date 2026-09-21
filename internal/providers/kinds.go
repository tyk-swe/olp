// Package providers owns provider connections: configuration drafts, model
// discovery and certification, credential pools, activation into immutable
// revisions, and history for the connector matrix.
package providers

import "github.com/tyk-swe/olp/internal/connectors"

// Kind names accepted by the gateway.
const (
	KindOpenAI           = "openai"
	KindOpenAICompatible = "openai_compatible"
	KindAnthropic        = "anthropic"
	KindGemini           = "gemini"
	KindAzure            = "azure_openai"
	KindVertex           = "vertex_ai"
	KindBedrock          = "bedrock"
)

// Auth modes accepted by the gateway.
const (
	AuthAPIKey  = "api_key"
	AuthHeaders = "headers"
	AuthNone    = "none"
)

// Capability tuple vocabulary shared across providers.
const (
	OperationGeneration = "generation"
	SurfaceOpenAI       = "openai"
	ModeUnary           = "unary"
	ModeStreaming       = "streaming"
)

// DefaultOpenAIEndpoint is applied when an openai connection omits one.
const DefaultOpenAIEndpoint = "https://api.openai.com/v1"

type authCapability struct {
	Mode       string `json:"mode"`
	Label      string `json:"label"`
	Credential string `json:"credential"`
}

type fieldCapability struct {
	Field    string `json:"field"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
}

type preset struct {
	ID                 string `json:"id"`
	Label              string `json:"label"`
	Description        string `json:"description"`
	Endpoint           string `json:"endpoint"`
	AuthMode           string `json:"auth_mode"`
	Maintainer         string `json:"maintainer"`
	DocumentationLabel string `json:"documentation_label"`
	DocumentationURL   string `json:"documentation_url"`
}

type kindCapability struct {
	Kind            string            `json:"kind"`
	Label           string            `json:"label"`
	Description     string            `json:"description"`
	DefaultAuthMode string            `json:"default_auth_mode"`
	AuthModes       []authCapability  `json:"auth_modes"`
	Fields          []fieldCapability `json:"fields"`
	Presets         []preset          `json:"presets"`
}

type vendor struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Connector        string   `json:"connector"`
	Discovery        bool     `json:"discovery"`
	Operations       []string `json:"operations"`
	Authentication   []string `json:"authentication"`
	Parameters       []string `json:"parameters"`
	DocumentationURL string   `json:"documentation_url"`
	Endpoint         *string  `json:"endpoint"`
}

type CapabilityInput struct {
	Operation string `json:"operation"`
	Surface   string `json:"surface"`
	Mode      string `json:"mode"`
}

var kinds = []kindCapability{
	{
		Kind:            KindOpenAI,
		Label:           "OpenAI",
		Description:     "OpenAI platform API with Chat Completions and Responses.",
		DefaultAuthMode: AuthAPIKey,
		AuthModes:       []authCapability{{Mode: AuthAPIKey, Label: "API key", Credential: "required"}},
		Fields:          []fieldCapability{{Field: "endpoint", Label: "Endpoint", Required: false}},
		Presets:         []preset{},
	},
	{
		Kind:            KindOpenAICompatible,
		Label:           "OpenAI-compatible",
		Description:     "Any server implementing the OpenAI Chat Completions or Responses API.",
		DefaultAuthMode: AuthAPIKey,
		AuthModes: []authCapability{
			{Mode: AuthAPIKey, Label: "Bearer API key", Credential: "required"},
			{Mode: AuthHeaders, Label: "Custom credential headers", Credential: "required"},
			{Mode: AuthNone, Label: "No credential", Credential: "forbidden"},
		},
		Fields: []fieldCapability{{Field: "endpoint", Label: "Endpoint", Required: true}},
		Presets: []preset{
			{ID: "groq", Label: "Groq", Description: "Groq OpenAI-compatible endpoint.", Endpoint: "https://api.groq.com/openai/v1", AuthMode: AuthAPIKey, Maintainer: "Groq", DocumentationLabel: "Groq OpenAI compatibility", DocumentationURL: "https://console.groq.com/docs/openai"},
			{ID: "mistral", Label: "Mistral", Description: "Mistral La Plateforme.", Endpoint: "https://api.mistral.ai/v1", AuthMode: AuthAPIKey, Maintainer: "Mistral AI", DocumentationLabel: "Mistral API", DocumentationURL: "https://docs.mistral.ai/api/"},
			{ID: "openrouter", Label: "OpenRouter", Description: "OpenRouter unified API.", Endpoint: "https://openrouter.ai/api/v1", AuthMode: AuthAPIKey, Maintainer: "OpenRouter", DocumentationLabel: "OpenRouter API", DocumentationURL: "https://openrouter.ai/docs"},
			{ID: "together", Label: "Together AI", Description: "Together AI inference.", Endpoint: "https://api.together.xyz/v1", AuthMode: AuthAPIKey, Maintainer: "Together AI", DocumentationLabel: "Together OpenAI compatibility", DocumentationURL: "https://docs.together.ai/docs/openai-api-compatibility"},
			{ID: "vllm", Label: "vLLM", Description: "Self-hosted vLLM OpenAI server.", Endpoint: "https://vllm.example.internal/v1", AuthMode: AuthNone, Maintainer: "vLLM", DocumentationLabel: "vLLM OpenAI server", DocumentationURL: "https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html"},
		},
	},
}

var generationParameters = []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "max_output_tokens", "stop", "tools", "tool_choice", "response_format", "seed", "presence_penalty", "frequency_penalty", "logit_bias", "n", "user", "reasoning", "reasoning_effort"}

var vendors = []vendor{
	{ID: KindOpenAI, Name: "OpenAI", Connector: KindOpenAI, Discovery: true, Operations: []string{OperationGeneration}, Authentication: []string{AuthAPIKey}, Parameters: generationParameters, DocumentationURL: "https://platform.openai.com/docs/api-reference", Endpoint: new(DefaultOpenAIEndpoint)},
	{ID: KindOpenAICompatible, Name: "OpenAI-compatible", Connector: KindOpenAICompatible, Discovery: true, Operations: []string{OperationGeneration}, Authentication: []string{AuthAPIKey, AuthHeaders, AuthNone}, Parameters: generationParameters, DocumentationURL: "https://platform.openai.com/docs/api-reference/chat", Endpoint: nil},
}

var CapabilityOptions = []CapabilityInput{
	{Operation: OperationGeneration, Surface: SurfaceOpenAI, Mode: ModeUnary},
	{Operation: OperationGeneration, Surface: SurfaceOpenAI, Mode: ModeStreaming},
}

// VendorKind maps a catalogue vendor identifier to the connector kind that
// serves it. Prices are published per vendor while every recorded attempt
// carries the kind of the provider that served it, so pricing selection
// applies a vendor-scoped price only where the two agree.
func VendorKind(vendor string) (string, bool) {
	for i := range vendors {
		if vendors[i].ID == vendor {
			return vendors[i].Connector, true
		}
	}
	return "", false
}

func kindByName(name string) *kindCapability {
	for i := range kinds {
		if kinds[i].Kind == name {
			return &kinds[i]
		}
	}
	return nil
}

func init() {
	custom := []authCapability{{Mode: AuthAPIKey, Label: "API key", Credential: "required"}, {Mode: AuthHeaders, Label: "Encrypted headers", Credential: "required"}, {Mode: AuthNone, Label: "No credential", Credential: "forbidden"}}
	kinds[0].AuthModes = custom
	for _, entry := range []struct{ kind, label string }{{KindAnthropic, "Anthropic"}, {KindGemini, "Google Gemini"}} {
		kinds = append(kinds, kindCapability{Kind: entry.kind, Label: entry.label, Description: "Native API, including custom endpoints.", DefaultAuthMode: AuthAPIKey, AuthModes: custom, Fields: []fieldCapability{{Field: "endpoint", Label: "Endpoint"}}, Presets: []preset{}})
	}
	kinds = append(kinds,
		kindCapability{Kind: KindAzure, Label: "Azure OpenAI", Description: "Azure deployment API.", DefaultAuthMode: AuthAPIKey, AuthModes: []authCapability{custom[0],
			{Mode: "azure_default", Label: "Microsoft Entra default credential", Credential: "forbidden"},
			{Mode: "azure_client_secret", Label: "Microsoft Entra client secret", Credential: "required"}},
			Fields: []fieldCapability{{Field: "endpoint", Label: "Resource origin", Required: true}, {Field: "deployment", Label: "Deployment", Required: true}, {Field: "api_version", Label: "API version", Required: true}}, Presets: []preset{}},
		kindCapability{Kind: KindVertex, Label: "Google Vertex AI", Description: "Vertex publisher generation API.", DefaultAuthMode: "adc", AuthModes: []authCapability{{Mode: "adc", Label: "Application default credentials", Credential: "forbidden"}, {Mode: "service_account", Label: "Service account JSON", Credential: "required"}}, Fields: []fieldCapability{{Field: "cloud_project", Label: "Project", Required: true}, {Field: "cloud_region", Label: "Location", Required: true}, {Field: "endpoint", Label: "Endpoint"}}, Presets: []preset{}},
		kindCapability{Kind: KindBedrock, Label: "Amazon Bedrock", Description: "Converse, ConverseStream and CountTokens.", DefaultAuthMode: "default_chain", AuthModes: []authCapability{{Mode: "default_chain", Label: "AWS credential chain", Credential: "forbidden"}, {Mode: "static", Label: "AWS credential JSON", Credential: "required"}}, Fields: []fieldCapability{{Field: "cloud_region", Label: "Region", Required: true}, {Field: "endpoint", Label: "Endpoint"}}, Presets: []preset{}},
	)
	for _, entry := range []struct {
		id, name, endpoint, docs string
		discovery                bool
		operations               []string
	}{
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", "https://api-docs.deepseek.com", true, []string{"generation"}},
		{"fireworks", "Fireworks", "https://api.fireworks.ai/inference/v1", "https://docs.fireworks.ai", true, []string{"generation"}},
		{"deepinfra", "DeepInfra", "https://api.deepinfra.com/v1/openai", "https://deepinfra.com/docs", true, []string{"generation"}},
		{"huggingface", "Hugging Face", "https://router.huggingface.co/v1", "https://huggingface.co/docs/inference-providers", true, []string{"generation"}},
		{"perplexity", "Perplexity", "https://api.perplexity.ai", "https://docs.perplexity.ai", false, []string{"generation"}},
		{"cohere", "Cohere", "https://api.cohere.ai/compatibility/v1", "https://docs.cohere.com", false, []string{"generation", "embeddings", "rerank"}},
		{"voyage", "Voyage AI", "https://api.voyageai.com/v1", "https://docs.voyageai.com", false, []string{"embeddings", "rerank"}},
	} {
		kinds[1].Presets = append(kinds[1].Presets, preset{ID: entry.id, Label: entry.name, Description: entry.name + " compatible API.", Endpoint: entry.endpoint, AuthMode: AuthAPIKey, Maintainer: entry.name, DocumentationLabel: entry.name + " API", DocumentationURL: entry.docs})
		vendors = append(vendors, vendor{ID: entry.id, Name: entry.name, Connector: KindOpenAICompatible, Discovery: entry.discovery, Operations: entry.operations, Authentication: []string{AuthAPIKey, AuthHeaders, AuthNone}, Parameters: generationParameters, DocumentationURL: entry.docs, Endpoint: new(entry.endpoint)})
	}
	for _, preset := range kinds[1].Presets {
		found := false
		for _, v := range vendors {
			found = found || v.ID == preset.ID
		}
		if !found {
			vendors = append(vendors, vendor{ID: preset.ID, Name: preset.Label, Connector: KindOpenAICompatible, Discovery: true, Operations: []string{"generation"}, Authentication: []string{AuthAPIKey, AuthHeaders, AuthNone}, Parameters: generationParameters, DocumentationURL: preset.DocumentationURL, Endpoint: new(preset.Endpoint)})
		}
	}
	for _, entry := range []struct{ id, kind, label, docs string }{{"anthropic", KindAnthropic, "Anthropic", "https://docs.anthropic.com"}, {"google", KindGemini, "Google Gemini", "https://ai.google.dev"}, {"google-vertex", KindVertex, "Google Vertex AI", "https://cloud.google.com/vertex-ai"}, {"amazon-bedrock", KindBedrock, "Amazon Bedrock", "https://docs.aws.amazon.com/bedrock"}, {"azure", KindAzure, "Azure OpenAI", "https://learn.microsoft.com/azure/ai-services/openai"}} {
		auth := []string{}
		for _, m := range kindByName(entry.kind).AuthModes {
			auth = append(auth, m.Mode)
		}
		endpoint := connectors.DefaultEndpoint(entry.kind, "", "")
		var ep *string
		if entry.kind == KindGemini || entry.kind == KindAnthropic {
			ep = &endpoint
		}
		vendors = append(vendors, vendor{ID: entry.id, Name: entry.label, Connector: entry.kind, Discovery: entry.kind != KindVertex, Operations: []string{"generation", "token_count"}, Authentication: auth, Parameters: generationParameters, DocumentationURL: entry.docs, Endpoint: ep})
	}
	vendors[0].Operations = []string{"generation", "embeddings", "token_count", "moderation"}
	vendors[1].Operations = vendors[0].Operations
	for i := range vendors {
		switch vendors[i].ID {
		case "azure":
			vendors[i].Operations = []string{"generation", "embeddings", "token_count", "moderation"}
		case "google":
			vendors[i].Operations = []string{"generation", "embeddings", "token_count"}
		case "google-vertex":
			vendors[i].Operations = []string{"generation", "embeddings", "token_count", "image_generation"}
		case "amazon-bedrock":
			vendors[i].Operations = []string{"generation", "embeddings", "token_count", "image_generation"}
		case "cohere":
			vendors[i].Parameters = []string{"temperature", "max_output_tokens", "top_p", "stop", "seed", "tools", "response_format", "encoding_format"}
		case "voyage":
			vendors[i].Parameters = []string{"dimensions", "input_type", "truncation", "output_dtype", "encoding_format"}
		}
	}

	for _, surface := range []string{"openai", "anthropic", "gemini"} {
		for _, mode := range []string{"unary", "streaming"} {
			if surface != "openai" {
				CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: "generation", Surface: surface, Mode: mode})
			}
		}
		CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: "token_count", Surface: surface, Mode: "unary"})
	}
	for _, operation := range []string{"embeddings", "moderation", "rerank"} {
		CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: operation, Surface: "openai", Mode: "unary"})
	}
	for _, operation := range []string{"image_generation", "image_edit", "speech", "transcription"} {
		for _, mode := range []string{"unary", "streaming"} {
			CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: operation, Surface: "openai", Mode: mode})
		}
	}
	for _, operation := range []string{"image_variation", "video_list", "video_get", "video_content", "video_delete"} {
		CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: operation, Surface: "openai", Mode: "unary"})
	}
	CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: "video_create", Surface: "openai", Mode: "async"})
	CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: "batch", Surface: "openai", Mode: "unary"})
	CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: "realtime", Surface: "openai", Mode: "realtime"})
	for _, operation := range []string{"generation", "bedrock_invoke"} {
		for _, mode := range []string{"unary", "streaming"} {
			CapabilityOptions = append(CapabilityOptions, CapabilityInput{Operation: operation, Surface: "bedrock", Mode: mode})
		}
	}

}
func defaultVendor(kind string) string {
	switch kind {
	case KindGemini:
		return "google"
	case KindVertex:
		return "google-vertex"
	case KindBedrock:
		return "amazon-bedrock"
	case KindAzure:
		return "azure"
	}
	return kind
}
func capabilitiesFor(kind, vendor string) []CapabilityInput {
	out := []CapabilityInput{}
	for _, c := range CapabilityOptions {
		if certifiable(kind, vendor, c) {
			out = append(out, c)
		}
	}
	return out
}

// Custom endpoints need a safe live probe; native media instead relies on the
// official connector contract and authenticated discovery, as in the Rust gateway.
func certifiable(kind, vendor string, c CapabilityInput) bool {
	if !connectors.Supports(kind, vendor, c.Operation, c.Surface, c.Mode) {
		return false
	}
	switch c.Operation {
	case "generation", "token_count", "embeddings", "moderation", "rerank":
		return true
	case "batch", "realtime":
		return kind == KindOpenAI || kind == KindAzure
	case "bedrock_invoke":
		return kind == KindBedrock
	case "image_generation":
		return kind == KindOpenAI || kind == KindVertex || kind == KindBedrock
	default:
		return kind == KindOpenAI
	}
}
