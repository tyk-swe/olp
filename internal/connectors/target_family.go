package connectors

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func (c Config) TargetFamily(source wire.Format) (wire.Format, error) {
	p, err := c.profile()
	if err != nil {
		return "", err
	}
	operation := source.Operation()
	if source == wire.Format("bedrock_count") {
		operation = "token_count"
	}
	if !slices.Contains(p.Operations, operation) {
		return "", errors.New("operation is outside the selected provider profile")
	}
	switch operation {
	case "decision":
		if p.OperationDialect(operation) == "systemone" {
			return wire.SystemOne, nil
		}
		return wire.Decisions, nil
	case "generation":
		if family, ok := generationFamily(p.OperationDialect("generation")); ok {
			return family, nil
		}
	case "token_count":
		switch p.Dialect {
		case "anthropic-messages":
			return wire.AnthropicCount, nil
		case "gemini-generate-content":
			return wire.GeminiCount, nil
		case "bedrock-converse":
			return wire.Format("bedrock_count"), nil
		default:
			return wire.InputTokens, nil
		}
	case "embeddings":
		switch p.Hosting {
		case "direct-gemini":
			return wire.GeminiEmbeddings, nil
		case "vertex-google":
			return wire.VertexEmbeddings, nil
		case "bedrock-converse":
			return wire.BedrockEmbeddings, nil
		default:
			return wire.Embeddings, nil
		}
	case "moderation":
		return wire.Moderation, nil
	case "ocr":
		if p.Hosting == "google-document-ai" {
			return wire.DocumentAIProcess, nil
		}
		return wire.OCR, nil
	case "search":
		return wire.Search, nil
	case "rerank":
		return wire.Rerank, nil
	case "bedrock_invoke":
		return wire.BedrockInvoke, nil
	}
	return "", errors.New("operation has no codec in the selected profile")
}

func generationFamily(dialect string) (wire.Format, bool) {
	switch dialect {
	case "openai-completions":
		return wire.Completions, true
	case "openai-chat":
		return wire.Chat, true
	case "openai-responses":
		return wire.Responses, true
	case "anthropic-messages":
		return wire.Anthropic, true
	case "gemini-generate-content":
		return wire.Gemini, true
	case "bedrock-converse":
		return wire.Bedrock, true
	case "mistral-fim":
		return wire.MistralFIM, true
	case "cohere-chat-v2":
		return wire.CohereChat, true
	}
	return "", false
}

func (c Config) WrapBody(body []byte, encodedWire wire.Format) ([]byte, error) {
	hosting := c.Hosting()
	if hosting != "vertex-anthropic" && hosting != "bedrock-anthropic-invoke" {
		return body, nil
	}
	if encodedWire != wire.Anthropic {
		return nil, errors.New("Anthropic cloud profile requires the Messages dialect")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("cloud request must be a JSON object")
	}
	p, _ := c.profile()
	version, _ := json.Marshal(p.DialectRevision)
	if prior, found := fields["anthropic_version"]; found && string(prior) != string(version) {
		return nil, errors.New("native cloud version collides with the configured profile")
	}
	fields["anthropic_version"] = version
	delete(fields, "model")
	if hosting == "bedrock-anthropic-invoke" {
		delete(fields, "stream")
	}
	return json.Marshal(fields)
}
