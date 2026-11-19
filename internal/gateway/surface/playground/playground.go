package playground

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"slices"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/publishedprompt"
)

// PlaygroundTimeout bounds one console playground call end to end.
const Timeout = 2 * time.Minute

const BodyLimit = 1 << 20

// Playground serves the console's unary test surface through the same
// executor as API traffic, admitting a console session or management token
// that holds the playground operation.
type API struct {
	kernel *kernel.Server
}

func NewAPI(k *kernel.Server) *API { return &API{kernel: k} }

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
	Model           string               `json:"model"`
	Operation       string               `json:"operation"`
	Input           string               `json:"input"`
	Request         json.RawMessage      `json:"request"`
	Stream          *bool                `json:"stream"`
	Surface         *string              `json:"surface"`
	Temperature     *float64             `json:"temperature"`
	MaxOutputTokens *int64               `json:"max_output_tokens"`
	Tools           []playgroundTool     `json:"tools"`
	ResponseFormat  *playgroundFormat    `json:"response_format"`
	Routing         *routing.Preferences `json:"routing"`
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

func (in *playgroundRequest) parse(operation, mode string, run *kernel.MemberGeneration) (kernel.ParsedRequest, error) {
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

func (p *API) member(r *http.Request, principal authz.Principal, in playgroundRequest) *kernel.MemberGeneration {
	return p.kernel.MemberGeneration(r, kernel.Member{UserID: principal.ID, Preferences: in.Routing, Document: in.Request, Admit: p.authorize(principal)})
}

func (p *API) authorize(principal authz.Principal) func(*routing.Route) *fault.Error {
	return func(route *routing.Route) *fault.Error {
		if principal.Project(route.ProjectID, authz.View) != nil {
			return fault.ModelNotFound(route.Slug)
		}
		return nil
	}
}

func (p *API) Handle(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
	var in playgroundRequest
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	if err := in.validate(false); err != nil {
		return httpkit.Reply{}, err
	}
	run := p.member(r, principal, in)
	parsed, err := in.parse(in.operation(), "unary", run)
	if err != nil {
		return httpkit.Reply{}, err
	}
	out := run.Run(r.Context(), parsed, nil)
	if e := out.Err; e != nil {
		return httpkit.Reply{}, httpkit.Fail(cmp.Or(e.Status, http.StatusBadGateway), e.Code, e.Message)
	}
	return httpkit.Reply{
		Status:              http.StatusOK,
		Body:                playgroundResponse(run.Trail(), out.Completion, in),
		DisableHTMLEscaping: in.operation() != "generation",
	}, nil
}

func routingEvidence(trail kernel.MemberTrail) []map[string]any {
	evidenceRows := []map[string]any{}
	for _, fact := range trail.Facts {
		var evidence routing.Decision
		for _, decision := range trail.Decisions {
			if decision.TargetID == fact.TargetID && decision.CredentialSlotID != nil && *decision.CredentialSlotID == fact.SlotID {
				evidence = decision
				break
			}
		}
		evidenceRows = append(evidenceRows, map[string]any{
			"target_id":      fact.TargetID,
			"provider_id":    fact.ProviderID,
			"upstream_model": fact.UpstreamModel,
			"eligible":       true,
			"priority":       attemptPriority(trail, fact.TargetID),
			"strategy":       fact.Strategy,
			"price":          fact.Price, "vendor_id": fact.VendorID, "performance": evidence.Performance, "metadata_observed_at": evidence.MetadataObservedAt,
			"attempt":            fact.Ordinal,
			"credential_slot_id": fact.SlotID,
			"reason":             fact.Class,
		})
	}
	return evidenceRows
}

func playgroundResponse(trail kernel.MemberTrail, c *wire.Completion, in playgroundRequest) map[string]any {
	routing := routingEvidence(trail)
	if in.operation() != "generation" {
		res := map[string]any{
			"id":          trail.ID,
			"model":       trail.Route,
			"output_text": "",
			"tool_calls":  []map[string]any{},
			"latency_ms":  trail.Elapsed.Milliseconds(),
			"routing":     routing,
		}
		if json.Valid(c.Body) {
			// Keep the operation-owned native result intact. Decoding to any
			// rounds scores, vector values and large integer metadata to float64.
			res["response"] = json.RawMessage(c.Body)
			res["response_raw"] = string(c.Body)
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
		"id":          trail.ID,
		"model":       trail.Route,
		"output_text": c.OutputText,
		"tool_calls":  tools,
		"latency_ms":  trail.Elapsed.Milliseconds(),
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
		decoder := json.NewDecoder(strings.NewReader(c.OutputText))
		decoder.UseNumber()
		var structured any
		if decoder.Decode(&structured) == nil && decoder.Decode(new(any)) == io.EOF {
			res["structured_output"] = structured
		}
	}
	return res
}

func attemptPriority(trail kernel.MemberTrail, targetID string) int {
	for _, a := range trail.Attempts {
		if a.TargetID == targetID {
			return a.Priority
		}
	}
	return 0
}

func (p *API) Stream(w http.ResponseWriter, r *http.Request, principal authz.Principal) error {
	var in playgroundRequest
	if err := httpkit.Decode(r, &in); err != nil {
		return err
	}
	if err := in.validate(true); err != nil {
		return err
	}
	run := p.member(r, principal, in)
	parsed, err := in.parse("generation", "streaming", run)
	if err != nil {
		return err
	}
	sw := &playgroundStreamWriter{w: w, maxTotal: p.kernel.Config.MaxResponseBytes}
	out := run.Run(r.Context(), parsed, sw.emit)
	if out.Cancelled {
		return nil
	}
	if out.Err != nil && !sw.committed {
		return httpkit.Fail(out.Err.Status, out.Err.Code, out.Err.Message)
	}
	sw.finish(run.Trail(), out)
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
	return sw.write("frame", payload, true)
}

// write sends one SSE event. Only provider frames count against maxTotal: the
// terminal done or error event is gateway-generated and must still arrive after
// the frames have exhausted the budget, or the client sees a silent truncation.
func (sw *playgroundStreamWriter) write(event string, data []byte, limited bool) error {
	chunk := int64(len(event) + len(data) + len("event: \ndata: \n\n"))
	if limited && sw.total+chunk > sw.maxTotal {
		return kernel.ErrResponseTooLarge
	}
	rc := http.NewResponseController(sw.w)
	if !sw.committed {
		// The playground answers a signed-in member, not an API key: there is no
		// allowance or key policy to state, so it adds no response headers.
		sw.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		sw.w.Header().Set("X-Accel-Buffering", "no")
		sw.w.WriteHeader(http.StatusOK)
		sw.committed = true
	}
	rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if _, err := fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	sw.total += chunk
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	return nil
}

func (sw *playgroundStreamWriter) finish(trail kernel.MemberTrail, out *kernel.Outcome) {
	if out.Err != nil {
		data, _ := json.Marshal(map[string]any{"code": out.Err.Code, "message": out.Err.Message, "status": out.Err.Status})
		_ = sw.write("error", data, false)
		return
	}
	meta := map[string]any{
		"id":      trail.ID,
		"model":   trail.Route,
		"routing": routingEvidence(trail),
	}
	if out.Completion != nil && out.Completion.Usage != nil {
		meta["usage"] = out.Completion.Usage
	}
	data, _ := json.Marshal(meta)
	_ = sw.write("done", data, false)
}
