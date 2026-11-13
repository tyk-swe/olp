package connectors

import (
	"errors"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/vendors"
)

func DefaultProfileEndpoint(kind, profileID, region, project string) string {
	hosting := profileHosting(profileID)
	traits := hostings[hosting]
	switch {
	case traits.googleService == documentAI:
		if !cloudIdentifier.MatchString(region) || !cloudIdentifier.MatchString(project) {
			return ""
		}
		return googleLocation(documentAI, region, project)
	case kind == KindVertexAI && traits.googleService != "":
		return googleLocation(traits.googleService, region, project) + traits.vertexPath
	case hosting == "bedrock-knowledge-bases":
		return bedrockAgentRuntimeOrigin(DefaultEndpoint(kind, region, project))
	}
	return DefaultEndpoint(kind, region, project)
}

func DefaultEndpoint(kind, region, project string) string {
	switch kind {
	case KindVertexAI:
		return googleLocation(vertexAI, region, project) + hostings["vertex-google"].vertexPath
	case KindBedrock:
		endpoint, _ := BedrockEndpoint(region, false)
		return endpoint
	case KindSageMaker:
		endpoint, _ := sageMakerEndpoint(region)
		return endpoint
	case KindWatsonx:
		if !cloudIdentifier.MatchString(region) {
			return ""
		}
		return "https://" + region + ".ml.cloud.ibm.com"
	}
	contract, _ := vendors.Lookup(vendors.DefaultFor(kind))
	return contract.Endpoint
}

type upstreamPath struct {
	unary, streaming string
}

func (p upstreamPath) of(model string, stream bool) string {
	template := p.unary
	if stream && p.streaming != "" {
		template = p.streaming
	}
	return strings.Replace(template, "{model}", url.PathEscape(model), 1)
}

var chatCompletionsPath = upstreamPath{unary: "/chat/completions"}

var bedrockInvokePath = upstreamPath{unary: "/model/{model}/invoke", streaming: "/model/{model}/invoke-with-response-stream"}

var wirePaths = map[wire.Format]upstreamPath{
	wire.Decisions:             {unary: "/decisions"},
	wire.SystemOne:             {unary: "/systemone"},
	wire.Completions:           {unary: "/completions"},
	wire.Responses:             {unary: "/responses"},
	wire.InputTokens:           {unary: "/responses/input_tokens"},
	wire.Embeddings:            {unary: "/embeddings"},
	wire.Moderation:            {unary: "/moderations"},
	wire.Anthropic:             {unary: "/messages"},
	wire.AnthropicCount:        {unary: "/messages/count_tokens"},
	wire.MistralFIM:            {unary: "/fim/completions"},
	wire.CohereChat:            {unary: "/chat"},
	wire.Rerank:                {unary: "/rerank"},
	wire.Gemini:                {unary: "/models/{model}:generateContent", streaming: "/models/{model}:streamGenerateContent?alt=sse"},
	wire.GeminiCount:           {unary: "/models/{model}:countTokens", streaming: "/models/{model}:countTokens?alt=sse"},
	wire.GeminiEmbeddings:      {unary: "/models/{model}:embedContent"},
	wire.GeminiEmbeddingsBatch: {unary: "/models/{model}:batchEmbedContents"},
	wire.VertexEmbeddings:      {unary: "/models/{model}:predict"},
	wire.DocumentAIProcess:     {unary: "/processors/{model}:process"},
	wire.Bedrock:               {unary: "/model/{model}/converse", streaming: "/model/{model}/converse-stream"},
	"bedrock_count":            {unary: "/model/{model}/count-tokens"},
	wire.BedrockInvoke:         bedrockInvokePath,
	wire.BedrockEmbeddings:     {unary: bedrockInvokePath.unary},
}

func (c Config) URL(encodedWire wire.Format, model string, stream bool) (string, error) {
	if c.ProfileID != "" {
		if err := c.ValidateProfile(); err != nil {
			return "", err
		}
		expected, err := c.TargetFamily(encodedWire)
		batch := encodedWire == wire.GeminiEmbeddingsBatch && expected == wire.GeminiEmbeddings
		if err != nil || encodedWire != expected && !batch {
			return "", errors.New("wire dialect does not match the selected provider profile")
		}
	}
	upstream := c.Model(model)
	base, traits := c.profileBase(), c.traits()
	if !c.ValidModel(upstream) {
		return "", errors.New("invalid upstream model identifier")
	}
	switch {
	case c.Kind == KindSageMaker:
		return sagemakerURL(base, encodedWire, upstream)
	case c.Kind == KindWatsonx:
		return c.watsonxURL(base, encodedWire, stream)
	case encodedWire == wire.BedrockRerank:
		return c.bedrockAgentRuntime(base)
	case encodedWire == wire.Anthropic && traits.messages != (upstreamPath{}):
		return base + traits.messages.of(upstream, stream), nil
	}
	if encodedWire == wire.Rerank {
		if endpoint, ok := c.operationEndpoint("rerank", base); ok {
			return endpoint, nil
		}
	}
	path := chatCompletionsPath.of(upstream, stream)
	if declared, ok := wirePaths[encodedWire]; ok {
		path = declared.of(upstream, stream)
	}
	if traits.resourceResponses && (encodedWire == wire.Responses || encodedWire == wire.InputTokens) {
		return base + "/openai" + path + "?api-version=" + url.QueryEscape(c.APIVersion), nil
	}
	deployed, err := c.deploymentBase(base, upstream)
	if err != nil {
		return "", err
	}
	if traits.apiVersion {
		return deployed + path + "?api-version=" + url.QueryEscape(c.APIVersion), nil
	}
	return deployed + path, nil
}

