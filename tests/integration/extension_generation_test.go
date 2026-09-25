//go:build integration && extension

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/tests/fidelity"
)

// The fixture generation dialect is compiled test code registered through the
// same trusted composition seam built-ins use. Its grammar deliberately looks
// nothing like Chat, Messages or generateContent — prompts, invocations and a
// meter — so the end-to-end behavior below can only pass when generic
// orchestration really consumes the registered contracts instead of an
// OpenAI-family switch.
const fixtureGenerationLabel = "fixture-generation"
const fixtureContractVersion = "fixture-tools-v1"

var fixtureGeneration = oif.Identity{ID: fixtureGenerationLabel, Revision: "fixture-v1"}

func registerFixtureGeneration(t *testing.T) {
	t.Helper()
	dialect := generation.Dialect{
		Identity:  fixtureGeneration,
		Operation: generation.Contract(),
		Surface:   "fixture",
		Label:     fixtureGenerationLabel,
		Evidence:  "fixture-generation/fixture-grammar-v1",
		Streaming: true,
		Address:   generation.Address{RelativePath: "generate"},
		IdentityRules: []oif.IdentityRule{
			{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String},
		},
		Lift:            fixtureLift,
		StreamField:     "stream",
		ValidateSource:  fixtureSourceConsistency,
		IdentityChanges: fixtureIdentity,
		DecodeNative:    fixtureDecode,
		StreamNative:    fixtureStream,
		ValidateEvent:   fixtureEventGuard,
		EventActionable: func(event oif.Event) bool { return event.Name() == "fixture.call" },
		TerminalControl: "[END]",
		BindResultModel: func(document oif.Document, route string) ([]oif.Change, error) {
			if _, present := document.Lookup("/model"); !present {
				return nil, nil
			}
			model, _ := json.Marshal(route)
			return []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published response model"}}, nil
		},
		// The fixture grammar declares no provider-state or hosted tool
		// effects, so its admission list is empty by construction.
		Effects:         func(oif.Document, generation.StateInput, *oif.Obligations) ([]string, error) { return nil, nil },
		Estimate:        fixtureEstimate,
		Parameters:      fixtureParameters,
		MeaningfulFrame: func(frame []byte) bool { return bytes.Contains(frame, []byte(`"text"`)) },
		Probe:           fixtureProbe,
	}
	mapping := generation.Mapping{
		Source: protocols.DialectChat, Target: fixtureGeneration,
		ClientContract: fixtureContractVersion,
		Evidence:       "fixture-generation/ordered-tools-v1",
		Lower:          lowerFixtureTools,
		ProjectResult:  projectFixtureToolsUnary,
		ProjectEvents:  projectFixtureToolEvents,
	}
	operationregistry.RegisterGenerationDialects([]generation.Dialect{dialect}, []generation.Mapping{mapping})
}

// --- fixture dialect codec hooks -------------------------------------------

func fixtureReject(field, requirement string) (generation.Source, error) {
	return generation.Source{}, generation.Incompatible("target_capability", field, requirement, "The fixture source grammar is invalid.")
}

func fixtureLift(body []byte, route string, transportStream bool, maxBytes int) (generation.Source, error) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxBytes})
	if err != nil {
		return generation.Source{}, err
	}
	root := document.Root()
	if root.Kind() != oif.Object || !generation.OnlyMembers(root, "model stream prompts tools budget") {
		return fixtureReject("/request", "fixture_source_grammar")
	}
	if generation.Member(root, "model").Kind() != oif.String || generation.Text(generation.Member(root, "model")) == "" {
		return fixtureReject("/model", "model_identity")
	}
	stream := transportStream
	if flag, present := root.Lookup("stream"); present {
		if flag.Kind() != oif.Boolean {
			return fixtureReject("/stream", "delivery_presence")
		}
		stream = flag.Raw() == "true"
	}
	prompts := generation.Member(root, "prompts")
	if prompts.Kind() != oif.Array || len(prompts.Elements()) == 0 {
		return fixtureReject("/prompts", "ordered_prompt_history")
	}
	for _, prompt := range prompts.Elements() {
		role := generation.Text(generation.Member(prompt, "role"))
		switch role {
		case "user":
			if !generation.OnlyMembers(prompt, "role content") || generation.Member(prompt, "content").Kind() != oif.String {
				return fixtureReject("/prompts", "prompt_shape")
			}
		case "assistant":
			if !generation.OnlyMembers(prompt, "role content invocations") || generation.Member(prompt, "content").Kind() != oif.String {
				return fixtureReject("/prompts", "prompt_shape")
			}
			for _, call := range generation.Member(prompt, "invocations").Elements() {
				if !generation.OnlyMembers(call, "id name arguments") || generation.Member(call, "id").Kind() != oif.String || generation.Member(call, "name").Kind() != oif.String || generation.Member(call, "arguments").Kind() != oif.String {
					return fixtureReject("/prompts", "invocation_shape")
				}
			}
		case "tool":
			if !generation.OnlyMembers(prompt, "role call_id content") || generation.Member(prompt, "call_id").Kind() != oif.String || generation.Member(prompt, "content").Kind() != oif.String {
				return fixtureReject("/prompts", "tool_result_shape")
			}
		default:
			return fixtureReject("/prompts", "prompt_role")
		}
	}
	for _, tool := range generation.Member(root, "tools").Elements() {
		if !generation.OnlyMembers(tool, "name description schema") || generation.Member(tool, "name").Kind() != oif.String || generation.Text(generation.Member(tool, "name")) == "" || generation.Member(tool, "schema").Kind() != oif.Object {
			return fixtureReject("/tools", "capability_shape")
		}
	}
	if budget := generation.Member(root, "budget"); budget.Kind() != oif.Absent && !generation.NonnegativeInteger(budget) {
		return fixtureReject("/budget", "output_budget")
	}
	request, err := oif.NewRequest(generation.Descriptor(fixtureGeneration, stream), document)
	if err != nil {
		return generation.Source{}, err
	}
	return generation.Source{Request: request, Route: route, Stream: stream, IncludeUsage: true}, nil
}

// fixtureSourceConsistency keeps the admitted delivery flag in agreement with
// the envelope the dialect lifted — the same own-grammar invariant the
// checked-in dialects declare.
func fixtureSourceConsistency(source generation.Source) error {
	flag := generation.Member(source.Document().Root(), "stream")
	if (flag.Raw() == "true") != source.Stream {
		return generation.Incompatible("target_capability", "/stream", "source_delivery_consistency", "The selected delivery mode disagrees with the native source.")
	}
	return nil
}

