package connection

import (
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/vendors"
)

type CredentialPolicy string

const (
	CredentialRequired  CredentialPolicy = "required"
	CredentialForbidden CredentialPolicy = "forbidden"
	CredentialGrant     CredentialPolicy = "grant"
)

func credentialPolicy(mode string) CredentialPolicy {
	switch {
	case mode == connectors.AuthGrant:
		return CredentialGrant
	case connectors.SecretRequired(mode):
		return CredentialRequired
	default:
		return CredentialForbidden
	}
}

type AuthOption struct {
	Mode       string           `json:"mode"`
	Label      string           `json:"label"`
	Credential CredentialPolicy `json:"credential"`
}

func auth(mode, label string) AuthOption {
	return AuthOption{Mode: mode, Label: label, Credential: credentialPolicy(mode)}
}

type Field struct {
	Field    string `json:"field"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
}

type Kind struct {
	Name            string       `json:"kind"`
	Label           string       `json:"label"`
	Description     string       `json:"description"`
	DefaultAuthMode string       `json:"default_auth_mode"`
	AuthModes       []AuthOption `json:"auth_modes"`
	Fields          []Field      `json:"fields"`
}

func (k Kind) Offers(mode string) bool {
	return slices.ContainsFunc(k.AuthModes, func(option AuthOption) bool { return option.Mode == mode })
}

type ListedKind struct {
	Kind
	Presets []Preset `json:"presets"`
}

type Preset struct {
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
	Discovery   bool `json:"discovery"`
	Placeholder bool `json:"placeholder"`
	// ProfileID and ProfileRevision name the profile onboarding selects, so
	// the provider can serve strict routes. The server never infers it.
	ProfileID       *string `json:"profile_id"`
	ProfileRevision *string `json:"profile_revision"`
}

type Vendor struct {
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
	Dialects              []string `json:"dialects"`
	UnsupportedParameters []string `json:"unsupported_parameters"`
}

var (
	apiKeyAuth  = auth(connectors.AuthAPIKey, "API key")
	headersAuth = auth(connectors.AuthHeaders, "Encrypted headers")
	noAuth      = auth(connectors.AuthNone, "No credential")
	endpoint    = Field{Field: "endpoint", Label: "Endpoint"}
)

var kinds = []Kind{
	{Name: connectors.KindOpenAI, Label: "OpenAI", Description: "OpenAI platform API with Chat Completions and Responses.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []AuthOption{apiKeyAuth, headersAuth, noAuth}, Fields: []Field{endpoint}},
	{Name: connectors.KindOpenAICompatible, Label: "OpenAI-compatible", Description: "Any server implementing the OpenAI Chat Completions or Responses API.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []AuthOption{auth(connectors.AuthAPIKey, "Bearer API key"), auth(connectors.AuthHeaders, "Custom credential headers"), noAuth},
		Fields:    []Field{{Field: "endpoint", Label: "Endpoint", Required: true}}},
	{Name: connectors.KindAnthropic, Label: "Anthropic", Description: "Native API, including custom endpoints.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []AuthOption{apiKeyAuth, headersAuth, noAuth}, Fields: []Field{endpoint}},
	{Name: connectors.KindGemini, Label: "Google Gemini", Description: "Native API, including custom endpoints.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []AuthOption{apiKeyAuth, headersAuth, noAuth}, Fields: []Field{endpoint}},
	{Name: connectors.KindAzureOpenAI, Label: "Azure OpenAI", Description: "Azure deployment API.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []AuthOption{apiKeyAuth, auth(connectors.AuthAzureDefault, "Microsoft Entra default credential"), auth(connectors.AuthAzureClientSecret, "Microsoft Entra client secret")},
		Fields:    []Field{{Field: "endpoint", Label: "Resource origin", Required: true}, {Field: "deployment", Label: "Deployment", Required: true}, {Field: "api_version", Label: "API version", Required: true}}},
	{Name: connectors.KindAzureAISearch, Label: "Azure AI Search", Description: "Query-only operator-managed search indexes.", DefaultAuthMode: connectors.AuthAPIKey,
		AuthModes: []AuthOption{auth(connectors.AuthAPIKey, "API key"), auth(connectors.AuthAzureDefault, "Azure identity"), auth(connectors.AuthAzureClientSecret, "Service principal JSON")},
		Fields:    []Field{{Field: "endpoint", Label: "Search service origin", Required: true}, {Field: "api_version", Label: "API version", Required: true}, {Field: "model", Label: "Index name", Required: true}}},
	{Name: connectors.KindVertexAI, Label: "Google Vertex AI", Description: "Vertex publisher generation API.", DefaultAuthMode: connectors.AuthADC,
		AuthModes: []AuthOption{auth(connectors.AuthADC, "Application default credentials"), auth(connectors.AuthServiceAccount, "Service account JSON")},
		Fields:    []Field{{Field: "cloud_project", Label: "Project", Required: true}, {Field: "cloud_region", Label: "Location", Required: true}, endpoint, {Field: "batch_storage", Label: "Batch storage"}}},
	{Name: connectors.KindBedrock, Label: "Amazon Bedrock", Description: "Converse, ConverseStream and CountTokens.", DefaultAuthMode: connectors.AuthDefaultChain,
		AuthModes: []AuthOption{auth(connectors.AuthDefaultChain, "AWS credential chain"), auth(connectors.AuthStatic, "AWS credential JSON")},
		Fields:    []Field{{Field: "cloud_region", Label: "Region", Required: true}, endpoint, {Field: "batch_storage", Label: "Batch storage"}}},
	// A SageMaker provider's models are its endpoints, or endpoint/component
	// for inference components, which operators declare.
	{Name: connectors.KindSageMaker, Label: "Amazon SageMaker AI", Description: "Real-time endpoints serving OpenAI Chat Completions.", DefaultAuthMode: connectors.AuthDefaultChain,
		AuthModes: []AuthOption{auth(connectors.AuthDefaultChain, "AWS credential chain"), auth(connectors.AuthStatic, "AWS credential JSON")},
		Fields:    []Field{{Field: "cloud_region", Label: "Region", Required: true}, endpoint, {Field: "model", Label: "Probe model", Required: true}}},
	{Name: connectors.KindWatsonx, Label: "IBM watsonx.ai", Description: "Chat API with IBM Cloud IAM authentication.", DefaultAuthMode: connectors.AuthIBMIAM,
		AuthModes: []AuthOption{auth(connectors.AuthIBMIAM, "IBM Cloud API key")},
		Fields: []Field{{Field: "cloud_region", Label: "Region", Required: true}, {Field: "cloud_project", Label: "Project ID", Required: true},
			endpoint, {Field: "api_version", Label: "API version date", Required: false}}},
	// A plugin profile supplies the address, credential placement, model
	// discovery, and whether a static credential or a grant authenticates.
	// Without discovery, the operator names a model to probe, as the
	// profile catalogue's model_discovery tells.
	{Name: connectors.KindPlugin, Label: "Provider plugin", Description: "A profile an installed provider plugin supplies around a built-in dialect.", DefaultAuthMode: connectors.AuthStaticCredential,
		AuthModes: []AuthOption{auth(connectors.AuthStaticCredential, "Static credential"), auth(connectors.AuthGrant, "Grant")},
		Fields:    []Field{}},
}

func Kinds() []ListedKind {
	out := make([]ListedKind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, ListedKind{Kind: k, Presets: presetsOf(k.Name)})
	}
	return out
}

func LookupKind(name string) (Kind, bool) {
	i := slices.IndexFunc(kinds, func(k Kind) bool { return k.Name == name })
	if i < 0 {
		return Kind{}, false
	}
	return kinds[i], true
}

func presetsOf(kind string) []Preset {
	out := []Preset{}
	for _, c := range vendors.All() {
		if c.Connector != kind || c.Preset == nil {
			continue
		}
		entry := Preset{ID: c.ID, Label: c.Name, Description: c.Description, Endpoint: c.Endpoint, AuthMode: c.Preset.AuthMode,
			Maintainer: c.Maintainer, DocumentationLabel: c.Documentation.Label, DocumentationURL: c.Documentation.URL,
			Discovery: c.Discovery, Placeholder: c.Preset.Placeholder}
		if profile := c.Preset.Profile; profile != nil {
			entry.ProfileID, entry.ProfileRevision = new(profile.ID), new(profile.Revision)
		}
		out = append(out, entry)
	}
	return out
}

func Vendors() []Vendor {
	out := make([]Vendor, 0, len(vendors.All()))
	for _, c := range vendors.All() {
		kind, _ := LookupKind(c.Connector)
		auth := make([]string, 0, len(kind.AuthModes))
		for _, mode := range kind.AuthModes {
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
		out = append(out, Vendor{ID: c.ID, Name: c.Name, Connector: c.Connector, Discovery: c.Discovery, Operations: c.Operations, Authentication: auth,
			Parameters: c.Parameters, DocumentationURL: c.Documentation.URL, Endpoint: endpoint,
			Dialects: append([]string{}, c.Dialects...), UnsupportedParameters: append([]string{}, slices.Compact(unsupported)...)})
	}
	return out
}
