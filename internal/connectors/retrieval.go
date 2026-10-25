package connectors

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const KindAzureAISearch = "azure_ai_search"

func (c Config) RetrievalBackend() string {
	if c.ProfileRevision != ProfileRevision {
		return ""
	}
	switch c.ProfileID {
	case "bedrock-knowledge-bases":
		if c.Kind == "bedrock" {
			return "bedrock_knowledge_base"
		}
	case "vertex-rag-engine":
		if c.Kind == "vertex_ai" {
			return "vertex_rag"
		}
	case "azure-ai-search":
		if c.Kind == KindAzureAISearch {
			return KindAzureAISearch
		}
	}
	return ""
}
func (c Config) retrievalModelValid(model string) bool {
	switch c.RetrievalBackend() {
	case "bedrock_knowledge_base":
		return regexp.MustCompile(`^[A-Za-z0-9]{10}$`).MatchString(model)
	case "vertex_rag":
		return strings.HasPrefix(model, "projects/"+c.CloudProject+"/locations/"+c.CloudRegion+"/ragCorpora/") && regexp.MustCompile(`^projects/[A-Za-z0-9_-]+/locations/[a-z0-9-]+/ragCorpora/[A-Za-z0-9_-]+$`).MatchString(model)
	case KindAzureAISearch:
		return regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`).MatchString(model)
	}
	return false
}

func (c Config) RetrievalURL(resource string) (string, error) {
	if !c.retrievalModelValid(resource) {
		return "", errors.New("invalid bound retrieval resource")
	}
	base := strings.TrimRight(c.Endpoint, "/")
	switch c.RetrievalBackend() {
	case "bedrock_knowledge_base":
		return base + "/knowledgebases/" + url.PathEscape(resource) + "/retrieve", nil
	case "vertex_rag":
		return base + ":retrieveContexts", nil
	case KindAzureAISearch:
		return base + "/indexes/" + url.PathEscape(resource) + "/docs/search?api-version=" + url.QueryEscape(c.APIVersion), nil
	}
	return "", errors.New("no retrieval profile")
}

// RetrievalProbeURL names a cost-free metadata read for the resource used as
// this target's model. Search permissions still require live qualification.
func (c Config) RetrievalProbeURL(resource string) (string, error) {
	if !c.retrievalModelValid(resource) {
		return "", errors.New("invalid retrieval resource")
	}
	base := strings.TrimRight(c.Endpoint, "/")
	switch c.RetrievalBackend() {
	case "bedrock_knowledge_base":
		want, _ := BedrockEndpoint(c.CloudRegion, false)
		want = strings.Replace(want, "://bedrock-runtime.", "://bedrock-agent-runtime.", 1)
		if base != want {
			return "", errors.New("metadata certification requires the default regional Knowledge Bases endpoint")
		}
		return strings.Replace(base, "://bedrock-agent-runtime.", "://bedrock-agent.", 1) + "/knowledgebases/" + url.PathEscape(resource), nil
	case "vertex_rag":
		return base + "/ragCorpora/" + url.PathEscape(resource[strings.LastIndex(resource, "/")+1:]), nil
	case KindAzureAISearch:
		return base + "/indexes/" + url.PathEscape(resource) + "?api-version=" + url.QueryEscape(c.APIVersion), nil
	}
	return "", errors.New("no retrieval profile")
}
