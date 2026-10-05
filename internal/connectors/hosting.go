package connectors

import (
	"regexp"
	"strings"
)

// hostingTraits are the addressing, authentication and framing facts of a
// hosting. Connector methods read them instead of naming hostings, so a new
// hosting is one entry here rather than a branch in each method.
type hostingTraits struct {
	// baseSuffix is appended to the endpoint unless it already ends with it.
	baseSuffix string
	// deployment addresses calls through an Azure deployment: the
	// /openai/deployments/{deployment} prefix.
	deployment bool
	// apiVersion carries the configured API version as the api-version
	// query of every call.
	apiVersion bool
	// azureScope is the Microsoft Entra scope of the hosting's tokens.
	azureScope string
	// awsService is the SigV4 signing name of the hosting's requests.
	awsService string
	// eventStream reports AWS event-stream framed streaming responses.
	eventStream bool
	// model is the syntax of the hosting's upstream model names, where it
	// differs from its kind's.
	model *regexp.Regexp
}

const cognitiveServicesScope = "https://cognitiveservices.azure.com/.default"

var hostings = map[string]hostingTraits{
	"azure-deployment":         {deployment: true, apiVersion: true, azureScope: cognitiveServicesScope},
	"azure-responses-legacy":   {deployment: true, apiVersion: true, azureScope: cognitiveServicesScope},
	"azure-v1":                 {baseSuffix: "/openai/v1", azureScope: "https://ai.azure.com/.default"},
	"bedrock-converse":         {awsService: "bedrock", eventStream: true},
	"bedrock-anthropic-invoke": {awsService: "bedrock", eventStream: true},
	"bedrock-invoke":           {awsService: "bedrock"},
	// Vertex's OpenAI-compatible endpoint serves Google, partner and open
	// models by publisher/model name.
	"vertex-openai": {model: regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}/[A-Za-z0-9][A-Za-z0-9._@:-]{0,127}$`)},
}

// automaticHostings are the traits of a provider without a profile, by kind.
var automaticHostings = map[string]hostingTraits{
	"azure_openai": {deployment: true, apiVersion: true, azureScope: cognitiveServicesScope},
	"bedrock":      {awsService: "bedrock"},
}

// traits are the hosting traits of the connector: its profile's hosting, or
// its kind's automatic hosting.
func (c Config) traits() hostingTraits {
	if c.ProfileID == "" {
		return automaticHostings[c.Kind]
	}
	return hostings[c.Hosting()]
}

// ValidModel reports whether model is an upstream model name the connector's
// hosting addresses.
func (c Config) ValidModel(model string) bool {
	if syntax := c.traits().model; syntax != nil {
		return syntax.MatchString(model) && !strings.Contains(model, "..")
	}
	return ModelValid(c.Kind, model)
}

// AzureScope is the Microsoft Entra scope of the connector's tokens.
func (c Config) AzureScope() string {
	if scope := c.traits().azureScope; scope != "" {
		return scope
	}
	return cognitiveServicesScope
}

// awsService is the SigV4 signing name of the connector's requests.
func (c Config) awsService() string {
	if service := c.traits().awsService; service != "" {
		return service
	}
	return "bedrock"
}

func (c Config) profileBase() string {
	base := strings.TrimRight(c.Endpoint, "/")
	if suffix := c.traits().baseSuffix; suffix != "" && !strings.HasSuffix(base, suffix) {
		base += suffix
	}
	return base
}
