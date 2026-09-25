// Operation-owned generation contracts. Every type in this file is neutral:
// no provider SDK, request codec or wire-family type appears in the planning
// surface. A dialect is a trusted registration; a mapping qualifies one actual
// source/target pair including the client contract it can serve.
package generation

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/tyk-swe/olp/internal/oif"
)

// Source is one admitted caller generation request: the immutable native
// envelope plus its client-facing delivery context. Runtime binding supplies
// request values here; authorized resources remain caller-admitted overlays.
type Source struct {
	Request      oif.Request
	Route        string
	Stream       bool
	IncludeUsage bool
}

func (s Source) Descriptor() oif.Descriptor { return s.Request.Descriptor() }
func (s Source) Document() oif.Document     { return s.Request.Document() }

// Field returns a top-level source field verbatim, or nil when absent.
func (s Source) Field(name string) json.RawMessage {
	value, _ := s.Request.Document().Root().Lookup(name)
	return value.Bytes()
}

// Descriptor is the source envelope descriptor generation contracts share.
// The dialect identity names the registered wire contract; the legacy profile
// marker remains only so pre-registration envelopes compare equal.
func Descriptor(dialect oif.Identity, stream bool) oif.Descriptor {
	delivery := "unary"
	if stream {
		delivery = "incremental"
	}
	return oif.Descriptor{Operation: Contract(), Dialect: dialect, Profile: oif.Identity{ID: "legacy", Revision: "1"}, Execution: oif.Execution{Delivery: delivery, Lifetime: "request", Submission: "immediate", Effects: []string{"inference"}}}
}

// Usage is the token accounting observed on one generation. Optional provider
// categories stay optional so no codec invents them.
type Usage struct {
	InputTokens, OutputTokens, TotalTokens int64
	CachedInputTokens                      *int64
	CacheWriteInputTokens                  *int64
	CacheWrite5MInputTokens                *int64
	CacheWrite1HInputTokens                *int64
	ReasoningTokens                        *int64
	MediaUnits                             *string
}

// ToolCall is one declared client tool call observed in a native result.
type ToolCall struct{ ID, Name, Arguments string }

// Native is the operation-owned decoded native completion. Result retains the
// immutable provider document; the summary fields feed shared accounting and
// client delivery only.
type Native struct {
	Result        oif.Result
	Body          []byte
	Route         string
	UpstreamID    string
	ProviderModel string
	FinishReason  string
	OutputText    string
	Refusal       string
	ToolCalls     []ToolCall
	Usage         *Usage
}

// Continuation is a mapping's complete immutable next-turn dependency. It is
// sensitive: only the resource owner may persist it, encrypted. Receipts never
// include these values. Native blocks retain opaque signatures exactly.
type Continuation struct {
	Version       string          `json:"version"`
	Source        json.RawMessage `json:"source"`
	NativeRequest json.RawMessage `json:"native_request"`
	Blocks        json.RawMessage `json:"blocks"`
	Assistant     json.RawMessage `json:"assistant"`
}

// NativeTerminal is the bounded terminal observation the negotiated carrier
// retains for one accepted native result. It records the dialect reducer's
// admitted terminal facts verbatim beside the compatible client-protocol
// finish reason; it is never reconstructed from the narrowed Chat output.
type NativeTerminal struct {
	// Reason is the declared native stop_reason at the terminal boundary.
	Reason string `json:"stop_reason"`
	// Sequence carries the matched stop_sequence exactly as the native
	// contract defines it: a string when the terminal declared a match, an
	// explicit null when it declared none, and an absent member when no
	// admitted update carried the member at all.
	Sequence json.RawMessage `json:"stop_sequence,omitempty"`
	// Finish is the compatible client-protocol finish reason emitted beside
	// this record.
	Finish string `json:"finish_reason"`
}

// Valid reports whether a decoded record satisfies the admitted carrier
// grammar: a qualified native reason, the compatible finish it projects to,
// and the matched-sequence correspondence the native contract defines. A
// stored record that fails this shape was never committed by this contract.
func (t *NativeTerminal) Valid() bool {
	if t == nil {
		return false
	}
	switch t.Reason {
	case "end_turn", "tool_use", "max_tokens", "stop_sequence":
	default:
		return false
	}
	switch t.Finish {
	case "stop", "tool_calls", "length":
	default:
		return false
	}
	matched := len(t.Sequence) != 0 && string(t.Sequence) != "null"
	if matched {
		var sequence string
		if json.Unmarshal(t.Sequence, &sequence) != nil || sequence == "" {
			return false
		}
	}
	return (t.Reason == "stop_sequence") == matched
}