func (c Config) operationEndpoint(operation, base string) (string, bool) {
	contract, ok := vendors.Lookup(c.VendorID)
	if !ok || base != strings.TrimRight(contract.Endpoint, "/") {
		return "", false
	}
	endpoint, ok := contract.OperationEndpoints[operation]
	return endpoint, ok
}

func (c Config) deploymentBase(base, deployment string) (string, error) {
	if !c.traits().deployment {
		return base, nil
	}
	if !c.servesDeployment(deployment) {
		return "", errors.New("model has no configured Azure deployment")
	}
	return base + "/openai/deployments/" + url.PathEscape(deployment), nil
}

func (c Config) servesDeployment(deployment string) bool {
	if deployment == c.Deployment {
		return true
	}
	for _, binding := range c.Bindings {
		if binding.Deployment == deployment {
			return true
		}
	}
	for _, metadata := range c.Models {
		if modelDeployment(metadata) == deployment {
			return true
		}
	}
	return false
}

func (c Config) hostingQuery() url.Values {
	if !c.traits().apiVersion {
		return url.Values{}
	}
	return url.Values{"api-version": {c.APIVersion}}
}

func withQuery(endpoint string, query url.Values) string {
	if len(query) == 0 {
		return endpoint
	}
	return endpoint + "?" + query.Encode()
}

func (c Config) MediaURL(path, model string, query url.Values) (string, error) {
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.ContainsAny(path, "\\?#") {
		return "", errors.New("invalid upstream resource path")
	}
	base, err := c.deploymentBase(c.profileBase(), c.Model(model))
	if err != nil {
		return "", err
	}
	merged := c.hostingQuery()
	maps.Copy(merged, query)
	return withQuery(base+"/"+path, merged), nil
}

func (c Config) ResourceURL(model, path string, query url.Values) (string, error) {
	if strings.Contains(path, "..") || strings.ContainsAny(path, "\\?#") {
		return "", errors.New("invalid upstream resource path")
	}
	base, resource, err := c.resourceRoot(model, path)
	if err != nil {
		return "", err
	}
	merged := c.hostingQuery()
	for name, values := range query {
		if existing, ok := merged[name]; ok && !slices.Equal(existing, values) {
			return "", errors.New("query collides with hosting API revision")
		}
	}
	maps.Copy(merged, query)
	return withQuery(base+"/"+strings.TrimPrefix(resource, "/"), merged), nil
}

func (c Config) resourceRoot(model, path string) (string, string, error) {
	base := c.profileBase()
	switch {
	case c.Kind == KindVertexAI && (path == "tuningJobs" || strings.HasPrefix(path, "tuningJobs/")):
		u, err := url.Parse(base)
		if err != nil {
			return "", "", err
		}
		location, _, _ := strings.Cut(u.Path, "/publishers/")
		located := *u
		located.Path = location
		return strings.TrimRight(located.String(), "/"), path, nil
	case !c.traits().deployment:
		return base, path, nil
	case !strings.HasPrefix(path, "deployments/"):
		return base + "/openai", path, nil
	}
	deployment := c.Model(model)
	if deployment == "" {
		return "", "", errors.New("model has no configured Azure deployment")
	}
	return base + "/openai/deployments/" + url.PathEscape(deployment), strings.TrimPrefix(path, "deployments/"), nil
}

func websocketURL(endpoint string) (string, bool) {
	if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		return "wss://" + rest, true
	}
	if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		return "ws://" + rest, true
	}
	return "", false
}

func (c Config) realtimeSocket(model string, query url.Values) (string, error) {
	endpoint, err := c.ResourceURL(model, "realtime", query)
	if err != nil {
		return "", err
	}
	socket, ok := websocketURL(endpoint)
	if !ok {
		return "", errors.New("realtime endpoint requires HTTP(S) hosting")
	}
	return socket, nil
}

func (c Config) RealtimeURL(model string) (string, error) {
	if c.ProfileID != "" && !c.Supports("realtime", "openai", "realtime") {
		return "", errors.New("profile does not support realtime")
	}
	if c.traits().deployment {
		return c.realtimeSocket(model, url.Values{"deployment": {c.Model(model)}})
	}
	return c.realtimeSocket(model, url.Values{"model": {c.Model(model)}})
}

var realtimeCallID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (c Config) webRTCRefusal() error {
	if c.ProfileID != "" && !c.Supports("realtime", "openai", "webrtc") {
		return errors.New("profile does not support webrtc")
	}
	return nil
}

func (c Config) RealtimeCallsURL(model string) (string, error) {
	if err := c.webRTCRefusal(); err != nil {
		return "", err
	}
	return c.ResourceURL(model, "realtime/calls", nil)
}

func (c Config) RealtimeControlURL(model, callID string) (string, error) {
	if !realtimeCallID.MatchString(callID) {
		return "", errors.New("invalid realtime call id")
	}
	if err := c.webRTCRefusal(); err != nil {
		return "", err
	}
	return c.realtimeSocket(model, url.Values{"call_id": {callID}})
}

func (c Config) RealtimeHangupURL(model, callID string) (string, error) {
	if !realtimeCallID.MatchString(callID) {
		return "", errors.New("invalid realtime call id")
	}
	if err := c.webRTCRefusal(); err != nil {
		return "", err
	}
	return c.ResourceURL(model, "realtime/calls/"+url.PathEscape(callID)+"/hangup", nil)
}
