// Package connectors owns provider addressing and the preparation of upstream
// requests: hosting, authentication and signing. It does not retry inference
// calls: the gateway's attempt executor is the only retry owner.
package connectors

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/vendors"
)

type Config struct {
	Network *egress.ConnectionOptions
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

// DefaultProfileEndpoint is the endpoint a provider of kind using profileID
// gets when it names none: its kind's, or the address the profile's hosting
// is served at.
func DefaultProfileEndpoint(kind, profileID, region, project string) string {
	endpoint := DefaultEndpoint(kind, region, project)
	if kind != "vertex_ai" || endpoint == "" {
		return endpoint
	}
	switch profileID {
	case "vertex-anthropic":
		return strings.TrimSuffix(endpoint, "/publishers/google") + "/publishers/anthropic"
	case "vertex-openai":
		return strings.TrimSuffix(endpoint, "/publishers/google") + "/endpoints/openapi"
	}
	return endpoint
}

func DefaultEndpoint(kind, region, project string) string {
	switch kind {
	case "openai":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "gemini":
		return "https://generativelanguage.googleapis.com/v1beta"
	case "vertex_ai":
		host := region + "-aiplatform.googleapis.com"
		if region == "global" {
			host = "aiplatform.googleapis.com"
		}
		if region == "us" || region == "eu" {
			host = "aiplatform." + region + ".rep.googleapis.com"
		}
		return "https://" + host + "/v1/projects/" + project + "/locations/" + region + "/publishers/google"
	case "bedrock":
		endpoint, _ := BedrockEndpoint(region, false)
		return endpoint
	case KindSageMaker:
		endpoint, _ := SageMakerEndpoint(region)
		return endpoint
	case KindWatsonx:
		if !cloudIdentifier.MatchString(region) {
			return ""
		}
		return "https://" + region + ".ml.cloud.ibm.com"
	}
	return ""
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
	if err := c.validateProfileEndpoint(u); err != nil {
		return err
	}
	if hosting := c.Hosting(); hosting == "azure-v1" || hosting == "azure-responses-legacy" {
		return nil
	}
	switch c.Kind {
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
	if c.Kind == "gemini" || c.Kind == "vertex_ai" || c.Kind == "azure_openai" {
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
	var metadata struct {
		Deployment string `json:"deployment"`
	}
	// A model with no metadata is not decoded: the error that would build is
	// discarded, for every attempt.
	if raw := c.Models[model]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &metadata)
	}
	if metadata.Deployment != "" {
		return metadata.Deployment
	}
	return model
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
func (c Config) URL(wire openai.Family, model string, stream bool) (string, error) {
	if c.ProfileID != "" {
		if err := c.ValidateProfile(); err != nil {
			return "", err
		}
		expected, err := c.TargetFamily(wire)
		batch := wire == openai.FamilyGeminiEmbeddingsBatch && expected == openai.FamilyGeminiEmbeddings
		if err != nil || wire != expected && !batch {
			return "", errors.New("wire dialect does not match the selected provider profile")
		}
	}
	model = c.Model(model)
	base := c.profileBase()
	if !c.ValidModel(model) {
		return "", errors.New("invalid upstream model identifier")
	}
	switch c.Kind {
	case KindSageMaker:
		return sagemakerURL(base, wire, model)
	case KindWatsonx:
		return c.watsonxURL(base, wire, stream)
	}
	path := "/chat/completions"
	switch wire {
	case openai.FamilyResponses:
		path = "/responses"
	case openai.FamilyInputTokens:
		path = "/responses/input_tokens"
	case openai.FamilyEmbeddings:
		path = "/embeddings"
	case openai.FamilyModeration:
		path = "/moderations"
	case openai.FamilyAnthropic:
		path = "/messages"
	case openai.FamilyAnthropicCount:
		path = "/messages/count_tokens"
	case openai.FamilyGemini, openai.FamilyGeminiCount:
		operation := "generateContent"
		if stream {
			operation = "streamGenerateContent"
		}
		if wire == openai.FamilyGeminiCount {
			operation = "countTokens"
		}
		path = "/models/" + url.PathEscape(model) + ":" + operation
		if stream {
			path += "?alt=sse"
		}
	case "bedrock":
		operation := "converse"
		if stream {
			operation = "converse-stream"
		}
		path = "/model/" + url.PathEscape(model) + "/" + operation
	case "bedrock_count":
		path = "/model/" + url.PathEscape(model) + "/count-tokens"
	case openai.FamilyGeminiEmbeddings:
		path = "/models/" + url.PathEscape(model) + ":embedContent"
	case openai.FamilyGeminiEmbeddingsBatch:
		path = "/models/" + url.PathEscape(model) + ":batchEmbedContents"
	case openai.FamilyVertexEmbeddings:
		path = "/models/" + url.PathEscape(model) + ":predict"
	case openai.FamilyBedrockInvoke:
		operation := "invoke"
		if stream {
			operation = "invoke-with-response-stream"
		}
		path = "/model/" + url.PathEscape(model) + "/" + operation
	case openai.FamilyBedrockEmbeddings:
		path = "/model/" + url.PathEscape(model) + "/invoke"
	case openai.FamilyBedrockRerank:
		return c.bedrockAgentRuntime(base)
	case openai.FamilyMistralFIM:
		path = "/fim/completions"
	case openai.FamilyCohereChat:
		path = "/chat"
	case openai.FamilyRerank:
		if endpoint, ok := c.operationEndpoint("rerank", base); ok {
			return endpoint, nil
		}
		path = "/rerank"
	}
	if c.Hosting() == "vertex-anthropic" && wire == openai.FamilyAnthropic {
		action := "rawPredict"
		if stream {
			action = "streamRawPredict"
		}
		return base + "/models/" + url.PathEscape(model) + ":" + action, nil
	}
	if c.Hosting() == "bedrock-anthropic-invoke" && wire == openai.FamilyAnthropic {
		action := "invoke"
		if stream {
			action = "invoke-with-response-stream"
		}
		return base + "/model/" + url.PathEscape(model) + "/" + action, nil
	}
	if c.Hosting() == "azure-responses-legacy" && (wire == openai.FamilyResponses || wire == openai.FamilyInputTokens) {
		return base + "/openai" + path + "?api-version=" + url.QueryEscape(c.APIVersion), nil
	}
	if c.traits().deployment {
		if model != c.Deployment && !c.hasDeployment(model) {
			var metadata struct {
				Deployment string `json:"deployment"`
			}
			for _, v := range c.Models {
				_ = json.Unmarshal(v, &metadata)
				if metadata.Deployment == model {
					break
				}
			}
			if metadata.Deployment != model {
				return "", errors.New("model has no configured Azure deployment")
			}
		}
		base += "/openai/deployments/" + url.PathEscape(model)
	}
	if c.traits().apiVersion {
		path += "?api-version=" + url.QueryEscape(c.APIVersion)
	}
	return base + path, nil
}

// operationEndpoint is the address the vendor's contract documents for an
// operation it serves outside its reviewed endpoint, while the provider still
// uses that endpoint.
func (c Config) operationEndpoint(operation, base string) (string, bool) {
	contract, ok := vendors.Lookup(c.VendorID)
	if !ok || base != strings.TrimRight(contract.Endpoint, "/") {
		return "", false
	}
	endpoint, ok := contract.OperationEndpoints[operation]
	return endpoint, ok
}

// MediaURL resolves an OpenAI-family media resource path — images, audio, and
// videos — against the connector endpoint. Azure deployments keep their
// address prefix and fixed api-version query exactly as generation URLs do.
// The path is a fixed relative resource from the media dispatch table; job
// identifiers are validated by the caller before reaching it.
func (c Config) MediaURL(path, model string, query url.Values) (string, error) {
	if strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.ContainsAny(path, "\\?#") {
		return "", errors.New("invalid upstream resource path")
	}
	base := c.profileBase()
	if c.traits().deployment {
		deployment := c.Model(model)
		if deployment != c.Deployment && !c.hasDeployment(deployment) {
			var metadata struct {
				Deployment string `json:"deployment"`
			}
			found := false
			for _, v := range c.Models {
				metadata.Deployment = ""
				_ = json.Unmarshal(v, &metadata)
				if metadata.Deployment == deployment {
					found = true
					break
				}
			}
			if !found {
				return "", errors.New("model has no configured Azure deployment")
			}
		}
		base += "/openai/deployments/" + url.PathEscape(deployment)
	}
	u := base + "/" + path
	merged := url.Values{}
	if c.traits().apiVersion {
		merged.Set("api-version", c.APIVersion)
	}
	for name, values := range query {
		merged[name] = append([]string{}, values...)
	}
	if len(merged) > 0 {
		u += "?" + merged.Encode()
	}
	return u, nil
}

func (c Config) hasDeployment(model string) bool {
	for _, binding := range c.Bindings {
		if binding.Deployment == model {
			return true
		}
	}
	return false
}

// ResourceURL resolves resources independently from generation endpoint choice.
func (c Config) ResourceURL(model, path string, query url.Values) (string, error) {
	if strings.Contains(path, "..") || strings.ContainsAny(path, "\\?#") {
		return "", errors.New("invalid upstream resource path")
	}
	base := c.profileBase()
	if c.traits().deployment {
		if strings.HasPrefix(path, "deployments/") {
			deployment := c.Model(model)
			if deployment == "" {
				return "", errors.New("model has no configured Azure deployment")
			}
			base += "/openai/deployments/" + url.PathEscape(deployment)
			path = strings.TrimPrefix(path, "deployments/")
		} else {
			base += "/openai"
		}
	}
	merged := url.Values{}
	if c.traits().apiVersion {
		merged.Set("api-version", c.APIVersion)
	}
	for name, values := range query {
		if existing, ok := merged[name]; ok && !slices.Equal(existing, values) {
			return "", errors.New("query collides with hosting API revision")
		}
		merged[name] = append([]string(nil), values...)
	}
	endpoint := base + "/" + strings.TrimPrefix(path, "/")
	if len(merged) > 0 {
		endpoint += "?" + merged.Encode()
	}
	return endpoint, nil
}

func (c Config) RealtimeURL(model string) (string, error) {
	if c.ProfileID != "" && !c.Supports("realtime", "openai", "realtime") {
		return "", errors.New("profile does not support realtime")
	}
	query := url.Values{}
	if c.traits().deployment {
		query.Set("deployment", c.Model(model))
	} else {
		query.Set("model", c.Model(model))
	}
	endpoint, err := c.ResourceURL(model, "realtime", query)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(endpoint, "https://") {
		return "wss://" + strings.TrimPrefix(endpoint, "https://"), nil
	}
	if strings.HasPrefix(endpoint, "http://") {
		return "ws://" + strings.TrimPrefix(endpoint, "http://"), nil
	}
	return "", errors.New("realtime endpoint requires HTTP(S) hosting")
}