// ContinuationActions is the committed explicit actionability claim of one
// ready delivery, independent of the ready handle itself. The handle means the
// dependency and its recorded delivery are recoverable; this member states
// which next actions the admitted native outcome actually permits. A tool call
// identity is listed only when the native terminal yielded tool use, the call
// carried complete validated arguments, the ordered dependencies were
// retained, and the whole-turn durability barrier committed before any
// actionable byte was published. A partial or non-tool outcome is recoverable
// while claiming no tool action; a delivery committed before this member
// existed decodes with a nil Actions and reads as explicitly unavailable.
type ContinuationActions struct {
	// ToolCalls lists the ordered call identities the qualified client may
	// answer with tool results. An empty member is an explicit "no tool
	// action" claim; it never means "run these tools" by implication.
	ToolCalls []string `json:"tool_calls"`
}

// UnmarshalJSON enforces the committed carrier grammar at the member level:
// exactly the tool_calls member, so a stored claim carrying anything else —
// or omitting it — was never committed by this contract and fails decode.
func (a *ContinuationActions) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	if len(members) != 1 {
		return errors.New("continuation actions carry members outside the carrier")
	}
	var calls []string
	if err := json.Unmarshal(members["tool_calls"], &calls); err != nil {
		return err
	}
	a.ToolCalls = calls
	return nil
}

// Valid reports whether a decoded claim satisfies the committed carrier
// grammar: an explicit tool_calls member of bounded nonempty identities. A
// stored claim that fails this shape was never committed by this contract.
func (a *ContinuationActions) Valid() bool {
	if a == nil || a.ToolCalls == nil || len(a.ToolCalls) > 1024 {
		return false
	}
	for _, id := range a.ToolCalls {
		if id == "" {
			return false
		}
	}
	return true
}

// Delivery is already projected client data. The gateway commits it with the
// complete native dependency before publishing any queued actionable frame.
// Replaying this value never invokes a provider or claims new token usage.
// Terminal and Actions are additive on the carrier: deliveries committed
// before those records existed decode with nil members and read as explicitly
// unavailable rather than upgrading to a tool yield on replay.
type Delivery struct {
	Stream   bool                 `json:"stream"`
	Frames   []json.RawMessage    `json:"frames,omitempty"`
	Body     json.RawMessage      `json:"body,omitempty"`
	Terminal *NativeTerminal      `json:"native_terminal,omitempty"`
	Actions  *ContinuationActions `json:"actions,omitempty"`
}

// Accounting reports a projection's separated resource counters so callers
// and tests can distinguish retained dependency state, projected replay
// delivery, live transient memory and aggregate admitted event work.
type Accounting struct {
	RetainedDependencyBytes int
	ProjectedDeliveryBytes  int
	TransientEventBytes     int
	AdmittedEventWorkBytes  int
}

// Projection is a mapping-owned ordered client projection: it consumes
// reducer-admitted native events and produces projected client frames, then
// completes into the reconstructable next-turn dependency. Unary-only
// mappings do not implement it. Every projection accounts its admitted-event
// work, transient memory, retained dependency and projected delivery as
// separately bounded resources.
type Projection interface {
	// Observe consumes one reducer-admitted native event and returns the
	// projected client frames for it; nil when the event produced none.
	Observe(event oif.Event) ([][]byte, error)
	// Complete completes the projection over the decoded native summary,
	// returning the next-turn dependency plus the recorded client delivery.
	Complete(native *Native, handle string) (*Continuation, Delivery, error)
	// Accounting reports the projection's separated resource counters.
	Accounting() Accounting
}

// Address publishes how a registered generation dialect is invoked.
// RelativePath is appended to the connector endpoint and requires a
// direct-compatible hosting contract. LegacyPath names one checked-in hosting
// path table entry; it exists only so dialects that predate registration keep
// their historical addresses, and is never a semantic intermediate form.
type Address struct{ LegacyPath, RelativePath string }

// Estimate is the dialect-owned conservative reservation shape: the input
// tokens a request may consume, the largest output bound it allowed, and the
// candidates asked for.
type Estimate struct {
	Input      int64
	Output     *int64
	Candidates int64
}

