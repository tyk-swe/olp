// Package providers owns provider connections: configuration drafts, model
// discovery and certification, credential pools, activation into immutable
// revisions, and history for the connector matrix.
package providers

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/providerconfig"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/vendors"
)

// Capability tuple vocabulary shared across providers.
const (
	OperationGeneration = "generation"
	SurfaceOpenAI       = "openai"
	ModeUnary           = "unary"
	ModeStreaming       = "streaming"
)

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
	// Discovery reports that the upstream lists its models; without it the
	// connection test needs a declared probe model.
	Discovery bool `json:"discovery"`
	// Placeholder marks an endpoint the operator replaces with their own.
	Placeholder bool `json:"placeholder"`
	// ProfileID and ProfileRevision name the profile onboarding selects, so
	// the provider can serve strict routes. The server never infers it.
	ProfileID       *string `json:"profile_id"`
	ProfileRevision *string `json:"profile_revision"`
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
	// Dialects are the generation dialects the vendor documents; profiles in
	// other generation dialects are refused for it.
	Dialects []string `json:"dialects"`
	// UnsupportedParameters are request fields refused for the vendor.
	UnsupportedParameters []string `json:"unsupported_parameters"`
}

type CapabilityInput struct {
	Operation string `json:"operation"`
	Surface   string `json:"surface"`
	Mode      string `json:"mode"`
}

var (
	apiKeyAuth  = authCapability{Mode: connectors.AuthAPIKey, Label: "API key", Credential: "required"}
	headersAuth = authCapability{Mode: connectors.AuthHeaders, Label: "Encrypted headers", Credential: "required"}
	noAuth      = authCapability{Mode: connectors.AuthNone, Label: "No credential", Credential: "forbidden"}
	endpoint    = fieldCapability{Field: "endpoint", Label: "Endpoint"}
)

