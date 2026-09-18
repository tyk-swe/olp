// Package connectors owns provider addressing and authentication. It does not
// retry inference calls: the gateway's attempt executor is the only retry owner.
package connectors

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type Config struct {
	Kind, AuthMode, Endpoint, CloudRegion, CloudProject, Deployment, APIVersion, VendorID string
	CredentialHeaders                                                                     []string
	Models                                                                                map[string]json.RawMessage
}

func SecretRequired(mode string) bool {
	return mode != "none" && mode != "adc" && mode != "default_chain"
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
	}
	return ""
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var cloudIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)
var bedrockModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func ModelValid(kind, model string) bool {
	if kind == "bedrock" {
		return len(model) <= 2048 && bedrockModel.MatchString(model) && !strings.Contains(model, "..")
	}
	if kind == "azure_openai" || kind == "gemini" || kind == "vertex_ai" {
		return identifier.MatchString(strings.TrimPrefix(model, "models/")) && !strings.HasSuffix(model, ".")
	}
	return model != "" && !strings.ContainsAny(model, "\x00\r\n")
}
func (c Config) Validate(policy *egress.Policy) error {
	u, e := policy.ValidateEndpoint(c.Endpoint)
	if e != nil {
		return e
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
func (c Config) Model(model string) string {
	var metadata struct {
		Deployment string `json:"deployment"`
	}
	_ = json.Unmarshal(c.Models[model], &metadata)
	if metadata.Deployment != "" {
		return metadata.Deployment
	}
	return strings.TrimPrefix(model, "models/")
}
func (c Config) URL(wire openai.Family, model string, stream bool) (string, error) {
	model = c.Model(model)
	base := strings.TrimRight(c.Endpoint, "/")
	if !ModelValid(c.Kind, model) {
		return "", errors.New("invalid upstream model identifier")
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
	}
	if c.Kind == "azure_openai" {
		if model != c.Deployment {
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
		path += "?api-version=" + url.QueryEscape(c.APIVersion)
	}
	return base + path, nil
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
	base := strings.TrimRight(c.Endpoint, "/")
	if c.Kind == "azure_openai" {
		deployment := c.Model(model)
		if deployment != c.Deployment {
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
	if c.Kind == "azure_openai" {
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
