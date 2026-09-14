// Package providers owns provider connections: configuration drafts, model
// discovery and certification, credential pools, activation into immutable
// revisions, and history. Only OpenAI-compatible connections serve in this
// milestone; other kinds and media stay unavailable rather than pretending.
package providers

// Kind names accepted by the gateway.
const (
	KindOpenAI           = "openai"
	KindOpenAICompatible = "openai_compatible"
)

// Auth modes accepted by the gateway.
const (
	AuthAPIKey  = "api_key"
	AuthHeaders = "headers"
	AuthNone    = "none"
)

// Capability tuple vocabulary served in this milestone.
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

type capabilityInput struct {
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
	{ID: KindOpenAI, Name: "OpenAI", Connector: KindOpenAI, Discovery: true, Operations: []string{OperationGeneration}, Authentication: []string{AuthAPIKey}, Parameters: generationParameters, DocumentationURL: "https://platform.openai.com/docs/api-reference", Endpoint: ptr(DefaultOpenAIEndpoint)},
	{ID: KindOpenAICompatible, Name: "OpenAI-compatible", Connector: KindOpenAICompatible, Discovery: true, Operations: []string{OperationGeneration}, Authentication: []string{AuthAPIKey, AuthHeaders, AuthNone}, Parameters: generationParameters, DocumentationURL: "https://platform.openai.com/docs/api-reference/chat", Endpoint: nil},
}

// capabilityOptions lists the tuples a kind may declare and certify.
var capabilityOptions = []capabilityInput{
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

func ptr[T any](v T) *T { return &v }
