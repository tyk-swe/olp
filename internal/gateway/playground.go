package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/telemetry"
	"slices"
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
	mux.HandleFunc("POST /api/v3/playground/stream", p.Access.HandleStream(1<<20, playgroundTimeout, p.stream))
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
	Operation       string              `json:"operation"`
	Input           string              `json:"input"`
	Request         json.RawMessage     `json:"request"`
	Stream          *bool               `json:"stream"`
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

func (in *playgroundRequest) operation() string {
	if in.Operation == "" {
		return "generation"
	}
	return in.Operation
}

func (in *playgroundRequest) surface() string {
	if in.Surface == nil {
		return "openai"
	}
	return *in.Surface
}

func (in *playgroundRequest) validate(streaming bool) error {
	switch in.operation() {
	case "generation", "token_count", "embeddings", "moderation", "rerank":
	default:
		return access.Invalid("operation", "operation must be generation, token_count, embeddings, moderation, or rerank")
	}
	switch {
	case strings.TrimSpace(in.Model) == "":
		return access.Invalid("model", "model is required")
	case in.Surface != nil && !slices.Contains([]string{"openai", "anthropic", "gemini"}, *in.Surface):
		return access.Invalid("surface", "surface must be openai, anthropic, or gemini")
	case in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2):
		return access.Invalid("temperature", "temperature must be between 0 and 2")
	case in.MaxOutputTokens != nil && (*in.MaxOutputTokens < 1 || *in.MaxOutputTokens > 1<<20):
		return access.Invalid("max_output_tokens", "max_output_tokens must be between 1 and 1048576")
	case len(in.Tools) > 128:
		return access.Invalid("tools", "at most 128 tools are allowed")
	}
	if in.Request != nil {
		if in.Input != "" || len(in.Tools) > 0 || in.ResponseFormat != nil || in.Temperature != nil || in.MaxOutputTokens != nil {
			return access.Invalid("request", "request cannot be combined with input, tools, response_format, temperature, or max_output_tokens")
		}
	} else {
		if in.Input == "" {
			return access.Invalid("input", "input is required")
		}
		if in.operation() != "generation" {
			return access.Invalid("operation", "non-generation operations require a raw request document")
		}
	}
	if streaming {
		if in.Stream == nil || !*in.Stream {
			return access.Invalid("stream", "stream must be true for the streaming playground")
		}
		if in.operation() != "generation" {
			return access.Invalid("operation", "streaming supports generation only")
		}
	} else if in.Stream != nil && *in.Stream {
		return access.Invalid("stream", "streaming requests use the streaming playground endpoint")
	}
	if in.operation() != "generation" && in.operation() != "token_count" && in.surface() != "openai" {
		return access.Invalid("surface", "this operation is available on the openai surface only")
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

func (in *playgroundRequest) parse(operation, mode string) (*openai.Request, openai.Family, error) {
	if in.Request != nil {
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(in.Request, &fields); err != nil || fields == nil {
			return nil, "", access.Invalid("request", "request must be a JSON object")
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
			return nil, "", access.Fail(http.StatusUnprocessableEntity, "validation_failed", err.Error())
		}
		return parsed, parsed.Family, nil
	}
	body, err := in.chatBody()
	if err != nil {
		return nil, "", access.Invalid("input", "the request could not be encoded")
	}
	parsed, err := openai.Parse(openai.FamilyChat, body)
	if err != nil {
		return nil, "", access.Fail(http.StatusUnprocessableEntity, "validation_failed", err.Error())
	}
	family := openai.FamilyChat
	if in.Surface != nil && (*in.Surface == "anthropic" || *in.Surface == "gemini") {
		kind := *in.Surface
		if in.MaxOutputTokens == nil {
			fields := parsed.Document()
			fields["max_completion_tokens"] = json.RawMessage("4096")
			body, _ = json.Marshal(fields)
			parsed, _ = openai.Parse(openai.FamilyChat, body)
		}
		encoded, wire, e := protocols.Encode(parsed, kind, kind, in.Model, nil)
		if e != nil {
			return nil, "", access.Invalid("input", e.Error())
		}
		parsed, e = protocols.Parse(wire, encoded, in.Model)
		if e != nil {
			return nil, "", access.Invalid("input", e.Error())
		}
		family = wire
	}
	return parsed, family, nil
}

func (p *Playground) execution(r *http.Request, principal access.Principal, parsed *openai.Request, family openai.Family, preferences *routes.Preferences) *execution {
	s := p.Gateway
	return &execution{
		request:     request{id: uuidString(), minted: true, clientIP: ClientIP(r, s.cfg.TrustedProxies), startedAt: s.now(), release: s.Runtime.Release(), trace: telemetry.RequestFromContext(r.Context())},
		family:      family,
		preferences: preferences,
		parsed:      parsed,
		actor:       "playground",
		userID:      principal.ID,
		affinity:    []byte(principal.ID),
	}
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
	if err := in.validate(false); err != nil {
		return access.Reply{}, err
	}
	parsed, family, err := in.parse(in.operation(), "unary")
	if err != nil {
		return access.Reply{}, err
	}
	s := p.Gateway
	x := p.execution(r, principal, parsed, family, in.Routing)
	if e := s.prepare(r.Context(), x, func(string) bool { return principal.CanProject(x.route.ProjectID, false) }); e != nil {
		x.failure = e
		s.finish(x, nil, e.Status)
		return access.Reply{}, access.Fail(e.Status, e.Code, e.Message)
	}
	if e := s.enforceContentPolicy(x); e != nil {
		x.failure = e
		s.finish(x, nil, e.Status)
		return access.Reply{}, access.Fail(e.Status, e.Code, e.Message)
	}
	x.estimate = requestEstimate(x)
	if !s.admit(r.Context()) {
		x.failure = overloaded
		s.finish(x, nil, overloaded.Status)
		return access.Reply{}, access.Fail(overloaded.Status, overloaded.Code, overloaded.Message)
	}
	defer s.release(r.Context())
	out := s.execute(r.Context(), x)
	if out.err != nil {
		s.finish(x, out, out.err.Status)
		status := out.err.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return access.Reply{}, access.Fail(status, out.err.Code, out.err.Message)
	}
	if e := s.enforceCompletionOutput(x, out.completion); e != nil {
		s.finish(x, out, e.Status)
		return access.Reply{}, access.Fail(e.Status, e.Code, e.Message)
	}
	s.finish(x, out, http.StatusOK)
	return access.OK(p.response(x, out, in)), nil
}

func routingEvidence(x *execution) []map[string]any {
	routing := []map[string]any{}
	for _, fact := range x.facts {
		var evidence runtime.Decision
		for _, decision := range x.decisions {
			if decision.TargetID == fact.TargetID && decision.CredentialSlotID != nil && *decision.CredentialSlotID == fact.SlotID {
				evidence = decision
				break
			}
		}
		routing = append(routing, map[string]any{
			"target_id":      fact.TargetID,
			"provider_id":    fact.ProviderID,
			"upstream_model": fact.UpstreamModel,
			"eligible":       true,
			"priority":       attemptPriority(x, fact.TargetID),
			"strategy":       fact.Strategy,
			"price":          fact.Price, "vendor_id": fact.VendorID, "performance": evidence.Performance, "metadata_observed_at": evidence.MetadataObservedAt,
			"attempt":            fact.Ordinal,
			"credential_slot_id": fact.SlotID,
			"reason":             fact.Class,
		})
	}
	return routing
}

func (p *Playground) response(x *execution, out *outcome, in playgroundRequest) map[string]any {
	c := out.completion
	routing := routingEvidence(x)
	if in.operation() != "generation" {
		res := map[string]any{
			"id":          x.request.id,
			"model":       x.route.Slug,
			"output_text": "",
			"tool_calls":  []map[string]any{},
			"latency_ms":  p.Gateway.now().Sub(x.request.startedAt).Milliseconds(),
			"routing":     routing,
		}
		var document any
		if json.Unmarshal(c.Body, &document) == nil {
			res["response"] = document
		}
		if c.Usage != nil {
			res["usage"] = c.Usage
		}
		return res
	}
	tools := []map[string]any{}
	for _, call := range c.ToolCalls {
		tools = append(tools, map[string]any{"id": call.ID, "name": call.Name, "arguments": call.Arguments})
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

func (p *Playground) stream(w http.ResponseWriter, r *http.Request) error {
	principal, err := p.Access.Principal(r, p.Access.Pool, "playground")
	if err != nil {
		return err
	}
	var in playgroundRequest
	if err := access.Decode(r, &in); err != nil {
		return err
	}
	if err := in.validate(true); err != nil {
		return err
	}
	parsed, family, err := in.parse("generation", "streaming")
	if err != nil {
		return err
	}
	s := p.Gateway
	x := p.execution(r, principal, parsed, family, in.Routing)
	if e := s.prepare(r.Context(), x, func(string) bool { return principal.CanProject(x.route.ProjectID, false) }); e != nil {
		x.failure = e
		s.finish(x, nil, e.Status)
		return access.Fail(e.Status, e.Code, e.Message)
	}
	if e := s.enforceContentPolicy(x); e != nil {
		x.failure = e
		s.finish(x, nil, e.Status)
		return access.Fail(e.Status, e.Code, e.Message)
	}
	x.estimate = requestEstimate(x)
	if !s.admit(r.Context()) {
		x.failure = overloaded
		s.finish(x, nil, overloaded.Status)
		return access.Fail(overloaded.Status, overloaded.Code, overloaded.Message)
	}
	defer s.release(r.Context())
	sw := &playgroundStreamWriter{w: w, maxTotal: s.cfg.MaxResponseBytes}
	x.emit = sw.emit
	out := s.execute(r.Context(), x)
	status := http.StatusOK
	if out.err != nil && out.err.Status > 0 {
		status = out.err.Status
	}
	s.finish(x, out, status)
	if out.cancelled {
		return nil
	}
	if out.err != nil && !sw.committed {
		return access.Fail(out.err.Status, out.err.Code, out.err.Message)
	}
	sw.finish(x, out)
	return nil
}

type playgroundStreamWriter struct {
	w         http.ResponseWriter
	committed bool
	total     int64
	maxTotal  int64
}

func (sw *playgroundStreamWriter) emit(frame []byte) error {
	payload, _ := json.Marshal(map[string]any{"data": string(frame)})
	return sw.write("frame", payload)
}

func (sw *playgroundStreamWriter) write(event string, data []byte) error {
	chunk := int64(len(event) + len(data) + len("event: \ndata: \n\n"))
	if sw.total+chunk > sw.maxTotal {
		return errResponseTooLarge
	}
	rc := http.NewResponseController(sw.w)
	if !sw.committed {
		sw.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		sw.w.Header().Set("X-Accel-Buffering", "no")
		sw.w.WriteHeader(http.StatusOK)
		sw.committed = true
	}
	rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if _, err := fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return fmt.Errorf("%w: %v", errClientWrite, err)
	}
	sw.total += chunk
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("%w: %v", errClientWrite, err)
	}
	return nil
}

func (sw *playgroundStreamWriter) finish(x *execution, out *outcome) {
	if out.err != nil {
		data, _ := json.Marshal(map[string]any{"code": out.err.Code, "message": out.err.Message, "status": out.err.Status})
		_ = sw.write("error", data)
		return
	}
	meta := map[string]any{
		"id":      x.request.id,
		"model":   x.route.Slug,
		"routing": routingEvidence(x),
	}
	if out.completion != nil && out.completion.Usage != nil {
		meta["usage"] = out.completion.Usage
	}
	data, _ := json.Marshal(meta)
	_ = sw.write("done", data)
}

func uuidString() string { return access.NewID() }