func fixtureIdentity(source generation.Source, model string) ([]oif.Change, error) {
	quoted, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	return []oif.Change{{Pointer: "/model", Value: string(quoted), Origin: oif.IdentityBinding, Reason: "published model binding"}}, nil
}

// fixtureResult is the dialect's unary result grammar: identity, the emitted
// assistant prompt, a declared terminal record and metered tokens. Terminal
// reasons stay inside the carrier's admitted vocabulary so the committed
// continuation record can never name a terminal the contract cannot decode.
func fixtureResult(document oif.Document) error {
	root := document.Root()
	if !generation.OnlyMembers(root, "id type model prompt stop meter") || generation.Text(generation.Member(root, "type")) != "fixture.completion" || generation.Text(generation.Member(root, "id")) == "" {
		return generation.GuardFailure("/result", "fixture_result_identity")
	}
	prompt := generation.Member(root, "prompt")
	if !generation.OnlyMembers(prompt, "role content invocations") || generation.Text(generation.Member(prompt, "role")) != "assistant" || generation.Member(prompt, "content").Kind() != oif.String {
		return generation.GuardFailure("/prompt", "assistant_prompt")
	}
	for _, call := range generation.Member(prompt, "invocations").Elements() {
		if !generation.OnlyMembers(call, "id name arguments") || generation.Member(call, "id").Kind() != oif.String || generation.Text(generation.Member(call, "id")) == "" || generation.Member(call, "name").Kind() != oif.String || generation.Member(call, "arguments").Kind() != oif.String {
			return generation.GuardFailure("/prompt/invocations", "ordered_call")
		}
	}
	stop := generation.Member(root, "stop")
	if !generation.OnlyMembers(stop, "reason matched") || !contains([]string{"end_turn", "tool_use", "max_tokens"}, generation.Text(generation.Member(stop, "reason"))) {
		return generation.GuardFailure("/stop", "terminal_reason")
	}
	if matched := generation.Member(stop, "matched"); matched.Kind() != oif.Absent && matched.Kind() != oif.Null && matched.Kind() != oif.String {
		return generation.GuardFailure("/stop/matched", "matched_sequence")
	}
	meter := generation.Member(root, "meter")
	if !generation.OnlyMembers(meter, "input output") || !generation.NonnegativeInteger(generation.Member(meter, "input")) || !generation.NonnegativeInteger(generation.Member(meter, "output")) {
		return generation.GuardFailure("/meter", "token_metering")
	}
	return nil
}

func fixtureNative(document oif.Document, body []byte, route string, stream bool) (*generation.Native, error) {
	root := document.Root()
	prompt := generation.Member(root, "prompt")
	stop := generation.Member(root, "stop")
	meter := generation.Member(root, "meter")
	reason := generation.Text(generation.Member(stop, "reason"))
	finish := map[string]string{"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length"}[reason]
	var input, output int64
	if v, ok := generation.Integer(generation.Member(meter, "input").Bytes()); ok {
		input = v
	}
	if v, ok := generation.Integer(generation.Member(meter, "output").Bytes()); ok {
		output = v
	}
	calls := []generation.ToolCall{}
	for _, call := range generation.Member(prompt, "invocations").Elements() {
		calls = append(calls, generation.ToolCall{ID: generation.Text(generation.Member(call, "id")), Name: generation.Text(generation.Member(call, "name")), Arguments: generation.Text(generation.Member(call, "arguments"))})
	}
	result, err := oif.NewResult(generation.Descriptor(fixtureGeneration, stream), document, oif.Complete)
	if err != nil {
		return nil, err
	}
	return &generation.Native{
		Result: result, Body: body, Route: route,
		UpstreamID: generation.Text(generation.Member(root, "id")), ProviderModel: generation.Text(generation.Member(root, "model")),
		FinishReason: finish, OutputText: generation.Text(generation.Member(prompt, "content")),
		ToolCalls: calls, Usage: &generation.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output},
	}, nil
}

func fixtureDecode(in generation.DecodeInput) (*generation.Native, error) {
	document, err := oif.ParseJSON(in.Body, oif.Limits{MaxBytes: in.MaxBytes})
	if err != nil {
		return nil, err
	}
	if err := fixtureResult(document); err != nil {
		return nil, err
	}
	return fixtureNative(document, in.Body, in.Route, false)
}