// StateInput carries the caller-binding values a declared-effects contract may
// rely on. A dialect never reads headers, keys or resources directly.
// AllowHostedTools is the caller's provider-hosted tool authorization and
// HostedTools names the bound profile's qualified hosted tool families; the
// dialect admits hosted effects only inside that intersection.
type StateInput struct {
	AllowProviderState bool
	AllowHostedTools   bool
	HostedTools        []string
	RetainedResponses  bool
	RequiredServing    *oif.ServingIdentity
}

// DecodeInput is the admitted context for decoding one native unary result.
type DecodeInput struct {
	Source    Source
	Effective oif.Document // the admitted effective native request document
	Body      []byte
	Route     string
	MaxBytes  int
}

// StreamInput is the admitted context for consuming one native event stream.
// IncludeUsage requests the dialect's terminal usage accounting independent
// of the client-requested usage flag.
type StreamInput struct {
	Source        Source
	Body          io.Reader // transport-adjusted event stream
	MaxEventBytes int
	Route         string
	IncludeUsage  bool
}

// LowerInput carries the binding values a qualified lowering consumes.
type LowerInput struct {
	Source              Source
	Model               string                     // published serving model
	Defaults            map[string]json.RawMessage // declared operation defaults
	Hosting             string                     // resolved hosting contract
	MaxBodyBytes        int
	Continuation        *Continuation // admitted prior next-turn dependency
	DurableContinuation bool          // caller authorized durable handles
	AllowProviderState  bool
}

// Lowered is a mapping's qualified lowering result: the target native document
// plus the receipt evidence and obligation overrides it declares.
type Lowered struct {
	Document     oif.Document
	Dispositions []oif.Disposition
	Obligations  *oif.Obligations // replaces default obligations when set
	Evidence     string
}

// ProjectInput carries the admitted plan facts a client projection needs.
// ValidateEvent is a plan-provided guard so the projection never reaches
// around the interaction contract.
type ProjectInput struct {
	Source        Source
	Prepared      oif.Prepared
	Effective     oif.Document
	Limit         int
	ValidateEvent func(oif.Event) error
}

