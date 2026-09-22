package connectors

import (
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func registerUnaryProfiles() {
	for _, d := range operationregistry.Default.Dialects() {
		// These are hosting compositions, not operation-kernel provider cases.
		kind, hosting, auth := "openai_compatible", "direct-compatible", []string{"api_key", "headers", "none"}
		switch d.Identity.ID {
		case "openai-embeddings", "openai-moderation", "openai-input-tokens":
			kind, hosting = "openai", "direct-openai"
		case "anthropic-count-tokens", "gemini-count-tokens", "bedrock-count-tokens", "gemini-embeddings", "vertex-embeddings", "bedrock-embeddings":
			continue // established cloud/native compositions
		case "gemini-batch-embeddings":
			kind, hosting = "gemini", "direct-gemini"
		}
		profile := Profile{ID: d.Identity.ID, Revision: ProfileRevision, Label: d.Label, Kind: kind, Dialect: d.Identity.ID, DialectRevision: d.Identity.Revision, Hosting: hosting, Authentication: auth, Transport: "http", Operations: []string{d.Operation.ID}, OperationDialects: map[string]string{d.Operation.ID: d.Identity.ID}, SemanticHeaders: []string{}, QuerySettings: []string{}, Documentation: d.Documentation}
		completeProfileMetadata(&profile)
		profileRegistry = append(profileRegistry, profile)
	}
}

// RegisterOperationProfile composes a trusted registered unary codec with an
// existing hosting/authentication contract. No generation operation is implied.
func RegisterOperationProfile(p Profile) error {
	profileMu.Lock()
	defer profileMu.Unlock()
	if p.ID == "" || len(p.ID) > 128 || p.Revision == "" || p.Label == "" || p.Transport != "http" || len(p.Operations) == 0 || len(profileRegistry) >= 4096 {
		return errors.New("operation profile requires bounded identity and HTTP contracts")
	}
	var host *Profile
	for i := range profileRegistry {
		prior := &profileRegistry[i]
		if prior.ID == p.ID && prior.Revision == p.Revision {
			return errors.New("profile revision already registered")
		}
		if prior.Kind == p.Kind && prior.Hosting == p.Hosting {
			host = prior
		}
	}
	if host == nil || len(p.Authentication) == 0 {
		return errors.New("hosting component is not registered")
	}
	for _, a := range p.Authentication {
		if !slices.Contains(host.Authentication, a) {
			return errors.New("authentication component is incompatible")
		}
	}
	for _, op := range p.Operations {
		d, ok := operationregistry.Lookup(p.OperationDialects[op])
		if !ok || d.Operation.ID != op {
			return errors.New("operation codec is not registered")
		}
		if d.Address.RelativePath != "" && p.Hosting != "direct-compatible" {
			return errors.New("relative operation addressing requires compatible direct hosting")
		}
	}
	for _, h := range p.SemanticHeaders {
		if !slices.Contains(host.SemanticHeaders, h) {
			return errors.New("semantic header is outside hosting contract")
		}
	}
	for _, q := range p.QuerySettings {
		if !slices.Contains(host.QuerySettings, q) {
			return errors.New("semantic query is outside hosting contract")
		}
	}
	completeProfileMetadata(&p)
	profileRegistry = append(profileRegistry, cloneProfile(p))
	return nil
}

// OperationURL uses only a registered native addressing contract. Its legacy
// adapter is bounded to explicit paths; unknown operations never choose Chat.
func (c Config) OperationURL(d operations.Dialect, model string) (string, error) {
	p, err := c.Profile()
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
		base, err := url.Parse(c.profileBase())
		if err != nil {
			return "", err
		}
		base.Path = strings.TrimRight(base.Path, "/") + "/" + path
		return base.String(), nil
	}
	paths := map[string]openai.Family{"embeddings": openai.FamilyEmbeddings, "moderation": openai.FamilyModeration, "input_tokens": openai.FamilyInputTokens, "anthropic_count": openai.FamilyAnthropicCount, "gemini_count": openai.FamilyGeminiCount, "bedrock_count": openai.Family("bedrock_count"), "gemini_embeddings": openai.FamilyGeminiEmbeddings, "gemini_embeddings_batch": openai.FamilyGeminiEmbeddingsBatch, "vertex_embeddings": openai.FamilyVertexEmbeddings, "bedrock_embeddings": openai.FamilyBedrockEmbeddings, "rerank": openai.FamilyRerank}
	wire, ok := paths[d.Address.LegacyPath]
	if !ok {
		return "", errors.New("operation path adapter is not registered")
	}
	return c.URL(wire, model, false)
}
