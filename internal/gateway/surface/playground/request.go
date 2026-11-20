package playground

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/publishedprompt"
	"github.com/tyk-swe/olp/internal/routing"
)

type tool struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"input_schema"`
	Description *string         `json:"description"`
}

type format struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Schema      json.RawMessage `json:"schema"`
	Strict      *bool           `json:"strict"`
	Description *string         `json:"description"`
}

type request struct {
	Model           string               `json:"model"`
	Operation       string               `json:"operation"`
	Input           string               `json:"input"`
	Request         json.RawMessage      `json:"request"`
	Stream          *bool                `json:"stream"`
	Surface         *string              `json:"surface"`
	Temperature     *float64             `json:"temperature"`
	MaxOutputTokens *int64               `json:"max_output_tokens"`
	Tools           []tool               `json:"tools"`
	ResponseFormat  *format              `json:"response_format"`
	Routing         *routing.Preferences `json:"routing"`
}

// chatBody translates the playground request into a native chat request.
func (in *request) chatBody() ([]byte, error) {
	body := map[string]any{
		"model":    in.Model,
		"messages": []map[string]any{{"role": "user", "content": in.Input}},
	}
	if in.Stream != nil && *in.Stream {
		body["stream"] = true
	}
	if in.Temperature != nil {
		body["temperature"] = *in.Temperature
	}
	if in.MaxOutputTokens != nil {
		body["max_completion_tokens"] = *in.MaxOutputTokens
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, tool := range in.Tools {
			fn := map[string]any{"name": tool.Name, "parameters": tool.InputSchema}
			if tool.Description != nil {
				fn["description"] = *tool.Description
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
	}
	if f := in.ResponseFormat; f != nil {
		switch f.Type {
		case "text", "json_object":
			body["response_format"] = map[string]any{"type": f.Type}
		case "json_schema":
			schema := map[string]any{"name": f.Name, "schema": f.Schema}
			if f.Strict != nil {
				schema["strict"] = *f.Strict
			}
			if f.Description != nil {
				schema["description"] = *f.Description
			}
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
		}
	}
	return json.Marshal(body)
}

func (in *request) operation() string {
	if in.Operation == "" {
		return "generation"
	}
	return in.Operation
}

func (in *request) surface() string {
	if in.Surface == nil {
		return "openai"
	}
	return *in.Surface
}

func (in *request) validate(streaming bool) error {
	switch in.operation() {
	case "generation", "token_count", "embeddings", "moderation", "rerank":
	default:
		return httpkit.Invalid("operation", "operation must be generation, token_count, embeddings, moderation, or rerank")
	}
	switch {
	case strings.TrimSpace(in.Model) == "":
		return httpkit.Invalid("model", "model is required")
	case in.Surface != nil && !slices.Contains([]string{"openai", "anthropic", "gemini"}, *in.Surface):
		return httpkit.Invalid("surface", "surface must be openai, anthropic, or gemini")
	case in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2):
		return httpkit.Invalid("temperature", "temperature must be between 0 and 2")
	case in.MaxOutputTokens != nil && (*in.MaxOutputTokens < 1 || *in.MaxOutputTokens > 1<<20):
		return httpkit.Invalid("max_output_tokens", "max_output_tokens must be between 1 and 1048576")
	case len(in.Tools) > 128:
		return httpkit.Invalid("tools", "at most 128 tools are allowed")
	}
	if in.Request != nil {
		if in.Input != "" || len(in.Tools) > 0 || in.ResponseFormat != nil || in.Temperature != nil || in.MaxOutputTokens != nil {
			return httpkit.Invalid("request", "request cannot be combined with input, tools, response_format, temperature, or max_output_tokens")
		}
	} else {
		if in.Input == "" {
			return httpkit.Invalid("input", "input is required")
		}
		if in.operation() != "generation" {
			return httpkit.Invalid("operation", "non-generation operations require a raw request document")
		}
	}
	if streaming {
		if in.Stream == nil || !*in.Stream {
			return httpkit.Invalid("stream", "stream must be true for the streaming playground")
		}
		if in.operation() != "generation" {
			return httpkit.Invalid("operation", "streaming supports generation only")
		}
	} else if in.Stream != nil && *in.Stream {
		return httpkit.Invalid("stream", "streaming requests use the streaming playground endpoint")
	}
	if in.operation() != "generation" && in.operation() != "token_count" && in.surface() != "openai" {
		return httpkit.Invalid("surface", "this operation is available on the openai surface only")
	}
	if err := in.Routing.Validate("routing"); err != nil {
		return err
	}
	for i, tool := range in.Tools {
		if strings.TrimSpace(tool.Name) == "" || len(tool.InputSchema) == 0 {
			return httpkit.Invalid("tools", "tool "+string(rune('0'+min(i, 9)))+" needs a name and an input_schema")
		}
	}
	if f := in.ResponseFormat; f != nil {
		switch f.Type {
		case "text", "json_object":
		case "json_schema":
			if strings.TrimSpace(f.Name) == "" || len(f.Schema) == 0 {
				return httpkit.Invalid("response_format", "json_schema formats need a name and a schema")
			}
		default:
			return httpkit.Invalid("response_format", "unsupported response_format type")
		}
	}
	return nil
}

func (in *request) parse(operation, mode string, run *kernel.MemberGeneration) (kernel.ParsedRequest, error) {
	if in.Request != nil {
		family := protocols.WireFamily(in.surface(), "", wire.Chat)
		if family == wire.Gemini && mode == "streaming" {
			family = wire.GeminiStream
		}
		doc, err := protocols.ParseSource(family, in.Request)
		if err != nil {
			e := kernel.RequestError(err)
			return kernel.ParsedRequest{}, httpkit.Fail(e.Status, e.Code, e.Message)
		}
		if operation == "generation" {
			if family == wire.Chat {
				if !publishedprompt.Referenced(doc, wire.Chat) {
					if publishedprompt.Referenced(doc, wire.Responses) {
						family = wire.Responses
					}
				}
			}
			if publishedprompt.Referenced(doc, family) {
				bindings := []oif.Change{}
				if family == wire.Gemini || family == wire.GeminiStream {
					for _, name := range []string{"model", "stream"} {
						if _, ok := doc.Root().Lookup(name); ok {
							bindings = append(bindings, oif.Change{Pointer: "/" + name, Remove: true, Origin: oif.IdentityBinding, Reason: "playground route and mode binding"})
						}
					}
				} else {
					model, _ := json.Marshal(in.Model)
					stream, _ := json.Marshal(mode == "streaming")
					bindings = []oif.Change{
						{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "playground route binding"},
						{Pointer: "/stream", Value: string(stream), Origin: oif.TransportOption, Reason: "playground mode binding"},
					}
				}
				parsed, e := run.ExpandPrompt(family, doc, in.Model, bindings)
				if e != nil {
					return kernel.ParsedRequest{}, httpkit.Fail(e.Status, e.Code, e.Message)
				}
				return parsed, nil
			}
		}
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(in.Request, &fields); err != nil || fields == nil {
			return kernel.ParsedRequest{}, httpkit.Invalid("request", "request must be a JSON object")
		}
		if in.surface() == "gemini" {
			delete(fields, "model")
			delete(fields, "stream")
		} else {
			fields["model"], _ = json.Marshal(in.Model)
			fields["stream"], _ = json.Marshal(mode == "streaming")
		}
		data, _ := json.Marshal(fields)
		parsed, err := protocols.SimulationRequest(data, operation, in.surface(), mode, in.Model)
		if err != nil {
			return kernel.ParsedRequest{}, httpkit.Fail(http.StatusUnprocessableEntity, "validation_failed", err.Error())
		}
		return kernel.ParsedRequest{Request: parsed, Decoded: int64(len(data))}, nil
	}
	body, err := in.chatBody()
	if err != nil {
		return kernel.ParsedRequest{}, httpkit.Invalid("input", "the request could not be encoded")
	}
	parsed, err := openai.Parse(wire.Chat, body)
	if err != nil {
		return kernel.ParsedRequest{}, httpkit.Fail(http.StatusUnprocessableEntity, "validation_failed", err.Error())
	}
	if in.Surface != nil && (*in.Surface == "anthropic" || *in.Surface == "gemini") {
		kind := *in.Surface
		if in.MaxOutputTokens == nil {
			fields := parsed.Document()
			fields["max_completion_tokens"] = json.RawMessage("4096")
			body, _ = json.Marshal(fields)
			parsed, _ = openai.Parse(wire.Chat, body)
		}
		encoded, encodedWire, e := protocols.Encode(parsed, kind, kind, in.Model, nil)
		if e != nil {
			return kernel.ParsedRequest{}, httpkit.Invalid("input", e.Error())
		}
		parsed, e = protocols.Parse(encodedWire, encoded, in.Model)
		if e != nil {
			return kernel.ParsedRequest{}, httpkit.Invalid("input", e.Error())
		}
	}
	return kernel.ParsedRequest{Request: parsed}, nil
}
