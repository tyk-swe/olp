package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/routes"
)

// playgroundTimeout bounds one console playground call end to end.
const playgroundTimeout = 2 * time.Minute

// Playground serves the console's unary test surface through the same
// executor as API traffic, authenticated by console session.
type Playground struct {
	Access  *access.Server
	Gateway *Server
}

func (p *Playground) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v3/playground", p.Access.HandleTimeout(1<<20, playgroundTimeout, p.handle))
}

type playgroundTool struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"input_schema"`
	Description *string         `json:"description"`
}

type playgroundFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Schema      json.RawMessage `json:"schema"`
	Strict      *bool           `json:"strict"`
	Description *string         `json:"description"`
}

type playgroundRequest struct {
	Model           string              `json:"model"`
	Input           string              `json:"input"`
	Surface         *string             `json:"surface"`
	Temperature     *float64            `json:"temperature"`
	MaxOutputTokens *int64              `json:"max_output_tokens"`
	Tools           []playgroundTool    `json:"tools"`
	ResponseFormat  *playgroundFormat   `json:"response_format"`
	Routing         *routes.Preferences `json:"routing"`
}

// chatBody translates the playground request into a native chat request.
func (in *playgroundRequest) chatBody() ([]byte, error) {
	body := map[string]any{
		"model":    in.Model,
		"messages": []map[string]any{{"role": "user", "content": in.Input}},
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

func (in *playgroundRequest) validate() error {
	switch {
	case strings.TrimSpace(in.Model) == "":
		return access.Invalid("model", "model is required")
	case in.Input == "":
		return access.Invalid("input", "input is required")
	case in.Surface != nil && *in.Surface != "openai":
		return access.Invalid("surface", "only the openai surface is available")
	case in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2):
		return access.Invalid("temperature", "temperature must be between 0 and 2")
	case in.MaxOutputTokens != nil && (*in.MaxOutputTokens < 1 || *in.MaxOutputTokens > 1<<20):
		return access.Invalid("max_output_tokens", "max_output_tokens must be between 1 and 1048576")
	case len(in.Tools) > 128:
		return access.Invalid("tools", "at most 128 tools are allowed")
	}
	if err := in.Routing.Validate("routing"); err != nil {
		return err
	}
	for i, tool := range in.Tools {
		if strings.TrimSpace(tool.Name) == "" || len(tool.InputSchema) == 0 {
			return access.Invalid("tools", "tool "+string(rune('0'+min(i, 9)))+" needs a name and an input_schema")
		}
	}
	if f := in.ResponseFormat; f != nil {
		switch f.Type {
		case "text", "json_object":
		case "json_schema":
			if strings.TrimSpace(f.Name) == "" || len(f.Schema) == 0 {
				return access.Invalid("response_format", "json_schema formats need a name and a schema")
			}
		default:
			return access.Invalid("response_format", "unsupported response_format type")
		}
	}
	return nil
}

func (p *Playground) handle(r *http.Request) (access.Reply, error) {
	principal, err := p.Access.Principal(r, p.Access.Pool, "playground")
	if err != nil {
		return access.Reply{}, err
	}
	var in playgroundRequest
	if err := access.Decode(r, &in); err != nil {
		return access.Reply{}, err
	}
	if err := in.validate(); err != nil {
		return access.Reply{}, err
	}
	body, err := in.chatBody()
	if err != nil {
		return access.Reply{}, access.Invalid("input", "the request could not be encoded")
	}
	parsed, err := openai.Parse(openai.FamilyChat, body)
	if err != nil {
		return access.Reply{}, access.Fail(http.StatusUnprocessableEntity, "validation_failed", err.Error())
	}
	s := p.Gateway
	x := &execution{
		request:  request{id: uuidString(), minted: true, clientIP: ClientIP(r, s.cfg.TrustedProxies), startedAt: s.now(), release: s.Runtime.Release()},
		family:   openai.FamilyChat,
		parsed:   parsed,
		actor:    "playground",
		userID:   principal.ID,
		affinity: []byte(principal.ID),
		// The playground carries no API key budget, but the provider quotas it
		// consumes are the same ones inference traffic reserves.
		estimate: estimateTokens(parsed),
	}
	if e := s.prepare(x, func(string) bool { return true }); e != nil {
		x.failure = e
		s.finish(x, nil, e.Status)
		return access.Reply{}, access.Fail(e.Status, e.Code, e.Message)
	}
	x.budget = in.Routing.Budget(x.budget)
	if !s.admit() {
		x.failure = overloaded
		s.finish(x, nil, overloaded.Status)
		return access.Reply{}, access.Fail(overloaded.Status, overloaded.Code, overloaded.Message)
	}
	defer s.release()
	out := s.execute(r.Context(), x)
	if out.err != nil {
		s.finish(x, out, out.err.Status)
		status := out.err.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return access.Reply{}, access.Fail(status, out.err.Code, out.err.Message)
	}
	s.finish(x, out, http.StatusOK)
	return access.OK(p.response(x, out, in)), nil
}

func (p *Playground) response(x *execution, out *outcome, in playgroundRequest) map[string]any {
	c := out.completion
	tools := []map[string]any{}
	for _, call := range c.ToolCalls {
		tools = append(tools, map[string]any{"id": call.ID, "name": call.Name, "arguments": call.Arguments})
	}
	routing := []map[string]any{}
	for _, fact := range x.facts {
		routing = append(routing, map[string]any{
			"target_id":          fact.TargetID,
			"provider_id":        fact.ProviderID,
			"upstream_model":     fact.UpstreamModel,
			"eligible":           true,
			"priority":           attemptPriority(x, fact.TargetID),
			"strategy":           "weighted",
			"attempt":            fact.Ordinal,
			"credential_slot_id": fact.SlotID,
			"reason":             fact.Class,
		})
	}
	res := map[string]any{
		"id":          x.request.id,
		"model":       x.route.Slug,
		"output_text": c.OutputText,
		"tool_calls":  tools,
		"latency_ms":  p.Gateway.now().Sub(x.request.startedAt).Milliseconds(),
		"routing":     routing,
	}
	if c.FinishReason != "" {
		res["finish_reason"] = c.FinishReason
	}
	if c.ProviderModel != "" {
		res["provider_model"] = c.ProviderModel
	}
	if c.Refusal != "" {
		res["refusal"] = c.Refusal
	}
	if c.Usage != nil {
		res["usage"] = c.Usage
	}
	if in.ResponseFormat != nil && in.ResponseFormat.Type != "text" && c.OutputText != "" {
		var structured any
		if json.Unmarshal([]byte(c.OutputText), &structured) == nil {
			res["structured_output"] = structured
		}
	}
	return res
}

func attemptPriority(x *execution, targetID string) int {
	for _, a := range x.attempts {
		if a.TargetID == targetID {
			return a.Priority
		}
	}
	return 0
}

func uuidString() string { return access.NewID() }
