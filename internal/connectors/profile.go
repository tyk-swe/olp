package connectors

import (
	"encoding/json"
	"errors"
)

type Profile struct {
	OperationDialects map[string]string          `json:"operation_dialects"`
	DefaultSchemas    map[string]json.RawMessage `json:"default_schemas"`
	ID                string                     `json:"id"`
	Revision          string                     `json:"revision"`
	Label             string                     `json:"label"`
	Kind              Kind                       `json:"kind"`
	Dialect           string                     `json:"dialect"`
	DialectRevision   string                     `json:"dialect_revision"`
	Hosting           string                     `json:"hosting"`
	Authentication    []AuthMode                 `json:"authentication"`
	Transport         string                     `json:"transport"`
	Operations        []string                   `json:"operations"`
	SemanticHeaders   []string                   `json:"semantic_headers"`
	QuerySettings     []string                   `json:"query_settings"`
	Documentation     string                     `json:"documentation"`
	Plugin            *Plugin                    `json:"plugin,omitempty"`
	ModelDiscovery    bool                       `json:"model_discovery,omitempty"`
	OptionsSchema     json.RawMessage            `json:"options_schema,omitempty"`
	Strict            bool                       `json:"strict"`
	vendor            reviewedVendor
}

func (c Config) profile() (Profile, error) {
	if c.Plugin == nil {
		return builtInProfile(c.ProfileID, c.ProfileRevision)
	}
	if p := c.Plugin.profile; p.ID == c.ProfileID && p.Revision == c.ProfileRevision {
		return p, nil
	}
	return Profile{}, errors.New("the plugin profile is not the configured profile revision")
}

func (c Config) Profile() (Profile, error) {
	p, err := c.profile()
	if err != nil {
		return Profile{}, err
	}
	return cloneProfile(p), nil
}

func (c Config) Hosting() string {
	if p, err := c.profile(); err == nil {
		return p.Hosting
	}
	return ""
}

func (p Profile) Vendor() string {
	required, _ := p.reviewedVendor()
	return required.id
}

func (p Profile) OperationDialect(operation string) string {
	if dialect := p.OperationDialects[operation]; dialect != "" {
		return dialect
	}
	if operation == "generation" {
		return p.Dialect
	}
	switch operation {
	case "embeddings":
		switch p.Hosting {
		case "direct-gemini":
			return "gemini-embeddings"
		case "vertex-google":
			return "vertex-embeddings"
		case "bedrock-converse":
			return "bedrock-embeddings"
		}
		return "openai-embeddings"
	case "token_count":
		switch p.Dialect {
		case "anthropic-messages":
			return "anthropic-count-tokens"
		case "gemini-generate-content":
			return "gemini-count-tokens"
		case "bedrock-converse":
			return "bedrock-count-tokens"
		}
		return "openai-input-tokens"
	case "rerank":
		return "rerank"
	case "bedrock_invoke":
		return "bedrock-invoke"
	case "realtime":
		if (p.Kind == KindOpenAI && p.Hosting == "direct-openai") || (p.Kind == KindAzureOpenAI && p.Hosting == "azure-v1") {
			return "openai-realtime"
		}
		switch p.Dialect {
		case "xai-realtime", "vertex-realtime", "bedrock-realtime":
			return p.Dialect
		}
	}
	return p.Hosting + "/" + operation
}