// Dialect is the operation-owned generation contract: a trusted registration
// covering source lifting, identity preparation, native result and event
// grammar, and declared effects and limits for one dialect. Optional hooks are
// absent where a dialect does not admit that capability — a unary-only dialect
// never carries streaming methods, and coverage hooks stay nil where the
// dialect has no policy inspection contract so policy use fails closed.
type Dialect struct {
	Identity      oif.Identity
	Surface       string
	Label         string
	Documentation string
	Evidence      string
	Streaming     bool
	Address       Address
	IdentityRules []oif.IdentityRule

	// Lift validates one caller body and lifts it to the immutable source
	// envelope for this dialect. transportStream is the transport-level
	// delivery selection for dialects without an in-body flag. It owns the
	// complete envelope admission.
	Lift func(body []byte, route string, transportStream bool, maxBytes int) (Source, error)
	// StreamField names the in-body delivery flag this grammar carries (for
	// example "stream"); empty when delivery is transport-selected.
	StreamField string
	// TransportOverlay names an optional body field admitted as a transport
	// accounting option rather than model semantics (for example
	// "stream_options").
	TransportOverlay string
	// DeliveryKey/DeliveryValue name a semantic-query selector that promotes a
	// unary caller request to incremental delivery for dialects whose delivery
	// is transport-selected (for example alt=sse).
	DeliveryKey, DeliveryValue string
	// Operation is always Contract(); it is stored so registry validation and
	// capability checks stay operation-generic.
	Operation oif.Identity
	// IdentityChanges are the admitted model/transport identity overlays
	// applied to the source document for a native plan.
	IdentityChanges func(source Source, model string) ([]oif.Change, error)

	// DecodeNative runs the dialect's native result grammar for native client
	// delivery. StreamNative consumes the dialect's native event grammar,
	// emitting client frames and observing reducer-admitted events; the
	// dialect's authoritative reducer owns ordering. Both are nil for a
	// dialect that does not serve that delivery mode.
	DecodeNative func(in DecodeInput) (*Native, error)
	StreamNative func(in StreamInput, emit func(frame []byte) error, observe func(oif.Event) error) (*Native, error)
	// ValidateSource performs dialect admission beyond lifting: it checks the
	// admitted source's own delivery/identity consistency before planning.
	ValidateSource func(source Source) error
	// ValidateEvent is the dialect-owned per-event admission guard invoked
	// before any observation or projection. hosted carries the provider-hosted
	// tool families the bound plan admitted so the grammar can bound
	// provider-emitted observations against them.
	ValidateEvent func(event oif.Event, hosted []string) error
	// ValidateResult, when set, bounds a decoded native result document against
	// the effects the plan admitted — for example provider-hosted tool
	// observations — beyond the codec's own grammar. Nil admits codec grammar
	// only.
	ValidateResult func(result oif.Document, hosted []string) error
	// EventActionable marks an admitted native event as carrying client-visible
	// tool or semantic substance; nil events are never actionable.
	EventActionable func(event oif.Event) bool
	// TerminalControl names the stream's control-frame terminal marker (for
	// example "[DONE]"); empty when the grammar has no control frame.
	TerminalControl string
	// EventStream reports a binary event transport (AWS event-stream) instead
	// of SSE so transport admission and content negotiation match the dialect.
	EventStream bool
	// BindResultModel, when set, rewrites the published route identity into a
	// decoded native result for native client delivery.
	BindResultModel func(document oif.Document, route string) ([]oif.Change, error)
	// RedactEvent, when set, scrubs upstream frames that can carry credential
	// material before they reach the client.
	RedactEvent func(frame []byte, secrets []string) ([]byte, error)
	// IncompleteEvent names the native event type that marks a stream the
	// provider ended incomplete; empty when the grammar has none.
	IncompleteEvent string

	// Declared effects and limits.
	//
	// Assets rejects provider asset references outside authorized positions.
	// Effects evaluates provider-state and tool declarations in the effective
	// document, amends the obligation record and reports the provider-hosted
	// tool families it admitted; downstream guards bound provider emissions
	// against that admission. InputCoverage, RequestCoverage and ResultCoverage
	// are the policy inspection contracts — hosted is the same admission — and
	// a nil coverage hook fails closed. Estimate and Parameters are the
	// reservation and routing shapes. Admit performs final admission of the
	// identity-prepared effective document.
	Assets          func(effective oif.Document) error
	Effects         func(effective oif.Document, in StateInput, obligations *oif.Obligations) (hosted []string, err error)
	InputCoverage   func(effective oif.Document) error
	RequestCoverage func(effective oif.Document, hosted []string) error
	ResultCoverage  func(result oif.Document, hosted []string) error
	Estimate        func(effective oif.Document) Estimate
	Parameters      func(effective oif.Document) []string
	Admit           func(effective oif.Document) error
	// InspectOutput rewrites caller-visible text in a projected or native
	// result body for output content policy; nil means the dialect exposes no
	// inspectable output text and policy enforcement fails closed.
	InspectOutput func(body []byte, fn func(text string) (next string, stop bool)) ([]byte, error)
	// MeaningfulFrame reports whether a projected client frame carries
	// observable content (used for first-output accounting only).
	MeaningfulFrame func(frame []byte) bool
}

// Mapping qualifies one actual source/target generation dialect pair,
// including the advertised client contract it serves. ClientContract is empty
// for the stateless mapping; a registered contract version names a negotiated
// continuation such as an ordered tool/next-turn dependency.
type Mapping struct {
	Source, Target oif.Identity
	ClientContract string
	Evidence       string
	// Lower is the qualified lowering from the admitted source document to the
	// target native document, including continuation reconstruction when the
	// contract carries a prior next-turn dependency.
	Lower func(in LowerInput) (Lowered, error)
	// ValidateResult optionally tightens the native result admission beyond
	// the dialect's own grammar for this mapping's client contract.
	ValidateResult func(document oif.Document) error
	// ProjectResult projects a decoded native result to the client contract.
	// handle is the resource-owned continuation identity this delivery may
	// advertise; empty for deliveries without a continuation authority.
	// Continuation contracts also return the next-turn dependency; stateless
	// mappings return only delivery bytes.
	ProjectResult func(in ProjectInput, result *Native, handle string) (*Continuation, Delivery, error)
	// ProjectEvents builds the streaming client projection for continuation
	// contracts; nil where the mapping has no streaming contract.
	ProjectEvents func(in ProjectInput) (Projection, error)
}