// fixtureStream is the dialect-owned reducer for its own SSE grammar: it
// validates every event before it is observed or emitted, admits the declared
// terminal control frame, and reconstructs the native result from admitted
// events only.
func fixtureStream(in generation.StreamInput, emit func([]byte) error, observe func(oif.Event) error) (*generation.Native, error) {
	descriptor := generation.Descriptor(fixtureGeneration, true)
	scanner := bufio.NewScanner(in.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), in.MaxEventBytes)
	var eventName string
	var data strings.Builder
	var raw bytes.Buffer
	var sequence uint64
	var text strings.Builder
	var calls []map[string]string
	var end oif.Value
	flush := func() error {
		payload := strings.TrimSpace(data.String())
		if payload == "" {
			return nil
		}
		defer func() { eventName, data, raw = "", strings.Builder{}, bytes.Buffer{} }()
		if payload == "[END]" {
			event := oif.ControlEvent(descriptor, "", "[END]", sequence)
			sequence++
			if err := observe(event); err != nil {
				return err
			}
			return emit(append(bytes.Clone(raw.Bytes()), '\n'))
		}
		document, err := oif.ParseJSON([]byte(payload), oif.Limits{MaxBytes: in.MaxEventBytes})
		if err != nil {
			return err
		}
		event, err := oif.NewEvent(descriptor, document, eventName, sequence)
		if err != nil {
			return err
		}
		sequence++
		// The decode-time grammar check carries no hosted admission; the
		// plan-level guard bounds observed events separately.
		if err := fixtureEventGuard(event, nil); err != nil {
			return err
		}
		if err := observe(event); err != nil {
			return err
		}
		if err := emit(append(bytes.Clone(raw.Bytes()), '\n')); err != nil {
			return err
		}
		switch eventName {
		case "fixture.delta":
			text.WriteString(generation.Text(generation.Member(document.Root(), "text")))
		case "fixture.call":
			call := generation.Member(document.Root(), "call")
			calls = append(calls, map[string]string{"id": generation.Text(generation.Member(call, "id")), "name": generation.Text(generation.Member(call, "name")), "arguments": generation.Text(generation.Member(call, "arguments"))})
		case "fixture.end":
			end = document.Root()
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		raw.WriteString(line)
		raw.WriteString("\n")
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			eventName = name
		} else if payload, ok := strings.CutPrefix(line, "data: "); ok {
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(payload)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if end.Kind() == oif.Absent {
		return nil, generation.GuardFailure("/stop", "terminal_boundary")
	}
	invocations, _ := json.Marshal(calls)
	resultBody, _ := json.Marshal(map[string]any{
		"id": "fx-stream", "type": "fixture.completion", "model": in.Route,
		"prompt": map[string]any{"role": "assistant", "content": text.String(), "invocations": json.RawMessage(invocations)},
		"stop":   json.RawMessage(generation.Member(end, "stop").Bytes()),
		"meter":  json.RawMessage(generation.Member(end, "meter").Bytes()),
	})
	document, err := oif.ParseJSON(resultBody, oif.Limits{MaxBytes: in.MaxEventBytes})
	if err != nil {
		return nil, err
	}
	return fixtureNative(document, resultBody, in.Route, true)
}

// fixtureEventGuard is the dialect's per-event admission grammar. The fixture
// admits no provider-hosted tool effects, so the hosted list the plan admitted
// is bounded by construction and never consulted here.
func fixtureEventGuard(event oif.Event, _ []string) error {
	if event.Control() != "" {
		if event.Control() == "[END]" {
			return nil
		}
		return generation.GuardFailure("/events", "terminal_control")
	}
	root := event.Source().Root()
	switch event.Name() {
	case "fixture.delta":
		if !generation.OnlyMembers(root, "type text") || generation.Text(generation.Member(root, "type")) != "fixture.delta" || generation.Member(root, "text").Kind() != oif.String {
			return generation.GuardFailure("/events", "delta_shape")
		}
	case "fixture.call":
		call := generation.Member(root, "call")
		if !generation.OnlyMembers(root, "type call") || generation.Text(generation.Member(root, "type")) != "fixture.call" || !generation.OnlyMembers(call, "id name arguments") || generation.Text(generation.Member(call, "id")) == "" || generation.Member(call, "name").Kind() != oif.String || generation.Member(call, "arguments").Kind() != oif.String {
			return generation.GuardFailure("/events", "call_shape")
		}
	case "fixture.end":
		stop := generation.Member(root, "stop")
		meter := generation.Member(root, "meter")
		if !generation.OnlyMembers(root, "type stop meter") || generation.Text(generation.Member(root, "type")) != "fixture.end" || !generation.OnlyMembers(stop, "reason matched") || !contains([]string{"end_turn", "tool_use", "max_tokens"}, generation.Text(generation.Member(stop, "reason"))) || !generation.OnlyMembers(meter, "input output") || !generation.NonnegativeInteger(generation.Member(meter, "input")) || !generation.NonnegativeInteger(generation.Member(meter, "output")) {
			return generation.GuardFailure("/events", "terminal_shape")
		}
	default:
		return generation.GuardFailure("/events", "unregistered_event")
	}
	return nil
}

func fixtureEstimate(effective oif.Document) generation.Estimate {
	root := effective.Root()
	estimate := generation.Estimate{Input: generation.EstimateText(generation.Member(root, "prompts").Bytes()) + generation.EstimateItems(generation.Member(root, "tools").Bytes()), Candidates: 1}
	if budget, ok := generation.Integer(generation.Member(root, "budget").Bytes()); ok && budget > 0 {
		estimate.Output = &budget
	}
	return estimate
}

func fixtureParameters(effective oif.Document) []string {
	out := []string{}
	for _, member := range effective.Root().Members() {
		if member.Name != "model" && member.Name != "prompts" {
			out = append(out, member.Name)
		}
	}
	return out
}

func fixtureProbe(model string, stream bool) []byte {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": stream, "budget": 16, "prompts": []any{map[string]any{"role": "user", "content": "Reply with OK."}}})
	return body
}

// --- fixture qualified mapping: ordered Chat tool history over the fixture
// grammar, under the negotiated fixture-tools-v1 continuation contract ------

type fixtureProjection struct {
	in          generation.ProjectInput
	route       string
	stream      bool
	limit       int
	frames      []json.RawMessage
	content     strings.Builder
	calls       []map[string]any
	invocations []map[string]string
	meter       map[string]json.RawMessage
	reason      string
	matched     *json.RawMessage
	ended       bool
	// Separately bounded resources: dependency is the retained native state
	// materialized into the persisted Continuation; delivery is projected
	// client output retained for replay; transient is live in-flight event
	// memory; eventWork is the aggregate admitted-event work counter.
	dependency, delivery, transient, eventWork int
}

func projectFixtureToolEvents(in generation.ProjectInput) (generation.Projection, error) {
	if in.Limit < 1 || in.Limit > 4<<20 {
		return nil, generation.ContinuationFailure("/client_contract", "bounded_tool_projection")
	}
	return &fixtureProjection{in: in, route: in.Source.Route, stream: in.Source.Stream, limit: in.Limit}, nil
}

// charge debits one named resource budget against the mapping's bounded limit.
func (p *fixtureProjection) charge(field *int, resource string, category generation.LimitCategory, n int) error {
	*field += n
	if n < 0 || *field > p.limit {
		return &generation.Exhaustion{Resource: resource, Category: category, Limit: p.limit}
	}
	return nil
}

func (p *fixtureProjection) chunk(delta map[string]any, observation map[string]any) ([]byte, error) {
	frame, err := json.Marshal(map[string]any{
		"id": "fx-completion", "object": "chat.completion.chunk", "created": 0, "model": p.route,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
		"olp":     map[string]any{"version": fixtureContractVersion, "observation": observation},
	})
	if err != nil {
		return nil, err
	}
	if err := p.charge(&p.delivery, "projected_delivery_bytes", generation.LimitBytes, len(frame)); err != nil {
		return nil, err
	}
	p.frames = append(p.frames, frame)
	return frame, nil
}

// Accounting reports the projection's separated resource counters so callers
// can distinguish retained dependency state, projected replay delivery, live
// transient memory and aggregate admitted event work.
func (p *fixtureProjection) Accounting() generation.Accounting {
	return generation.Accounting{
		RetainedDependencyBytes: p.dependency,
		ProjectedDeliveryBytes:  p.delivery,
		TransientEventBytes:     p.transient,
		AdmittedEventWorkBytes:  p.eventWork,
	}
}

func (p *fixtureProjection) Observe(event oif.Event) ([][]byte, error) {
	if err := p.in.ValidateEvent(event); err != nil {
		return nil, err
	}
	if err := p.charge(&p.eventWork, "admitted_event_work_bytes", generation.LimitEventWork, event.Source().Len()); err != nil {
		return nil, err
	}
	root := event.Source().Root()
	switch event.Name() {
	case "fixture.delta":
		text := generation.Text(generation.Member(root, "text"))
		p.content.WriteString(text)
		if err := p.charge(&p.transient, "transient_event_bytes", generation.LimitBytes, len(text)); err != nil {
			return nil, err
		}
		frame, err := p.chunk(map[string]any{"content": text}, map[string]any{"type": "fixture.delta", "bytes": len(text)})
		if err != nil {
			return nil, err
		}
		return [][]byte{frame}, nil
	case "fixture.call":
		call := generation.Member(root, "call")
		// calls carries the projected client tool-call shape; invocations
		// retains the dialect's own ordered call form for next-turn
		// reconstruction — the dependency is never rebuilt from the client
		// projection.
		invocation := map[string]string{"id": generation.Text(generation.Member(call, "id")), "name": generation.Text(generation.Member(call, "name")), "arguments": generation.Text(generation.Member(call, "arguments"))}
		if err := p.charge(&p.transient, "transient_event_bytes", generation.LimitBytes, len(call.Bytes())); err != nil {
			return nil, err
		}
		p.invocations = append(p.invocations, invocation)
		function := map[string]any{"name": generation.Text(generation.Member(call, "name")), "arguments": generation.Text(generation.Member(call, "arguments"))}
		p.calls = append(p.calls, map[string]any{"id": generation.Text(generation.Member(call, "id")), "type": "function", "function": function})
		frame, err := p.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": len(p.calls) - 1, "id": generation.Text(generation.Member(call, "id")), "type": "function", "function": function}}}, map[string]any{"type": "fixture.call", "id": generation.Text(generation.Member(call, "id"))})
		if err != nil {
			return nil, err
		}
		return [][]byte{frame}, nil
	case "fixture.end":
		stop := generation.Member(root, "stop")
		p.reason = generation.Text(generation.Member(stop, "reason"))
		if matched, present := stop.Lookup("matched"); present {
			raw := matched.Bytes()
			p.matched = (*json.RawMessage)(&raw)
		}
		p.meter = map[string]json.RawMessage{}
		for _, member := range generation.Member(root, "meter").Members() {
			p.meter[member.Name] = member.Value.Bytes()
		}
		p.ended = true
	}
	return nil, nil
}

func (p *fixtureProjection) Complete(native *generation.Native, handle string) (*generation.Continuation, generation.Delivery, error) {
	if !p.ended || native == nil || native.Usage == nil || handle == "" || p.reason == "" {
		return nil, generation.Delivery{}, generation.GuardFailure("/result", "complete_recoverable_result")
	}
	finish := "stop"
	if p.reason == "tool_use" {
		finish = "tool_calls"
	} else if p.reason == "max_tokens" {
		finish = "length"
	}
	terminal := &generation.NativeTerminal{Reason: p.reason, Finish: finish}
	if p.matched != nil {
		terminal.Sequence = *p.matched
	}
	actions := &generation.ContinuationActions{ToolCalls: []string{}}
	if p.reason == "tool_use" {
		for _, call := range p.calls {
			id, _ := call["id"].(string)
			if id == "" {
				return nil, generation.Delivery{}, generation.GuardFailure("/result", "complete_recoverable_result")
			}
			actions.ToolCalls = append(actions.ToolCalls, id)
		}
	}
	assistant := map[string]any{"role": "assistant", "content": p.content.String()}
	if len(p.calls) > 0 {
		assistant["tool_calls"] = p.calls
	}
	assistantRaw, _ := json.Marshal(assistant)
	// The retained native dependency is the ordered invocation list the
	// dialect emitted; reconstruction reuses it verbatim on the next turn.
	blocks, _ := json.Marshal(p.invocations)
	if err := p.charge(&p.dependency, "retained_dependency_bytes", generation.LimitPersistence, len(blocks)+len(assistantRaw)); err != nil {
		return nil, generation.Delivery{}, err
	}
	// The in-flight invocation materialization is now retained dependency
	// state; its transient charge is released.
	p.transient = 0
	state := &generation.Continuation{Version: fixtureContractVersion, Source: p.in.Source.Request.Document().Bytes(), NativeRequest: p.in.Effective.Bytes(), Blocks: blocks, Assistant: assistantRaw}
	usage := map[string]any{"prompt_tokens": native.Usage.InputTokens, "completion_tokens": native.Usage.OutputTokens, "total_tokens": native.Usage.TotalTokens}
	extension := map[string]any{"version": fixtureContractVersion, "handle": handle, "ready": true, "native_usage": p.meter, "native_terminal": terminal, "actions": actions}
	delivery := generation.Delivery{Stream: p.stream, Terminal: terminal, Actions: actions}
	if p.stream {
		terminalFrame, _ := json.Marshal(map[string]any{
			"id": "fx-completion", "object": "chat.completion.chunk", "created": 0, "model": p.route,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
			"usage":   usage, "olp": extension,
		})
		if err := p.charge(&p.delivery, "projected_delivery_bytes", generation.LimitBytes, len(terminalFrame)); err != nil {
			return nil, generation.Delivery{}, err
		}
		delivery.Frames = append(append([]json.RawMessage{}, p.frames...), terminalFrame)
	} else {
		body, _ := json.Marshal(map[string]any{
			"id": "fx-completion", "object": "chat.completion", "created": 0, "model": p.route,
			"choices": []any{map[string]any{"index": 0, "message": assistant, "finish_reason": finish}},
			"usage":   usage, "olp": extension,
		})
		if err := p.charge(&p.delivery, "projected_delivery_bytes", generation.LimitBytes, len(body)); err != nil {
			return nil, generation.Delivery{}, err
		}
		delivery.Body = body
	}
	return state, delivery, nil
}

func lowerFixtureTools(in generation.LowerInput) (generation.Lowered, error) {
	if !in.DurableContinuation {
		return generation.Lowered{}, generation.ContinuationFailure("/client_contract", "encrypted_continuation_authority")
	}
	if !in.AllowProviderState {
		return generation.Lowered{}, generation.Incompatible("policy_conflict", "/client_contract", "allow_provider_state", "This key does not permit retention of the native dependencies needed for a continuation handle.")
	}
	if len(in.Source.Request.Provenance()) != 0 {
		return generation.Lowered{}, generation.ContinuationFailure("/request", "immutable_source_history")
	}
	root := in.Source.Document().Root()
	for _, member := range root.Members() {
		switch member.Name {
		case "model", "messages", "stream", "tools", "max_tokens":
		case "stream_options":
			if !generation.OnlyMembers(member.Value, "include_usage") || generation.Member(member.Value, "include_usage").Raw() != "true" {
				return generation.Lowered{}, generation.ContinuationFailure("/stream_options", "qualified_usage_observation")
			}
		default:
			return generation.Lowered{}, generation.Incompatible("target_capability", generation.SafeField(member.Name), "qualified_source_control", "This source control has no qualified mapping in the negotiated tool contract.")
		}
	}
	fields := map[string]json.RawMessage{}
	if in.Continuation != nil {
		var err error
		fields, err = reconstructFixtureTools(in.Source, in.Continuation)
		if err != nil {
			return generation.Lowered{}, err
		}
	} else {
		prompts, err := fixtureInitialPrompts(generation.Member(root, "messages"))
		if err != nil {
			return generation.Lowered{}, err
		}
		fields["prompts"] = prompts
		if tools, present := root.Lookup("tools"); present {
			mapped, err := fixtureTools(tools)
			if err != nil {
				return generation.Lowered{}, err
			}
			fields["tools"] = mapped
		}
		budget, present := root.Lookup("max_tokens")
		if !present {
			return generation.Lowered{}, generation.Incompatible("reasoning_budget", "/max_tokens", "declared_output_budget", "The fixture contract requires an explicit positive output budget.")
		}
		var limit int64
		if json.Unmarshal(budget.Bytes(), &limit) != nil || limit < 1 {
			return generation.Lowered{}, generation.Incompatible("reasoning_budget", "/max_tokens", "declared_output_budget", "The fixture contract requires an explicit positive output budget.")
		}
		fields["budget"] = budget.Bytes()
	}
	fields["model"], _ = json.Marshal(in.Model)
	fields["stream"] = json.RawMessage("false")
	if stream, present := root.Lookup("stream"); present {
		fields["stream"] = stream.Bytes()
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return generation.Lowered{}, err
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: in.MaxBodyBytes})
	if err != nil {
		return generation.Lowered{}, generation.ContinuationFailure("/request", "bounded_continuation_history")
	}
	out := generation.Lowered{Document: document, Evidence: "ordered tool history over the registered fixture dialect with an encrypted native dependency", Obligations: &oif.Obligations{
		Continuation:         fixtureContractVersion,
		Lifetime:             "durable",
		MaxContinuationBytes: 4 << 20,
		Actionability:        "encrypted_native_dependency_before_tool_bytes",
		Submission:           "client_submission_identity",
		Retry:                "recover_same_delivery_never_replay_unknown_work",
	}}
	out.Dispositions = append(out.Dispositions,
		oif.Disposition{Field: "/messages", Disposition: "mapped", Rule: "ordered_native_dependency_reconstruction", Evidence: "fixture-generation/ordered-tools-v1"},
		oif.Disposition{Field: "/tools", Disposition: "mapped", Rule: "exact_capability_schema", Evidence: "fixture-generation/ordered-tools-v1"},
		oif.Disposition{Field: "/result", Disposition: "guarded", Rule: "ordered_observations_and_durable_actionability", Evidence: "fixture-generation/ordered-tools-v1"})
	return out, nil
}

