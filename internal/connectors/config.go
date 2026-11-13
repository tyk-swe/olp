// Package connectors owns provider addressing and the preparation of upstream
// requests: hosting, authentication and signing. It does not retry inference
// calls: the gateway's attempt executor is the only retry owner.
package connectors

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
)

const (
	KindOpenAI           = "openai"
	KindOpenAICompatible = "openai_compatible"
	KindAnthropic        = "anthropic"
	KindGemini           = "gemini"
	KindAzureOpenAI      = "azure_openai"
	KindVertexAI         = "vertex_ai"
	KindBedrock          = "bedrock"
)

type Config struct {
	CredentialSource string
	Network          *egress.ConnectionOptions
	// Plugin is the plugin profile a plugin provider pins as its profile
	// revision, or nil for any other provider. PluginOptions holds the
	// provider's values for the options that profile declares, by name.
	Plugin                                                                                *PluginProfile
	PluginOptions                                                                         map[string]string
	ProfileID, ProfileRevision                                                            string
	SemanticHeaders                                                                       map[string]string
	QuerySettings                                                                         map[string]string
	OperationDefaults                                                                     map[string]DefaultSet
	Bindings                                                                              map[string]Binding
	Kind, AuthMode, Endpoint, CloudRegion, CloudProject, Deployment, APIVersion, VendorID string
	BatchStorage                                                                          *BatchStorage
	TuningDatasetURIPrefixes                                                              []string
	DocumentURIPrefixes                                                                   []string
	// SigningService overrides the SigV4 signing name of one call to an AWS
	// service beside the connector's own, such as Amazon Polly's.
	SigningService    string `json:"-"`
	CredentialHeaders []string
	Models            map[string]json.RawMessage
	// ObservedPrincipal is the upstream principal that grant enrollment
	// observed for every credential slot of a provider authenticated by a
	// grant, or empty.
	ObservedPrincipal string
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var cloudIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
var bedrockModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func ModelValid(kind, model string) bool {
	if kind == KindSageMaker {
		return sagemakerModelValid(model)
	}
	if kind == KindWatsonx {
		return watsonxModelValid(model)
	}
	if kind == "bedrock" {
		return len(model) <= 2048 && bedrockModel.MatchString(model) && !strings.Contains(model, "..")
	}
	if kind == "azure_openai" || kind == "gemini" || kind == "vertex_ai" {
		return identifier.MatchString(strings.TrimPrefix(model, "models/")) && !strings.HasSuffix(model, ".")
	}
	return model != "" && !strings.ContainsAny(model, "\x00\r\n")
}
func (c Config) Validate(policy *egress.Policy) error {
	if err := policy.ValidateConnection(c.Network); err != nil {
		return err
	}
	if err := c.ValidateProfile(); err != nil {
		return err
	}
	u, e := policy.ValidateEndpoint(c.Endpoint)
	if e != nil {
		return e
	}
	if c.Kind == "vertex_ai" && !c.vertexDestination(u) {
		return errors.New("Vertex credentials require the Google endpoint for the configured location and system trust roots")
	}
	if err := c.validateProfileEndpoint(u); err != nil {
		return err
	}
	if hosting := c.Hosting(); hosting == "azure-v1" || hosting == "azure-responses-legacy" {
		return nil
	}
	switch c.Kind {
	case KindAzureAISearch:
		if c.ProfileID != "azure-ai-search" || u.Path != "" || c.CloudRegion != "" || c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "2025-09-01" {
			return errors.New("Azure AI Search requires its dedicated profile, a service origin and api_version 2025-09-01")
		}
	case "azure_openai":
		if u.Path != "" {
			return errors.New("Azure endpoint must be a resource origin")
		}
		if !ModelValid(c.Kind, c.Deployment) {
			return errors.New("Azure deployment must be a valid deployment identifier")
		}
		version := strings.TrimSuffix(c.APIVersion, "-preview")
		date, e := time.Parse("2006-01-02", version)
		if e != nil || date.Year() < 2020 {
			return errors.New("Azure api_version must be a valid YYYY-MM-DD or YYYY-MM-DD-preview")
		}
		if c.CloudRegion != "" || c.CloudProject != "" {
			return errors.New("Azure uses endpoint, deployment and API version")
		}
	case "vertex_ai":
		if !cloudIdentifier.MatchString(c.CloudRegion) || !cloudIdentifier.MatchString(c.CloudProject) {
			return errors.New("Vertex requires a project and location")
		}
		if c.Deployment != "" || c.APIVersion != "" {
			return errors.New("Vertex deployment and API version belong in the native endpoint")
		}
	case KindSageMaker:
		if !cloudIdentifier.MatchString(c.CloudRegion) {
			return errors.New("SageMaker requires an AWS region")
		}
		if u.Path != "" || c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "" {
			return errors.New("SageMaker uses a runtime endpoint origin and region; models name the endpoint")
		}
	case KindWatsonx:
		if !cloudIdentifier.MatchString(c.CloudRegion) || !cloudIdentifier.MatchString(c.CloudProject) {
			return errors.New("watsonx requires a region and a project ID")
		}
		if u.Path != "" || c.Deployment != "" {
			return errors.New("watsonx uses a regional endpoint origin, region and project")
		}
		if date, err := time.Parse(time.DateOnly, c.APIVersion); c.APIVersion != "" && (err != nil || date.Year() < 2024) {
			return errors.New("watsonx api_version must be a YYYY-MM-DD date")
		}
	case "bedrock":
		if !cloudIdentifier.MatchString(c.CloudRegion) {
			return errors.New("Bedrock requires an AWS region")
		}
		if c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "" {
			return errors.New("Bedrock uses endpoint and region")
		}
	default:
		if c.CloudRegion != "" || c.CloudProject != "" || c.Deployment != "" || c.APIVersion != "" {
			return errors.New("Cloud context applies only to cloud connectors")
		}
	}
	return nil
}

// Model is the upstream model a client model names: its binding's deployment
// or model, its metadata deployment, or the name itself. A Gemini-style
// "models/" resource prefix is dropped only for connector kinds whose model
// validation recognizes that resource syntax.
func (c Config) Model(model string) string {
	model = c.boundModel(model)
	if c.Kind == KindGemini || c.Kind == KindVertexAI || c.Kind == KindAzureOpenAI {
		return strings.TrimPrefix(model, "models/")
	}
	return model
}

func (c Config) boundModel(model string) string {
	if binding, ok := c.Bindings[model]; ok {
		if binding.Deployment != "" {
			return binding.Deployment
		}
		if binding.Model != "" {
			return binding.Model
		}
	}
	if deployment := modelDeployment(c.Models[model]); deployment != "" {
		return deployment
	}
	return model
}

func modelDeployment(metadata json.RawMessage) string {
	if len(metadata) == 0 {
		return ""
	}
	var declared struct {
		Deployment string `json:"deployment"`
	}
	_ = json.Unmarshal(metadata, &declared)
	return declared.Deployment
}

// ServingPrincipal is the upstream principal that serves a model, part of its
// serving identity: the provider's observed principal, which replaces any
// the model's serving binding declares, else the declared one, or "" when
// the principal is unknown.
func (c Config) ServingPrincipal(model string) string {
	if c.ObservedPrincipal != "" {
		return c.ObservedPrincipal
	}
	return c.Bindings[model].PrincipalID
}
