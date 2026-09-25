package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Registered generation dialect identities. Wire-v1 describes the checked-in
// codec contract, matching the descriptors the legacy family codecs already
// produce on source envelopes.
var (
	DialectChat      = oif.Identity{ID: "openai-chat", Revision: "wire-v1"}
	DialectResponses = oif.Identity{ID: "openai-responses", Revision: "wire-v1"}
	DialectAnthropic = oif.Identity{ID: "anthropic-messages", Revision: "wire-v1"}
	DialectGemini    = oif.Identity{ID: "gemini-generate-content", Revision: "wire-v1"}
	DialectBedrock   = oif.Identity{ID: "bedrock-converse", Revision: "wire-v1"}
)

const evidenceNativeCodec = "codec-fixture/native-source-conservation/v1"

// DialectForFamily reports the registered generation dialect a legacy wire
// family encodes, or false when the family is not a generation codec.
func DialectForFamily(family openai.Family) (generation.Dialect, bool) {
	label := openai.Descriptor(family, false).Dialect
	for _, d := range GenerationDialects() {
		if d.Identity == label {
			return d, true
		}
	}
	return generation.Dialect{}, false
}

// GenerationDialects are the built-in compatibility dialects: the checked-in
// wire codecs registered behind the neutral operation-owned contracts. New
// dialects register here or through a registry without touching generic
// orchestration.
func GenerationDialects() []generation.Dialect {
	return []generation.Dialect{chatDialect, responsesDialect, anthropicDialect, geminiDialect, bedrockDialect}
}

func init() {
	operationregistry.RegisterGenerationDialects(GenerationDialects(), GenerationMappings())
}

func sourceOf(r *openai.Request) generation.Source {
	return generation.Source{Request: r.OIF(), Route: r.Route, Stream: r.Stream, IncludeUsage: r.IncludeUsage}
}

// SourceOf lifts a legacy parsed request into the neutral generation source
// contract. It is the compatibility edge between envelopes that predate
// registration and registered dialect planning.
func SourceOf(r *openai.Request) generation.Source { return sourceOf(r) }

// CompletionNative projects a legacy decoded completion into the neutral
// native result summary the operation contracts consume.
func CompletionNative(c *openai.Completion, route string) *generation.Native {
	return nativeOf(c, route)
}