func fixtureInitialPrompts(value oif.Value) ([]byte, error) {
	if value.Kind() != oif.Array || len(value.Elements()) == 0 {
		return nil, generation.ContinuationFailure("/messages", "ordered_user_history")
	}
	out := []json.RawMessage{}
	prior := ""
	for _, message := range value.Elements() {
		role := generation.Text(generation.Member(message, "role"))
		if role == "system" || role == "developer" {
			return nil, generation.Incompatible("instruction_scope", "/messages", "instruction_scope", "The negotiated workflow has no qualified mapping for this instruction scope.")
		}
		if !generation.OnlyMembers(message, "role content") || (role != "user" && role != "assistant") || role == prior || generation.Member(message, "content").Kind() != oif.String {
			return nil, generation.ContinuationFailure("/messages", "unmodified_text_history_or_handle")
		}
		prior = role
		prompt, _ := json.Marshal(map[string]json.RawMessage{"role": generation.Member(message, "role").Bytes(), "content": generation.Member(message, "content").Bytes()})
		out = append(out, prompt)
	}
	if prior != "user" {
		return nil, generation.Incompatible("instruction_scope", "/messages", "assistant_prefill", "The negotiated workflow requires a final user turn.")
	}
	return json.Marshal(out)
}

func fixtureTools(value oif.Value) ([]byte, error) {
	if value.Kind() != oif.Array || len(value.Elements()) == 0 {
		return nil, generation.ContinuationFailure("/tools", "function_tools")
	}
	out := []map[string]json.RawMessage{}
	names := map[string]bool{}
	for _, tool := range value.Elements() {
		fn := generation.Member(tool, "function")
		name := generation.Text(generation.Member(fn, "name"))
		if !generation.OnlyMembers(tool, "type function") || generation.Text(generation.Member(tool, "type")) != "function" || !generation.OnlyMembers(fn, "name description parameters") || name == "" || names[name] || generation.Member(fn, "parameters").Kind() != oif.Object {
			return nil, generation.ContinuationFailure("/tools", "exact_function_schema")
		}
		names[name] = true
		mapped := map[string]json.RawMessage{"name": generation.Member(fn, "name").Bytes(), "schema": generation.Member(fn, "parameters").Bytes()}
		if description, present := fn.Lookup("description"); present {
			if description.Kind() != oif.String {
				return nil, generation.ContinuationFailure("/tools", "description_presence")
			}
			mapped["description"] = description.Bytes()
		}
		out = append(out, mapped)
	}
	return json.Marshal(out)
}

