package connectors

import (
	"errors"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
)

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