// kinds is the connector kind catalogue. Each kind's presets are the reviewed
// vendor contracts that offer themselves as its presets.
var kinds = withPresets([]kindCapability{
	{Kind: connectors.KindOpenAI, Label: "OpenAI", Description: "OpenAI platform API with Chat Completions and Responses.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []authCapability{apiKeyAuth, headersAuth, noAuth}, Fields: []fieldCapability{endpoint}},
	{Kind: connectors.KindOpenAICompatible, Label: "OpenAI-compatible", Description: "Any server implementing the OpenAI Chat Completions or Responses API.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []authCapability{{Mode: connectors.AuthAPIKey, Label: "Bearer API key", Credential: "required"}, {Mode: connectors.AuthHeaders, Label: "Custom credential headers", Credential: "required"}, noAuth},
		Fields:    []fieldCapability{{Field: "endpoint", Label: "Endpoint", Required: true}}},
	{Kind: connectors.KindAnthropic, Label: "Anthropic", Description: "Native API, including custom endpoints.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []authCapability{apiKeyAuth, headersAuth, noAuth}, Fields: []fieldCapability{endpoint}},
	{Kind: connectors.KindGemini, Label: "Google Gemini", Description: "Native API, including custom endpoints.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []authCapability{apiKeyAuth, headersAuth, noAuth}, Fields: []fieldCapability{endpoint}},
	{Kind: connectors.KindAzureOpenAI, Label: "Azure OpenAI", Description: "Azure deployment API.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []authCapability{apiKeyAuth, {Mode: connectors.AuthAzureDefault, Label: "Microsoft Entra default credential", Credential: "forbidden"}, {Mode: connectors.AuthAzureClientSecret, Label: "Microsoft Entra client secret", Credential: "required"}},
		Fields:    []fieldCapability{{Field: "endpoint", Label: "Resource origin", Required: true}, {Field: "deployment", Label: "Deployment", Required: true}, {Field: "api_version", Label: "API version", Required: true}}},
	{Kind: connectors.KindAzureAISearch, Label: "Azure AI Search", Description: "Query-only operator-managed search indexes.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []authCapability{{Mode: connectors.AuthAPIKey, Label: "API key", Credential: "required"}, {Mode: connectors.AuthAzureDefault, Label: "Azure identity", Credential: "forbidden"}, {Mode: connectors.AuthAzureClientSecret, Label: "Service principal JSON", Credential: "required"}},
		Fields:    []fieldCapability{{Field: "endpoint", Label: "Search service origin", Required: true}, {Field: "api_version", Label: "API version", Required: true}, {Field: "model", Label: "Index name", Required: true}}},
	{Kind: connectors.KindVertexAI, Label: "Google Vertex AI", Description: "Vertex publisher generation API.", DefaultAuthMode: connectors.AuthADC,
		AuthModes: []authCapability{{Mode: connectors.AuthADC, Label: "Application default credentials", Credential: "forbidden"}, {Mode: connectors.AuthServiceAccount, Label: "Service account JSON", Credential: "required"}},
		Fields:    []fieldCapability{{Field: "cloud_project", Label: "Project", Required: true}, {Field: "cloud_region", Label: "Location", Required: true}, endpoint, {Field: "batch_storage", Label: "Batch storage"}}},
	{Kind: connectors.KindBedrock, Label: "Amazon Bedrock", Description: "Converse, ConverseStream and CountTokens.", DefaultAuthMode: connectors.AuthDefaultChain,
		AuthModes: []authCapability{{Mode: connectors.AuthDefaultChain, Label: "AWS credential chain", Credential: "forbidden"}, {Mode: connectors.AuthStatic, Label: "AWS credential JSON", Credential: "required"}},
		Fields:    []fieldCapability{{Field: "cloud_region", Label: "Region", Required: true}, endpoint, {Field: "batch_storage", Label: "Batch storage"}}},
	// A SageMaker provider's models are its endpoints, or endpoint/component
	// for inference components, which operators declare.
	{Kind: connectors.KindSageMaker, Label: "Amazon SageMaker AI", Description: "Real-time endpoints serving OpenAI Chat Completions.", DefaultAuthMode: connectors.AuthDefaultChain,
		AuthModes: []authCapability{{Mode: connectors.AuthDefaultChain, Label: "AWS credential chain", Credential: "forbidden"}, {Mode: connectors.AuthStatic, Label: "AWS credential JSON", Credential: "required"}},
		Fields:    []fieldCapability{{Field: "cloud_region", Label: "Region", Required: true}, endpoint, {Field: "model", Label: "Probe model", Required: true}}},
	{Kind: connectors.KindWatsonx, Label: "IBM watsonx.ai", Description: "Chat API with IBM Cloud IAM authentication.", DefaultAuthMode: connectors.AuthIBMIAM,
		AuthModes: []authCapability{{Mode: connectors.AuthIBMIAM, Label: "IBM Cloud API key", Credential: "required"}},
		Fields: []fieldCapability{{Field: "cloud_region", Label: "Region", Required: true}, {Field: "cloud_project", Label: "Project ID", Required: true},
			endpoint, {Field: "api_version", Label: "API version date", Required: false}}},
	// A plugin profile supplies the address, credential placement, model
	// discovery, and whether a static credential or a grant authenticates.
	// Without discovery, the operator names a model to probe, as the
	// profile catalogue's model_discovery tells.
	{Kind: connectors.KindPlugin, Label: "Provider plugin", Description: "A profile an installed provider plugin supplies around a built-in dialect.", DefaultAuthMode: connectors.AuthStaticCredential,
		AuthModes: []authCapability{{Mode: connectors.AuthStaticCredential, Label: "Static credential", Credential: "required"}, {Mode: connectors.AuthGrant, Label: "Grant", Credential: "grant"}},
		Fields:    []fieldCapability{}},
})

// vendorCatalogue is what /provider-vendors publishes: every reviewed vendor
// contract, authenticated as its connector kind allows.
var vendorCatalogue = publishedVendors()

var capabilityVocabulary = capabilityOptions()

func withPresets(catalogue []kindCapability) []kindCapability {
	for i := range catalogue {
		catalogue[i].Presets = []preset{}
		for _, c := range vendors.All() {
			if c.Connector != catalogue[i].Kind || c.Preset == nil {
				continue
			}
			entry := preset{ID: c.ID, Label: c.Name, Description: c.Description, Endpoint: c.Endpoint, AuthMode: c.Preset.AuthMode,
				Maintainer: c.Maintainer, DocumentationLabel: c.Documentation.Label, DocumentationURL: c.Documentation.URL,
				Discovery: c.Discovery, Placeholder: c.Preset.Placeholder}
			if profile := c.Preset.Profile; profile != nil {
				entry.ProfileID, entry.ProfileRevision = new(profile.ID), new(profile.Revision)
			}
			catalogue[i].Presets = append(catalogue[i].Presets, entry)
		}
	}
	return catalogue
}

func publishedVendors() []vendor {
	out := []vendor{}
	for _, c := range vendors.All() {
		auth := []string{}
		for _, mode := range kindByName(c.Connector).AuthModes {
			auth = append(auth, mode.Mode)
		}
		var endpoint *string
		if c.Endpoint != "" {
			endpoint = new(c.Endpoint)
		}
		unsupported := slices.Clone(c.Unsupported)
		for _, shape := range c.Requests {
			unsupported = append(unsupported, shape.Unsupported...)
		}
		slices.Sort(unsupported)
		out = append(out, vendor{ID: c.ID, Name: c.Name, Connector: c.Connector, Discovery: c.Discovery, Operations: c.Operations, Authentication: auth,
			Parameters: c.Parameters, DocumentationURL: c.Documentation.URL, Endpoint: endpoint,
			Dialects: append([]string{}, c.Dialects...), UnsupportedParameters: append([]string{}, slices.Compact(unsupported)...)})
	}
	return out
}

func capabilityOptions() []CapabilityInput {
	tuple := func(operation, surface, mode string) CapabilityInput {
		return CapabilityInput{Operation: operation, Surface: surface, Mode: mode}
	}
	out := []CapabilityInput{tuple(OperationGeneration, SurfaceOpenAI, ModeUnary), tuple(OperationGeneration, SurfaceOpenAI, ModeStreaming)}
	for _, surface := range []string{"openai", "anthropic", "gemini"} {
		if surface != "openai" {
			out = append(out, tuple("generation", surface, "unary"), tuple("generation", surface, "streaming"))
		}
		out = append(out, tuple("token_count", surface, "unary"))
	}
	out = append(out, tuple("batch", "gemini", "unary"))
	for _, operation := range []string{"embeddings", "moderation", "rerank", "translation", "decision"} {
		out = append(out, tuple(operation, "openai", "unary"))
	}
	for _, operation := range []string{"ocr", "search"} {
		out = append(out, tuple(operation, "openai", "unary"), tuple(operation, "native", "unary"))
	}
	for _, operation := range []string{"image_generation", "image_edit", "speech", "transcription"} {
		out = append(out, tuple(operation, "openai", "unary"), tuple(operation, "openai", "streaming"))
	}
	for _, operation := range []string{"image_variation", "video_list", "video_get", "video_content", "video_delete"} {
		out = append(out, tuple(operation, "openai", "unary"))
	}
	out = append(out, tuple("video_create", "openai", "async"), tuple("batch", "openai", "unary"), tuple("batch", "anthropic", "unary"), tuple("realtime", "openai", "realtime"), tuple("realtime", "openai", "webrtc"), tuple("realtime", "gemini", "realtime"), tuple("realtime", "native", "realtime"))
	for _, operation := range []string{"generation", "bedrock_invoke"} {
		out = append(out, tuple(operation, "bedrock", "unary"), tuple(operation, "bedrock", "streaming"))
	}
	// Native generation dialects, which only their profiles serve.
	out = append(out, tuple("generation", "native", "unary"), tuple("generation", "native", "streaming"), tuple("batch", "native", "unary"))
	out = append(out, tuple("decision", "native", "unary"))
	out = append(out, tuple("agent_resource", "gemini", "unary"))
	for _, operation := range []string{"vector_store", "conversation", "container", "eval", "retrieval"} {
		out = append(out, tuple(operation, "openai", "unary"))
	}
	out = append(out, tuple("fine_tuning", "openai", "unary"), tuple("fine_tuning", "native", "unary"),
		tuple("skill", "anthropic", "unary"), tuple("context_cache", "gemini", "unary"))
	return out
}

// VendorKind maps a catalogue vendor identifier to the connector kind that
// serves it. Prices are published per vendor while every recorded attempt
// carries the kind of the provider that served it, so pricing selection
// applies a vendor-scoped price only where the two agree.

func kindByName(name string) *kindCapability {
	i := slices.IndexFunc(kinds, func(k kindCapability) bool { return k.Kind == name })
	if i < 0 {
		return nil
	}
	return &kinds[i]
}

// Kind options include tuples available through an explicit profile, even
// when the kind's Automatic connector cannot serve them.
func capabilityOptionsForKind(kind string) []CapabilityInput {
	var profiles []Configuration
	for _, profile := range connectors.Profiles() {
		if profile.Kind == kind {
			cfg := Configuration{Identity: providerconfig.Identity[*string]{Kind: kind, ProfileID: profile.ID, ProfileRevision: profile.Revision}}
			if kind == connectors.KindOpenAICompatible && strings.HasSuffix(profile.ID, "-files-batches") {
				vendor := strings.TrimSuffix(profile.ID, "-files-batches")
				cfg.Options.VendorID = &vendor
			}
			profiles = append(profiles, cfg)
		}
	}
	out := []CapabilityInput{}
	for _, tuple := range capabilityVocabulary {
		profiled := slices.ContainsFunc(profiles, func(cfg Configuration) bool {
			return configurationCertifiable(&cfg, tuple) && cfg.transport().Supports(tuple.Operation, tuple.Surface, tuple.Mode)
		})
		if certifiable(kind, vendors.DefaultFor(kind), tuple) || profiled {
			out = append(out, tuple)
		}
	}
	return out
}

// Custom endpoints need a safe live probe; native media instead relies on the
// official connector contract and authenticated discovery.
// reviewedMedia reports a reviewed vendor's media operation through its own
// connector, which certifies costlessly: by its model listing or its account
// probe, beside its codec contract. No reviewed vendor streams media.
func reviewedMedia(kind, vendor, operation string) bool {
	contract, ok := vendors.Lookup(vendor)
	if !ok || contract.Connector != kind || !contract.Serves(operation) {
		return false
	}
	return contract.AccountProbe != "" || contract.Discovery && vendors.MediaWire(vendor, operation) != ""
}

func certifiable(kind, vendor string, c CapabilityInput) bool {
	if c.Operation == "retrieval" && (kind == connectors.KindBedrock || kind == connectors.KindVertexAI || kind == connectors.KindAzureAISearch) {
		return false
	} // Cloud retrieval requires its dedicated profile.
	if retainedapi.Operation(c.Operation) {
		return connectors.Supports(kind, vendor, c.Operation, c.Surface, c.Mode)
	}
	if !connectors.Supports(kind, vendor, c.Operation, c.Surface, c.Mode) {
		return false
	}
	switch c.Operation {
	case "generation", "token_count", "embeddings", "moderation", "rerank":
		return true
	case "ocr", "search":
		return true
	case "batch":
		if c.Surface == "native" {
			return kind == connectors.KindVertexAI && vendor == "google-vertex" || kind == connectors.KindBedrock && vendor == "amazon-bedrock"
		}
		if c.Surface == "anthropic" {
			return kind == connectors.KindAnthropic && vendor == "anthropic"
		}
		if c.Surface == "gemini" {
			return kind == connectors.KindGemini && vendor == "google"
		}
		if c.Surface == "openai" && kind == connectors.KindGemini {
			return vendor == "google"
		}
		return kind == connectors.KindOpenAI || kind == connectors.KindAzureOpenAI || kind == connectors.KindOpenAICompatible &&
			(vendor == "groq" || vendor == "together" || vendor == "mistral")
	case "realtime":
		if c.Mode == "webrtc" {
			return kind == connectors.KindOpenAI
		}
		if c.Surface == "native" {
			return kind == connectors.KindOpenAICompatible && vendor == "xai" ||
				kind == connectors.KindVertexAI && vendor == "google-vertex" ||
				kind == connectors.KindBedrock && vendor == "amazon-bedrock"
		}
		return kind == connectors.KindOpenAI || kind == connectors.KindAzureOpenAI
	case "bedrock_invoke":
		return kind == connectors.KindBedrock
	default:
		// Reviewed vendor media is unary, but for the job a video creation
		// starts.
		vendorMode := c.Mode == ModeUnary || c.Operation == "video_create" && c.Mode == "async"
		return kind == connectors.KindOpenAI || (reviewedMedia(kind, vendor, c.Operation) || mediaByCall(kind, vendor, c.Operation)) && vendorMode
	}
}