// reconstructFixtureTools rebuilds the next-turn native request from the
// committed dependency: unchanged controls, the corresponding history prefix,
// the retained assistant prompt and ordered tool results.
func reconstructFixtureTools(source generation.Source, prior *generation.Continuation) (map[string]json.RawMessage, error) {
	if prior.Version != fixtureContractVersion {
		return nil, generation.ContinuationFailure("/continuation", "historical_contract_version")
	}
	history, err := oif.ParseJSON(prior.Source, oif.Limits{})
	if err != nil {
		return nil, generation.ContinuationFailure("/continuation", "authenticated_history")
	}
	native, err := oif.ParseJSON(prior.NativeRequest, oif.Limits{})
	if err != nil {
		return nil, generation.ContinuationFailure("/continuation", "authenticated_history")
	}
	root := source.Document().Root()
	for _, old := range history.Root().Members() {
		if old.Name == "messages" || old.Name == "stream" || old.Name == "stream_options" {
			continue
		}
		if !generation.SameValue(old.Value, generation.Member(root, old.Name)) {
			return nil, generation.ContinuationFailure(generation.SafeField(old.Name), "unchanged_continuation_controls")
		}
	}
	for _, next := range root.Members() {
		if next.Name == "messages" || next.Name == "stream" || next.Name == "stream_options" {
			continue
		}
		if !generation.SameValue(next.Value, generation.Member(history.Root(), next.Name)) {
			return nil, generation.ContinuationFailure(generation.SafeField(next.Name), "unchanged_continuation_controls")
		}
	}
	messages := generation.Member(root, "messages").Elements()
	old := generation.Member(history.Root(), "messages").Elements()
	if len(messages) < len(old)+2 {
		return nil, generation.ContinuationFailure("/messages", "complete_corresponding_history")
	}
	for i := range old {
		if !generation.SameValue(messages[i], old[i]) {
			return nil, generation.ContinuationFailure("/messages", "unchanged_corresponding_history")
		}
	}
	assistant, err := oif.ParseJSON(prior.Assistant, oif.Limits{})
	if err != nil || !generation.SameValue(messages[len(old)], assistant.Root()) {
		return nil, generation.ContinuationFailure("/messages", "assistant_correspondence")
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(prior.Blocks, &blocks); err != nil {
		return nil, generation.ContinuationFailure("/continuation", "authenticated_history")
	}
	calls := generation.Member(assistant.Root(), "tool_calls").Elements()
	if len(calls) != len(blocks) {
		return nil, generation.ContinuationFailure("/messages", "ordered_tool_correspondence")
	}
	prompts := generation.Member(native.Root(), "prompts").Bytes()
	var priorPrompts []json.RawMessage
	if err := json.Unmarshal(prompts, &priorPrompts); err != nil || len(priorPrompts) == 0 {
		return nil, generation.ContinuationFailure("/continuation", "authenticated_history")
	}
	// The retained invocation list is the assistant turn's native
	// representation; it re-enters verbatim, never reconstructed from the
	// projected client form.
	invocations := json.RawMessage(prior.Blocks)
	assistantPrompt, _ := json.Marshal(map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`), "content": generation.Member(assistant.Root(), "content").Bytes(), "invocations": invocations})
	priorPrompts = append(priorPrompts, assistantPrompt)
	for i, message := range messages[len(old)+1:] {
		if !generation.OnlyMembers(message, "role tool_call_id content") || generation.Text(generation.Member(message, "role")) != "tool" || generation.Member(message, "content").Kind() != oif.String {
			return nil, generation.ContinuationFailure("/messages", "ordered_tool_results")
		}
		id := generation.Text(generation.Member(calls[i], "id"))
		if generation.Text(generation.Member(message, "tool_call_id")) != id {
			return nil, generation.ContinuationFailure("/messages", "ordered_tool_correspondence")
		}
		result, _ := json.Marshal(map[string]json.RawMessage{"role": json.RawMessage(`"tool"`), "call_id": generation.Member(message, "tool_call_id").Bytes(), "content": generation.Member(message, "content").Bytes()})
		priorPrompts = append(priorPrompts, result)
	}
	promptList, _ := json.Marshal(priorPrompts)
	fields := map[string]json.RawMessage{"prompts": promptList}
	for _, member := range native.Root().Members() {
		if member.Name == "prompts" || member.Name == "stream" || member.Name == "model" {
			continue
		}
		fields[member.Name] = member.Value.Bytes()
	}
	return fields, nil
}

// projectFixtureToolsUnary synthesizes the dialect's own events from an
// admitted unary result and delivers through the same projection the stream
// path uses — no parallel result grammar.
func projectFixtureToolsUnary(in generation.ProjectInput, result *generation.Native, handle string) (*generation.Continuation, generation.Delivery, error) {
	projection, err := projectFixtureToolEvents(in)
	if err != nil {
		return nil, generation.Delivery{}, err
	}
	tool := projection.(*fixtureProjection)
	root := result.Result.Source().Root()
	if err := fixtureResult(result.Result.Source()); err != nil {
		return nil, generation.Delivery{}, err
	}
	observe := func(name string, body any) error {
		raw, _ := json.Marshal(body)
		doc, e := oif.ParseJSON(raw, oif.Limits{MaxBytes: in.Limit})
		if e != nil {
			return e
		}
		event, e := oif.NewEvent(generation.Descriptor(fixtureGeneration, in.Source.Stream), doc, name, 0)
		if e != nil {
			return e
		}
		_, e = projection.Observe(event)
		return e
	}
	if content := generation.Text(generation.Member(generation.Member(root, "prompt"), "content")); content != "" {
		if err := observe("fixture.delta", map[string]any{"type": "fixture.delta", "text": content}); err != nil {
			return nil, generation.Delivery{}, err
		}
	}
	for _, call := range generation.Member(generation.Member(root, "prompt"), "invocations").Elements() {
		if err := observe("fixture.call", map[string]any{"type": "fixture.call", "call": json.RawMessage(call.Bytes())}); err != nil {
			return nil, generation.Delivery{}, err
		}
	}
	stop := map[string]json.RawMessage{"reason": generation.Member(generation.Member(root, "stop"), "reason").Bytes()}
	if matched, present := generation.Member(root, "stop").Lookup("matched"); present {
		stop["matched"] = matched.Bytes()
	}
	if err := observe("fixture.end", map[string]any{"type": "fixture.end", "stop": stop, "meter": json.RawMessage(generation.Member(root, "meter").Bytes())}); err != nil {
		return nil, generation.Delivery{}, err
	}
	tool.stream = in.Source.Stream
	return tool.Complete(result, handle)
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// --- fixture provider -------------------------------------------------------

type fixtureGenerationProvider struct {
	*httptest.Server
	mu    sync.Mutex
	calls [][]byte
}

func newFixtureGenerationProvider(t *testing.T) *fixtureGenerationProvider {
	t.Helper()
	f := &fixtureGenerationProvider{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/generate" || r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			http.Error(w, "fixture address or credential changed", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, bytes.Clone(body))
		f.mu.Unlock()
		var input struct {
			Stream  bool `json:"stream"`
			Prompts []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"prompts"`
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(body, &input); err != nil {
			http.Error(w, "malformed fixture request", http.StatusBadRequest)
			return
		}
		last := input.Prompts[len(input.Prompts)-1]
		switch {
		case last.Role == "tool":
			writeJSON(w, map[string]any{"id": "fx-final", "type": "fixture.completion", "model": vendorModel,
				"prompt": map[string]any{"role": "assistant", "content": "Both fixture tools completed."},
				"stop":   map[string]any{"reason": "end_turn", "matched": nil},
				"meter":  map[string]int{"input": 30, "output": 4}})
		case len(input.Tools) > 0 && input.Stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: fixture.delta\ndata: {\"type\":\"fixture.delta\",\"text\":\"before\"}\n\nevent: fixture.delta\ndata: {\"type\":\"fixture.delta\",\"text\":\"after\"}\n\nevent: fixture.call\ndata: {\"type\":\"fixture.call\",\"call\":{\"id\":\"call-weather\",\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\nevent: fixture.call\ndata: {\"type\":\"fixture.call\",\"call\":{\"id\":\"call-clock\",\"name\":\"clock\",\"arguments\":\"{\\\"zone\\\":\\\"CET\\\"}\"}}\n\nevent: fixture.end\ndata: {\"type\":\"fixture.end\",\"stop\":{\"reason\":\"tool_use\",\"matched\":null},\"meter\":{\"input\":12,\"output\":9}}\n\ndata: [END]\n\n")
		case input.Stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: fixture.delta\ndata: {\"type\":\"fixture.delta\",\"text\":\"fixture \"}\n\nevent: fixture.delta\ndata: {\"type\":\"fixture.delta\",\"text\":\"answer\"}\n\nevent: fixture.end\ndata: {\"type\":\"fixture.end\",\"stop\":{\"reason\":\"end_turn\",\"matched\":null},\"meter\":{\"input\":4,\"output\":3}}\n\ndata: [END]\n\n")
		default:
			writeJSON(w, map[string]any{"id": "fx-1", "type": "fixture.completion", "model": vendorModel,
				"prompt": map[string]any{"role": "assistant", "content": "fixture answer"},
				"stop":   map[string]any{"reason": "end_turn", "matched": nil},
				"meter":  map[string]int{"input": 4, "output": 3}})
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fixtureGenerationProvider) captured() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.calls))
	for i, call := range f.calls {
		out[i] = bytes.Clone(call)
	}
	return out
}

