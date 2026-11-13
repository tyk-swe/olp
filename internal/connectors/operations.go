package connectors

import (
	"errors"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
)

func registerUnaryProfiles() {
	for _, d := range operationregistry.Catalog().Dialects() {
		// These are hosting compositions, not operation-kernel provider cases.
		kind, hosting, auth := "openai_compatible", "direct-compatible", []string{"api_key", "headers", "none"}
		switch d.Identity.ID {
		case "openai-embeddings", "openai-moderation", "openai-input-tokens", "openai-decisions":
			kind, hosting = "openai", "direct-openai"
		case "cohere-embed-v2", "cohere-rerank-v2", "mistral-ocr", "azure-document-intelligence-ocr", "perplexity-search", "tavily-search", "exa-search", "parallel-search", "brave-search":
			auth = []string{"api_key"}
		case "anthropic-count-tokens", "gemini-count-tokens", "bedrock-count-tokens", "gemini-embeddings", "vertex-embeddings", "bedrock-embeddings", "vertex-document-processing-ocr":
			continue // established cloud/native compositions
		case "gemini-batch-embeddings":
			kind, hosting = "gemini", "direct-gemini"
		}
		profile := Profile{ID: d.Identity.ID, Revision: ProfileRevision, Label: d.Label, Kind: kind, Dialect: d.Identity.ID, DialectRevision: d.Identity.Revision, Hosting: hosting, Authentication: auth, Transport: "http", Operations: []string{d.Operation.ID}, OperationDialects: map[string]string{d.Operation.ID: d.Identity.ID}, SemanticHeaders: []string{}, QuerySettings: []string{}, Documentation: d.Documentation, Strict: true}
		if d.Identity.ID == "cohere-embed-v2" || d.Identity.ID == "cohere-rerank-v2" {
			profile.DialectRevision = "v2"
		}
		completeProfileMetadata(&profile)
		profileRegistry = append(profileRegistry, profile)
	}
	// Cohere's native v2 API serves chat, embed and rerank on one hosting, so
	// one profile carries the three dialects and one provider serves them.
	cohere := Profile{ID: "cohere-v2", Revision: ProfileRevision, Label: "Cohere native v2", Kind: "openai_compatible", Dialect: "cohere-embed-v2", DialectRevision: "v2", Hosting: "direct-compatible",
		Authentication: []string{"api_key"}, Transport: "http", Operations: []string{"embeddings", "rerank", "generation"}, OperationDialects: map[string]string{"embeddings": "cohere-embed-v2", "rerank": "cohere-rerank-v2", "generation": "cohere-chat-v2"},
		SemanticHeaders: []string{}, QuerySettings: []string{}, Documentation: "https://docs.cohere.com/reference/embed", Strict: true}
	completeProfileMetadata(&cohere)
	profileRegistry = append(profileRegistry, cohere)
}

// OperationURL uses only a registered native addressing contract. Its family
// endpoint adapter is bounded to explicit paths; unknown operations never choose Chat.
func (c Config) OperationURL(d operations.Dialect, model string) (string, error) {
	p, err := c.profile()
	if err != nil {
		return "", err
	}
	target, ok := operationregistry.Lookup(p.OperationDialect(d.Operation.ID))
	if !ok || target.Identity != d.Identity {
		return "", errors.New("operation dialect differs from provider profile")
	}
	if err := c.ValidateProfile(); err != nil {
		return "", err
	}
	if d.Address.RelativePath != "" {
		path := d.Address.RelativePath
		if c.Hosting() != "direct-compatible" || strings.ContainsAny(path, "?#\\") || strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			return "", errors.New("invalid registered operation path")
		}
		if strings.Contains(path, "{model}") {
			model = c.Model(model)
			if !validPathModel(model) || strings.Count(path, "{model}") != 1 {
				return "", errors.New("invalid registered operation path")
			}
			path = strings.Replace(path, "{model}", url.PathEscape(model), 1)
		} else if strings.ContainsAny(path, "{}") {
			return "", errors.New("invalid registered operation path")
		}
		base, err := url.Parse(c.profileBase())
		if err != nil {
			return "", err
		}
		base.Path = strings.TrimRight(base.Path, "/") + "/" + path
		return base.String(), nil
	}
	paths := map[string]wire.Format{"decisions": wire.Decisions, "systemone": wire.SystemOne, "embeddings": wire.Embeddings, "moderation": wire.Moderation, "input_tokens": wire.InputTokens, "anthropic_count": wire.AnthropicCount, "gemini_count": wire.GeminiCount, "bedrock_count": wire.Format("bedrock_count"), "gemini_embeddings": wire.GeminiEmbeddings, "gemini_embeddings_batch": wire.GeminiEmbeddingsBatch, "vertex_embeddings": wire.VertexEmbeddings, "bedrock_embeddings": wire.BedrockEmbeddings, "rerank": wire.Rerank, "documentai_process": wire.DocumentAIProcess}
	encodedWire, ok := paths[d.Address.FamilyPath]
	if !ok {
		return "", errors.New("operation path adapter is not registered")
	}
	return c.URL(encodedWire, model, false)
}

func validPathModel(model string) bool {
	if model == "" || len(model) > 128 || model == "." || model == ".." {
		return false
	}
	for _, r := range model {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '~' || r == '-') {
			return false
		}
	}
	return true
}
