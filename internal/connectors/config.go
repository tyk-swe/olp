package connectors

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/retrieval/identity"
)

type Kind string

const (
	KindOpenAI           Kind = "openai"
	KindOpenAICompatible Kind = "openai_compatible"
	KindAnthropic        Kind = "anthropic"
	KindGemini           Kind = "gemini"
	KindAzureOpenAI      Kind = "azure_openai"
	KindVertexAI         Kind = "vertex_ai"
	KindBedrock          Kind = "bedrock"
	KindSageMaker        Kind = "sagemaker"
	KindWatsonx          Kind = "watsonx"
	KindAzureAISearch    Kind = Kind(identity.Azure)
	KindPlugin           Kind = "plugin"
)

type Config struct {
	CredentialSource         string
	Network                  *egress.ConnectionOptions
	Plugin                   *PluginProfile
	PluginOptions            map[string]string
	ProfileID                string
	ProfileRevision          string
	SemanticHeaders          map[string]string
	QuerySettings            map[string]string
	OperationDefaults        map[string]DefaultSet
	Bindings                 map[string]Binding
	Kind                     Kind
	AuthMode                 AuthMode
	Endpoint                 string
	CloudRegion              string
	CloudProject             string
	Deployment               string
	APIVersion               string
	VendorID                 string
	BatchStorage             *BatchStorage
	TuningDatasetURIPrefixes []string
	DocumentURIPrefixes      []string
	SigningService           string `json:"-"`
	CredentialHeaders        []string
	Models                   map[string]json.RawMessage
	ObservedPrincipal        string
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var cloudIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
var bedrockModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func ModelValid(kind Kind, model string) bool {
	if kind == KindSageMaker {
		return sagemakerModelValid(model)
	}
	if kind == KindWatsonx {
		return watsonxModelValid(model)
	}
	if kind == KindBedrock {
		return len(model) <= 2048 && bedrockModel.MatchString(model) && !strings.Contains(model, "..")
	}
	if kind == KindAzureOpenAI || kind == KindGemini || kind == KindVertexAI {
		return identifier.MatchString(strings.TrimPrefix(model, "models/")) && !strings.HasSuffix(model, ".")
	}
	return model != "" && !strings.ContainsAny(model, "\x00\r\n")
}

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

func (c Config) ServingPrincipal(model string) string {
	if c.ObservedPrincipal != "" {
		return c.ObservedPrincipal
	}
	return c.Bindings[model].PrincipalID
}