// --- the public demonstration ------------------------------------------------

// registeredGenerationExtension proves the generation half of the public
// extension story end to end through public configuration only: a compiled
// fixture dialect registers beside the built-ins, a provider profile publishes
// its label, public certification exercises its own probe/codec, and a strict
// route serves native unary, native streaming and the negotiated ordered
// tool/next-turn contract — with no provider or family switch in generic
// orchestration. It runs as a subtest of TestRegisteredExtensionsPublic so the
// extension suite's fixed -run selector picks it up.
func registeredGenerationExtension(t *testing.T) {
	registerFixtureGeneration(t)
	if err := connectors.RegisterOperationProfile(connectors.Profile{
		ID: "untrusted-gen-" + uuid.NewString()[:8], Revision: "1", Label: "Invalid fixture generation host",
		Kind: "azure_openai", Hosting: "azure-deployment", Authentication: []string{"api_key"}, Transport: "http",
		Operations: []string{"generation"}, OperationDialects: map[string]string{"generation": fixtureGenerationLabel},
	}); err == nil {
		t.Fatal("relative generation addressing escaped its trusted direct-compatible hosting")
	}
	profile := connectors.Profile{
		ID: "fixture-generation-" + uuid.NewString()[:8], Revision: "1", Label: "Fixture generation provider",
		Kind: "openai_compatible", Dialect: fixtureGenerationLabel, DialectRevision: "fixture-v1",
		Hosting: "direct-compatible", Authentication: []string{"api_key"}, Transport: "http",
		Operations: []string{"generation"}, OperationDialects: map[string]string{"generation": fixtureGenerationLabel},
		Documentation: "fixture-generation/1",
	}
	if err := connectors.RegisterOperationProfile(profile); err != nil {
		t.Fatal(err)
	}
	f := newFixtureGenerationProvider(t)
	h := newAccessHarness(t)
	owner := h.owner()
	provider := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name":          "Fixture generation " + uuid.NewString(),
		"configuration": map[string]any{"kind": "openai_compatible", "profile_id": profile.ID, "profile_revision": "1", "auth_mode": "api_key", "endpoint": f.URL + "/v1"},
		"model":         vendorModel, "credential": vendorSecret,
	}, idem(uuid.NewString()), 201)
	path := "/api/v3/providers/" + provider["id"].(string)
	modelID := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{
		map[string]any{"operation": "generation", "surface": "fixture", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "fixture", "mode": "streaming"},
		map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
	}}, etagHeader(provider), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(provider), 200)
	if certified["status"] != "certified" {
		t.Fatalf("registered generation dialect was not certified: %v", certified)
	}
	provider = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "fixture-generation-" + uuid.NewString()[:8]
	draft := fidelityDraft(slug, provider["id"])
	draft["fidelity"] = map[string]any{"mode": "strict"}
	working := h.want(owner, "POST", "/api/v3/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+working["id"].(string)+"/activate", nil, withMatch(working, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "fixture generation", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	h.refresh()
	secret := key["secret"].(string)
	before := len(f.captured())

	// Native unary through the registered dialect's own surface: the dialect
	// lifts the caller body, binds the published model and decodes its own
	// result grammar — no provider family participates.
	status, result, _ := h.gatewayRaw("POST", "/native/"+fixtureGenerationLabel+"/models/"+slug, secret, strings.NewReader(`{"model":"`+slug+`","prompts":[{"role":"user","content":"hi"}],"budget":16}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 || !bytes.Contains(result, []byte(`"fixture.completion"`)) || !bytes.Contains(result, []byte(`"model":"`+slug+`"`)) {
		t.Fatalf("fixture native unary failed: %d %s", status, result)
	}
	calls := f.captured()
	if len(calls) != before+1 || fidelity.Compare([]byte(`{"model":"`+vendorModel+`","prompts":[{"role":"user","content":"hi"}],"budget":16}`), calls[len(calls)-1]) != nil {
		t.Fatalf("fixture provider-bound request changed: %s", calls[len(calls)-1])
	}

	// Native streaming serves the dialect's own event grammar verbatim.
	status, stream, _ := h.gatewayRaw("POST", "/native/"+fixtureGenerationLabel+"/models/"+slug, secret, strings.NewReader(`{"model":"`+slug+`","stream":true,"prompts":[{"role":"user","content":"hi"}],"budget":16}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 || !bytes.Contains(stream, []byte("event: fixture.delta")) || !bytes.Contains(stream, []byte(`"text":"fixture "`)) || !bytes.Contains(stream, []byte(`"text":"answer"`)) || !bytes.Contains(stream, []byte("event: fixture.end")) || !bytes.Contains(stream, []byte("data: [END]")) {
		t.Fatalf("fixture native stream failed: %d %s", status, stream)
	}

	// The ordered tool contract negotiates through the chat surface: the
	// registered mapping owns lowering, event projection and the encrypted
	// next-turn dependency.
	tools := `"tools":[{"type":"function","function":{"name":"weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},{"type":"function","function":{"name":"clock","description":"Time","parameters":{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"]}}}]`
	source := `{"model":"` + slug + `","stream":true,"max_tokens":64,` + tools + `,"messages":[{"role":"user","content":"Weather and time in Paris?"}]}`
	headers := map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": fixtureContractVersion, "X-OLP-Submission-ID": resources.SubmissionID(time.Now(), uuid.New())}
	status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", secret, strings.NewReader(source), headers)
	if status != 200 {
		t.Fatalf("fixture tool turn: %d %s", status, raw)
	}
	var handle string
	var actions, terminal json.RawMessage
	var sawTool int
	var text strings.Builder
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		if ext, ok := chunk["olp"].(map[string]any); ok {
			if value, ok := ext["handle"].(string); ok {
				handle = value
			}
			if record, ok := ext["native_terminal"]; ok {
				terminal, _ = json.Marshal(record)
			}
			if claim, ok := ext["actions"]; ok {
				actions, _ = json.Marshal(claim)
			}
		}
		for _, choice := range chunk["choices"].([]any) {
			delta := choice.(map[string]any)["delta"].(map[string]any)
			if content, ok := delta["content"].(string); ok {
				text.WriteString(content)
			}
			if _, ok := delta["tool_calls"].([]any); ok {
				sawTool++
			}
		}
	}
	if sawTool != 2 || text.String() != "beforeafter" || !strings.HasPrefix(handle, "continuation_") {
		t.Fatalf("fixture projection incomplete: %s", raw)
	}
	if err := fidelity.Compare([]byte(`{"stop_reason":"tool_use","stop_sequence":null,"finish_reason":"tool_calls"}`), terminal); err != nil {
		t.Fatalf("fixture terminal record %s: %v", terminal, err)
	}
	if err := fidelity.Compare([]byte(`{"tool_calls":["call-weather","call-clock"]}`), actions); err != nil {
		t.Fatalf("fixture action claim %s: %v", actions, err)
	}

	// The next turn reconstructs the native request from the committed
	// dependency: the retained invocations re-enter verbatim and the ordered
	// tool results follow them.
	next := `{"model":"` + slug + `","max_tokens":64,` + tools + `,"messages":[{"role":"user","content":"Weather and time in Paris?"},{"role":"assistant","content":"beforeafter","tool_calls":[{"id":"call-weather","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call-clock","type":"function","function":{"name":"clock","arguments":"{\"zone\":\"CET\"}"}}]},{"role":"tool","tool_call_id":"call-weather","content":"sunny"},{"role":"tool","tool_call_id":"call-clock","content":"14:00"}]}`
	nextHeaders := map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": fixtureContractVersion, "X-OLP-Submission-ID": resources.SubmissionID(time.Now(), uuid.New()), "X-OLP-Continuation-Handle": handle}
	before = len(f.captured())
	status, final, _ := h.gatewayRaw("POST", "/v1/chat/completions", secret, strings.NewReader(next), nextHeaders)
	if status != 200 || !bytes.Contains(final, []byte("Both fixture tools completed.")) {
		t.Fatalf("fixture next turn: %d %s", status, final)
	}
	calls = f.captured()
	if len(calls) != before+1 {
		t.Fatalf("next turn did not reach the fixture provider: %d", len(calls))
	}
	var reconstructed struct {
		Prompts []struct {
			Role        string `json:"role"`
			Content     string `json:"content"`
			CallID      string `json:"call_id"`
			Invocations []struct {
				ID string `json:"id"`
			} `json:"invocations"`
		} `json:"prompts"`
	}
	if err := json.Unmarshal(calls[len(calls)-1], &reconstructed); err != nil {
		t.Fatal(err)
	}
	if len(reconstructed.Prompts) != 4 || reconstructed.Prompts[1].Role != "assistant" || len(reconstructed.Prompts[1].Invocations) != 2 || reconstructed.Prompts[1].Invocations[0].ID != "call-weather" || reconstructed.Prompts[2].Role != "tool" || reconstructed.Prompts[2].CallID != "call-weather" || reconstructed.Prompts[3].CallID != "call-clock" {
		raw, _ := json.Marshal(reconstructed)
		t.Fatalf("next-turn reconstruction wrong: %s", raw)
	}
	// A continuation handle cannot be reused to smuggle edited history: the
	// correspondence check rejects before provider dispatch.
	edited := strings.Replace(next, "Weather and time in Paris?", "edited history", 1)
	nextHeaders["X-OLP-Submission-ID"] = resources.SubmissionID(time.Now(), uuid.New())
	status, rejected, _ := h.gatewayRaw("POST", "/v1/chat/completions", secret, strings.NewReader(edited), nextHeaders)
	if status != 400 || !bytes.Contains(rejected, []byte("state_carrier")) || len(f.captured()) != before+1 {
		t.Fatalf("edited history admitted: %d %s", status, rejected)
	}
	// A chat request without the negotiated contract fails closed: no
	// stateless mapping was qualified, so no provider work can be claimed.
	before = len(f.captured())
	status, rejected, _ = h.gatewayRaw("POST", "/v1/chat/completions", secret, strings.NewReader(`{"model":"`+slug+`","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`), map[string]string{"Content-Type": "application/json"})
	if status != 400 || len(f.captured()) != before {
		t.Fatalf("unqualified chat request reached provider: %d %s", status, rejected)
	}
	// And a dialect the registry does not know can never reach dispatch.
	status, rejected, _ = h.gatewayRaw("POST", "/native/not-a-generation/models/"+slug, secret, strings.NewReader(source), map[string]string{"Content-Type": "application/json"})
	if status < 400 || status >= 500 {
		t.Fatalf("unregistered dialect admitted: %d %s", status, rejected)
	}
}