// CompletionFromNative is the reverse compatibility view: a neutral native
// summary decoded by a registered dialect, carried in the legacy completion
// shape the executor still accumulates.
func CompletionFromNative(n *generation.Native) *openai.Completion {
	if n == nil {
		return nil
	}
	calls := make([]openai.ToolCall, 0, len(n.ToolCalls))
	for _, call := range n.ToolCalls {
		calls = append(calls, openai.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	var usage *openai.Usage
	if n.Usage != nil {
		usage = &openai.Usage{
			InputTokens: n.Usage.InputTokens, OutputTokens: n.Usage.OutputTokens, TotalTokens: n.Usage.TotalTokens,
			CachedInputTokens: n.Usage.CachedInputTokens, CacheWriteInputTokens: n.Usage.CacheWriteInputTokens,
			CacheWrite5MInputTokens: n.Usage.CacheWrite5MInputTokens, CacheWrite1HInputTokens: n.Usage.CacheWrite1HInputTokens,
			ReasoningTokens: n.Usage.ReasoningTokens, MediaUnits: n.Usage.MediaUnits,
		}
	}
	return &openai.Completion{Native: n.Result, Body: n.Body, UpstreamID: n.UpstreamID, ProviderModel: n.ProviderModel, FinishReason: n.FinishReason, OutputText: n.OutputText, Refusal: n.Refusal, ToolCalls: calls, Usage: usage}
}

func lift(family openai.Family) func(body []byte, route string, transportStream bool, maxBytes int) (generation.Source, error) {
	return func(body []byte, route string, transportStream bool, maxBytes int) (generation.Source, error) {
		if transportStream {
			if family == openai.FamilyGemini {
				family = openai.FamilyGeminiStream
			}
		}
		r, err := Parse(family, body, route)
		if err != nil {
			return generation.Source{}, err
		}
		return sourceOf(r), nil
	}
}

func liftBedrock(body []byte, route string, transportStream bool, maxBytes int) (generation.Source, error) {
	r, err := ParseBedrockRequest(body, route, transportStream)
	if err != nil {
		return generation.Source{}, err
	}
	return sourceOf(r), nil
}

func usageOf(u *openai.Usage) *generation.Usage {
	if u == nil {
		return nil
	}
	return &generation.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, TotalTokens: u.TotalTokens,
		CachedInputTokens: u.CachedInputTokens, CacheWriteInputTokens: u.CacheWriteInputTokens,
		CacheWrite5MInputTokens: u.CacheWrite5MInputTokens, CacheWrite1HInputTokens: u.CacheWrite1HInputTokens,
		ReasoningTokens: u.ReasoningTokens, MediaUnits: u.MediaUnits,
	}
}

func nativeOf(c *openai.Completion, route string) *generation.Native {
	if c == nil {
		return nil
	}
	calls := make([]generation.ToolCall, 0, len(c.ToolCalls))
	for _, call := range c.ToolCalls {
		calls = append(calls, generation.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return &generation.Native{
		Result: c.Native, Body: c.Body, Route: route, UpstreamID: c.UpstreamID, ProviderModel: c.ProviderModel,
		FinishReason: c.FinishReason, OutputText: c.OutputText, Refusal: c.Refusal, ToolCalls: calls, Usage: usageOf(c.Usage),
	}
}

// nativeDecode runs the same dialect codec the strict executor used: the wire
// grammar decodes its own result with the admitted effective request.
func nativeDecode(family openai.Family) func(in generation.DecodeInput) (*generation.Native, error) {
	return func(in generation.DecodeInput) (*generation.Native, error) {
		effective := openai.NewSourceEnvelope(family, in.Route, in.Source.Stream, in.Effective)
		c, err := decode(family, family, in.Body, in.Route, "", effective)
		if err != nil {
			return nil, err
		}
		return nativeOf(c, in.Route), nil
	}
}

func nativeStream(family openai.Family) func(in generation.StreamInput, emit func([]byte) error, observe func(oif.Event) error) (*generation.Native, error) {
	return func(in generation.StreamInput, emit func([]byte) error, observe func(oif.Event) error) (*generation.Native, error) {
		includeUsage := in.Source.IncludeUsage || in.IncludeUsage
		c, err := StreamWithEvents(family, family, in.Body, in.MaxEventBytes, in.Route, includeUsage, emit, observe)
		if err != nil {
			return nil, err
		}
		return nativeOf(c, in.Route), nil
	}
}

// streamConsistency rejects a lifted envelope whose declared delivery does not
// match its own stream member — the dialect's own admission invariant.
func streamConsistency(source generation.Source) error {
	stream := generation.Member(source.Document().Root(), "stream")
	if (stream.Raw() == "true") != source.Stream {
		return generation.Incompatible("target_capability", "/stream", "source_delivery_consistency", "The selected delivery mode disagrees with the native source.")
	}
	return nil
}

// identityChangesFor carries over the overlays the checked-in identity
// contract admits: the published model binding plus the chat transport
// accounting option.
func identityChangesFor(family openai.Family) func(source generation.Source, model string) ([]oif.Change, error) {
	return func(source generation.Source, model string) ([]oif.Change, error) {
		request := openai.NewSourceEnvelope(family, source.Route, source.Stream, source.Document())
		return identityChanges(request, model), nil
	}
}

func inspectOutput(family openai.Family) func([]byte, func(string) (string, bool)) ([]byte, error) {
	return func(body []byte, fn func(string) (string, bool)) ([]byte, error) {
		return InspectOutputText(family, body, TextSlot(fn))
	}
}

func parametersFor(family openai.Family) func(oif.Document) []string {
	return func(effective oif.Document) []string {
		return ParameterNames(openai.NewSourceEnvelope(family, "", false, effective))
	}
}

func meaningfulFrame(family openai.Family) func([]byte) bool {
	return func(frame []byte) bool { return MeaningfulFrame(family, frame) }
}

// chatAdmit rejects the one scope ambiguity the chat codec cannot reconcile:
// the two token-bound aliases with distinct declared scopes.
func chatAdmit(effective oif.Document) error {
	root := effective.Root()
	_, legacyCap := root.Lookup("max_tokens")
	_, completionCap := root.Lookup("max_completion_tokens")
	if legacyCap && completionCap {
		return generation.Incompatible("reasoning_budget", "/max_completion_tokens", "conflicting_budget_scopes", "The effective request combines token controls with distinct scopes; no precedence or equivalence is qualified.")
	}
	return nil
}

var chatDialect = generation.Dialect{
	Identity:  DialectChat,
	Operation: generation.Contract(),
	Surface:   "openai",
	Label:     "openai-chat",
	Evidence:  evidenceNativeCodec,
	Streaming: true,
	Address:   generation.Address{LegacyPath: "chat"},
	IdentityRules: []oif.IdentityRule{
		{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String},
		{Pointer: "/stream_options", Origin: oif.TransportOption, Kind: oif.Object, Validate: func(before, after oif.Value) bool {
			return (before.Kind() == oif.Absent || before.Kind() == oif.Null) && len(after.Members()) == 1 && generation.Member(after, "include_usage").Raw() == "true"
		}},
		{Pointer: "/stream_options/include_usage", Origin: oif.TransportOption, Kind: oif.Boolean, Validate: func(_, after oif.Value) bool { return after.Raw() == "true" }},
	},
	Lift:            lift(openai.FamilyChat),
	StreamField:     "stream",
	ValidateSource:  streamConsistency,
	IdentityChanges: identityChangesFor(openai.FamilyChat),
	DecodeNative:    nativeDecode(openai.FamilyChat),
	StreamNative:    nativeStream(openai.FamilyChat),
	ValidateEvent:   wireEventGuard(openai.FamilyChat),
	EventActionable: actionableEvent,
	TerminalControl: "[DONE]",
	Effects:         wireEffects(openai.FamilyChat),
	Assets:          wireAssets(openai.FamilyChat),
	InputCoverage:   wireInputCoverage(openai.FamilyChat),
	RequestCoverage: requestCoverage,
	ResultCoverage:  wireResultCoverage(openai.FamilyChat),
	Estimate:        wireEstimate(openai.FamilyChat),
	Parameters:      parametersFor(openai.FamilyChat),
	Admit:           chatAdmit,
	InspectOutput:   inspectOutput(openai.FamilyChat),
	MeaningfulFrame: meaningfulFrame(openai.FamilyChat),
}

var responsesDialect = generation.Dialect{
	Identity:  DialectResponses,
	Operation: generation.Contract(),
	Surface:   "openai",
	Label:     "openai-responses",
	Evidence:  evidenceNativeCodec,
	Streaming: true,
	Address:   generation.Address{LegacyPath: "responses"},
	IdentityRules: []oif.IdentityRule{
		{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String},
		{Pointer: "/previous_response_id", Origin: oif.ResourceBinding, Kind: oif.String},
	},
	Lift:            lift(openai.FamilyResponses),
	StreamField:     "stream",
	ValidateSource:  streamConsistency,
	IdentityChanges: identityChangesFor(openai.FamilyResponses),
	DecodeNative:    nativeDecode(openai.FamilyResponses),
	StreamNative:    nativeStream(openai.FamilyResponses),
	ValidateEvent:   wireEventGuard(openai.FamilyResponses),
	EventActionable: actionableEvent,
	TerminalControl: "[DONE]",
	// The strict published route identity replaces the provider's own model
	// name on a delivered native result.
	BindResultModel: func(document oif.Document, route string) ([]oif.Change, error) {
		if _, present := document.Lookup("/model"); !present {
			return nil, nil
		}
		model, _ := json.Marshal(route)
		return []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published response model"}}, nil
	},
	RedactEvent:     redactFailedFrame,
	Effects:         responsesEffects,
	Assets:          wireAssets(openai.FamilyResponses),
	InputCoverage:   wireInputCoverage(openai.FamilyResponses),
	RequestCoverage: requestCoverage,
	ResultCoverage:  wireResultCoverage(openai.FamilyResponses),
	Estimate:        wireEstimate(openai.FamilyResponses),
	Parameters:      parametersFor(openai.FamilyResponses),
	InspectOutput:   inspectOutput(openai.FamilyResponses),
	MeaningfulFrame: meaningfulFrame(openai.FamilyResponses),
	IncompleteEvent: "response.incomplete",
}

var anthropicDialect = generation.Dialect{
	Identity:  DialectAnthropic,
	Operation: generation.Contract(),
	Surface:   "anthropic",
	Label:     "anthropic-messages",
	Evidence:  evidenceNativeCodec,
	Streaming: true,
	Address:   generation.Address{LegacyPath: "anthropic"},
	IdentityRules: []oif.IdentityRule{
		{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String},
	},
	Lift:            lift(openai.FamilyAnthropic),
	StreamField:     "stream",
	ValidateSource:  streamConsistency,
	IdentityChanges: identityChangesFor(openai.FamilyAnthropic),
	DecodeNative:    nativeDecode(openai.FamilyAnthropic),
	StreamNative:    nativeStream(openai.FamilyAnthropic),
	ValidateEvent:   wireEventGuard(openai.FamilyAnthropic),
	EventActionable: actionableEvent,
	Effects:         anthropicEffects,
	Assets:          wireAssets(openai.FamilyAnthropic),
	InputCoverage:   wireInputCoverage(openai.FamilyAnthropic),
	RequestCoverage: requestCoverage,
	ResultCoverage:  wireResultCoverage(openai.FamilyAnthropic),
	Estimate:        wireEstimate(openai.FamilyAnthropic),
	Parameters:      parametersFor(openai.FamilyAnthropic),
	InspectOutput:   inspectOutput(openai.FamilyAnthropic),
	MeaningfulFrame: meaningfulFrame(openai.FamilyAnthropic),
}

var geminiDialect = generation.Dialect{
	Identity:      DialectGemini,
	Operation:     generation.Contract(),
	Surface:       "gemini",
	Label:         "gemini-generate-content",
	Evidence:      evidenceNativeCodec,
	Streaming:     true,
	Address:       generation.Address{LegacyPath: "gemini"},
	DeliveryKey:   "alt",
	DeliveryValue: "sse",
	Lift:          lift(openai.FamilyGemini),
	DecodeNative:  nativeDecode(openai.FamilyGemini),
	StreamNative:  nativeStream(openai.FamilyGemini),
	ValidateEvent: wireEventGuard(openai.FamilyGemini),
	// The stream family shares the codec; Gemini picks delivery through the
	// transport, not a body member, so nothing extra is validated.
	EventActionable: actionableEvent,
	Effects:         wireEffects(openai.FamilyGemini),
	Assets:          wireAssets(openai.FamilyGemini),
	InputCoverage:   wireInputCoverage(openai.FamilyGemini),
	RequestCoverage: requestCoverage,
	ResultCoverage:  wireResultCoverage(openai.FamilyGemini),
	Estimate:        wireEstimate(openai.FamilyGemini),
	Parameters:      parametersFor(openai.FamilyGemini),
	InspectOutput:   inspectOutput(openai.FamilyGemini),
	MeaningfulFrame: meaningfulFrame(openai.FamilyGemini),
	IdentityChanges: func(source generation.Source, model string) ([]oif.Change, error) {
		return nil, nil
	},
}

var bedrockDialect = generation.Dialect{
	Identity:        DialectBedrock,
	Operation:       generation.Contract(),
	Surface:         "bedrock",
	Label:           "bedrock-converse",
	Evidence:        evidenceNativeCodec,
	Streaming:       true,
	Address:         generation.Address{LegacyPath: "bedrock"},
	DeliveryKey:     "delivery",
	DeliveryValue:   "incremental",
	EventStream:     true,
	Lift:            liftBedrock,
	DecodeNative:    nativeDecode(openai.FamilyBedrock),
	StreamNative:    nativeStream(openai.FamilyBedrock),
	ValidateEvent:   wireEventGuard(openai.FamilyBedrock),
	EventActionable: actionableEvent,
	Effects:         bedrockEffects,
	Assets:          wireAssets(openai.FamilyBedrock),
	InputCoverage:   wireInputCoverage(openai.FamilyBedrock),
	RequestCoverage: requestCoverage,
	// Bedrock results carry opaque union members the output contract cannot
	// inspect; coverage stays nil so any output policy fails closed.
	Estimate:        wireEstimate(openai.FamilyBedrock),
	Parameters:      parametersFor(openai.FamilyBedrock),
	InspectOutput:   inspectOutput(openai.FamilyBedrock),
	MeaningfulFrame: meaningfulFrame(openai.FamilyBedrock),
	IdentityChanges: func(source generation.Source, model string) ([]oif.Change, error) {
		return nil, nil
	},
}

// actionableEvent recognizes native tool representations before the admitted
// projection writes them. It does not inspect arbitrary text for tool names or
// infer that a client will wait for complete argument fragments.
func actionableEvent(event oif.Event) bool {
	root := event.Source().Root()
	typeOf := func(value oif.Value) string {
		kind, _ := value.Lookup("type")
		text, _ := kind.Text()
		return text
	}
	for _, container := range []string{"content_block", "item", "delta"} {
		value, _ := root.Lookup(container)
		switch typeOf(value) {
		case "tool_use", "server_tool_use", "function_call", "input_json_delta":
			return true
		}
	}
	if choices, ok := root.Lookup("choices"); ok {
		for _, choice := range choices.Elements() {
			delta, _ := choice.Lookup("delta")
			if calls, _ := delta.Lookup("tool_calls"); len(calls.Elements()) > 0 {
				return true
			}
		}
	}
	if candidates, ok := root.Lookup("candidates"); ok {
		for _, candidate := range candidates.Elements() {
			content, _ := candidate.Lookup("content")
			parts, _ := content.Lookup("parts")
			for _, part := range parts.Elements() {
				if _, ok := part.Lookup("functionCall"); ok {
					return true
				}
			}
		}
	}
	if block, ok := root.Lookup("contentBlockStart"); ok {
		start, _ := block.Lookup("start")
		if _, ok := start.Lookup("toolUse"); ok {
			return true
		}
	}
	return event.Name() == "response.function_call_arguments.delta"
}

// wireEventGuard is the dialect's per-event admission grammar, moved out of
// the generic plan validator. A control frame is admitted only as the
// dialect's declared terminal marker.
func wireEventGuard(family openai.Family) func(event oif.Event) error {
	return func(event oif.Event) error {
		if control := event.Control(); control != "" {
			if (family == openai.FamilyChat || family == openai.FamilyResponses) && control == "[DONE]" {
				return nil
			}
			return generation.GuardFailure("/events", "control_grammar")
		}
		root := event.Source().Root()
		switch family {
		case openai.FamilyAnthropic:
			kind := generation.Text(generation.Member(root, "type"))
			if kind == "" || event.Name() != "" && event.Name() != kind {
				return generation.GuardFailure("/events/type", "event_identity")
			}
			switch kind {
			case "message_start":
				if generation.Member(root, "message").Kind() != oif.Object {
					return generation.GuardFailure("/events/message", "message_start")
				}
			case "content_block_start":
				if !generation.NonnegativeInteger(generation.Member(root, "index")) || generation.Member(root, "content_block").Kind() != oif.Object {
					return generation.GuardFailure("/events/content_block", "block_start")
				}
			case "content_block_delta":
				if !generation.NonnegativeInteger(generation.Member(root, "index")) || generation.Member(root, "delta").Kind() != oif.Object {
					return generation.GuardFailure("/events/delta", "block_delta")
				}
			case "content_block_stop":
				if !generation.NonnegativeInteger(generation.Member(root, "index")) {
					return generation.GuardFailure("/events/index", "block_stop")
				}
			case "message_delta":
				if d := generation.Member(root, "delta"); d.Kind() != oif.Object && d.Kind() != oif.Absent && d.Kind() != oif.Null {
					return generation.GuardFailure("/events/delta", "message_delta")
				}
			}
		case openai.FamilyChat:
			if choices, present := root.Lookup("choices"); present && choices.Kind() != oif.Array {
				return generation.GuardFailure("/events/choices", "candidate_grammar")
			}
		case openai.FamilyResponses:
			if generation.Text(generation.Member(root, "type")) == "" {
				return generation.GuardFailure("/events/type", "event_identity")
			}
		case openai.FamilyGemini:
			if candidates, present := root.Lookup("candidates"); present && candidates.Kind() != oif.Array {
				return generation.GuardFailure("/events/candidates", "candidate_grammar")
			}
		}
		return nil
	}
}

// redactFailedFrame scrubs a Responses failure frame that may echo configured
// credential material. Only the declared terminal error frames enter this
// path; other frames pass through unchanged.
func redactFailedFrame(frame []byte, credentials []string) ([]byte, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 || Redact(string(frame[:i]), credentials) != string(frame[:i]) {
		return nil, errors.New("failed response event has unsafe framing")
	}
	payload := bytes.TrimSpace(frame[i+7:])
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: len(frame) + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, errors.New("failed response event is malformed")
	}
	changes := []oif.Change{}
	var inspect func(value oif.Value, pointer string) error
	inspect = func(value oif.Value, pointer string) error {
		for _, field := range value.Members() {
			if err := inspect(field.Value, pointer+"/"+field.Name); err != nil {
				return err
			}
		}
		for index, element := range value.Elements() {
			if err := inspect(element, pointer+"/"+strconv.Itoa(index)); err != nil {
				return err
			}
		}
		if value.Kind() == oif.String {
			text, _ := value.Text()
			if safe := Redact(text, credentials); safe != text {
				encoded, _ := json.Marshal(safe)
				changes = append(changes, oif.Change{Pointer: pointer, Value: string(encoded), Origin: oif.ExplicitTransform, Reason: "provider error credential redaction"})
			}
		}
		return nil
	}
	if err := inspect(doc.Root(), ""); err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return frame, nil
	}
	redacted, err := oif.Apply(doc, changes)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, i+7+redacted.Len()+2)
	out = append(out, frame[:i+7]...)
	out = append(out, redacted.Bytes()...)
	out = append(out, '\n', '\n')
	return out, nil
}
