package connectors

import (
	"regexp"
	"strings"
)

type hostingTraits struct {
	baseSuffix        string
	deployment        bool
	apiVersion        bool
	resourceResponses bool
	azureScope        string
	awsService        string
	eventStream       bool
	awsBearer         bool
	model             *regexp.Regexp
	googleService     string
	vertexPath        string
	misplaced         string
	messages          upstreamPath
}

const (
	cognitiveServicesScope = "https://cognitiveservices.azure.com/.default"
	vertexMisplaced        = "Vertex endpoint path must match the profile's publisher or endpoint, project and location"
)

var hostings = map[string]hostingTraits{
	"azure-ai-search":          {azureScope: "https://search.azure.com/.default"},
	"vertex-rag-engine":        {googleService: vertexAI, misplaced: "Vertex RAG endpoint must match its configured project and location"},
	"google-document-ai":       {googleService: documentAI, misplaced: "Document AI endpoint must match its configured project and location"},
	"bedrock-knowledge-bases":  {awsService: "bedrock"},
	"azure-deployment":         {deployment: true, apiVersion: true, azureScope: cognitiveServicesScope},
	"azure-responses-legacy":   {deployment: true, apiVersion: true, resourceResponses: true, azureScope: cognitiveServicesScope},
	"azure-v1":                 {baseSuffix: "/openai/v1", azureScope: "https://ai.azure.com/.default"},
	"bedrock-converse":         {awsService: "bedrock", eventStream: true},
	"bedrock-anthropic-invoke": {awsService: "bedrock", eventStream: true, messages: bedrockInvokePath},
	"bedrock-invoke":           {awsService: "bedrock"},
	"bedrock-batch-inference":  {awsService: "bedrock"},
	"bedrock-nova-sonic":       {awsService: "bedrock", eventStream: true},
	"sagemaker-openai":         {awsService: "sagemaker", awsBearer: true},
	"vertex-google":            {googleService: vertexAI, vertexPath: "/publishers/google", misplaced: vertexMisplaced},
	"vertex-anthropic": {googleService: vertexAI, vertexPath: "/publishers/anthropic", misplaced: vertexMisplaced,
		messages: upstreamPath{unary: "/models/{model}:rawPredict", streaming: "/models/{model}:streamRawPredict"}},
	"vertex-openai": {googleService: vertexAI, vertexPath: "/endpoints/openapi", misplaced: vertexMisplaced,
		model: regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}/[A-Za-z0-9][A-Za-z0-9._@:-]{0,127}$`)},
}

var automaticHostings = map[string]string{
	KindAzureOpenAI: "azure-deployment",
	KindBedrock:     "bedrock-invoke",
	KindSageMaker:   "sagemaker-openai",
}

func (c Config) traits() hostingTraits {
	if c.ProfileID == "" {
		return hostings[automaticHostings[c.Kind]]
	}
	return hostings[c.Hosting()]
}

func (c Config) ValidModel(model string) bool {
	if c.RetrievalBackend() != "" {
		return c.retrievalModelValid(model)
	}
	if syntax := c.traits().model; syntax != nil {
		return syntax.MatchString(model) && !strings.Contains(model, "..")
	}
	return ModelValid(c.Kind, model)
}

func (c Config) azureScope() string {
	if scope := c.traits().azureScope; scope != "" {
		return scope
	}
	return cognitiveServicesScope
}

func (c Config) awsService() string {
	if c.SigningService != "" {
		return c.SigningService
	}
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
