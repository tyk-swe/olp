package connectors

import "strings"

// hostingTraits are the addressing, authentication and framing facts of a
// hosting. Connector methods read them instead of naming hostings, so a new
// hosting is one entry here rather than a branch in each method.
type hostingTraits struct {
	// baseSuffix is appended to the endpoint unless it already ends with it.
	baseSuffix string
	// deployment addresses calls through an Azure deployment: the
	// /openai/deployments/{deployment} prefix and the api-version query.
	deployment bool
	// azureScope is the Microsoft Entra scope of the hosting's tokens.
	azureScope string
	// awsService is the SigV4 signing name of the hosting's requests.
	awsService string
	// eventStream reports AWS event-stream framed streaming responses.
	eventStream bool
}

const cognitiveServicesScope = "https://cognitiveservices.azure.com/.default"

var hostings = map[string]hostingTraits{
	"azure-deployment":         {deployment: true, azureScope: cognitiveServicesScope},
	"azure-responses-legacy":   {deployment: true, azureScope: cognitiveServicesScope},
	"azure-v1":                 {baseSuffix: "/openai/v1", azureScope: "https://ai.azure.com/.default"},
	"bedrock-converse":         {awsService: "bedrock", eventStream: true},
	"bedrock-anthropic-invoke": {awsService: "bedrock", eventStream: true},
	"bedrock-invoke":           {awsService: "bedrock"},
}

// automaticHostings are the traits of a provider without a profile, by kind.
var automaticHostings = map[string]hostingTraits{
	"azure_openai": {deployment: true, azureScope: cognitiveServicesScope},
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
