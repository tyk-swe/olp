package connection

import (
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/vendors"
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

var (
	apiKeyAuth  = authCapability{Mode: connectors.AuthAPIKey, Label: "API key", Credential: "required"}
	headersAuth = authCapability{Mode: connectors.AuthHeaders, Label: "Encrypted headers", Credential: "required"}
	noAuth      = authCapability{Mode: connectors.AuthNone, Label: "No credential", Credential: "forbidden"}
	endpoint    = fieldCapability{Field: "endpoint", Label: "Endpoint"}
)

// Kinds is the connector kind catalogue. Each kind's presets are the reviewed
// vendor contracts that offer themselves as its presets.
var Kinds = withPresets([]kindCapability{
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

// VendorCatalogue is what /provider-vendors publishes: every reviewed vendor
// contract, authenticated as its connector kind allows.
var VendorCatalogue = publishedVendors()

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
		for _, mode := range KindByName(c.Connector).AuthModes {
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

func KindByName(name string) *kindCapability {
	i := slices.IndexFunc(Kinds, func(k kindCapability) bool { return k.Kind == name })
	if i < 0 {
		return nil
	}
	return &Kinds[i]
}
