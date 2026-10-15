package wire

import "github.com/tyk-swe/olp/internal/oif"

// Usage is the token accounting observed on one generation.
type Usage struct {
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	TotalTokens       int64  `json:"total_tokens"`
	CachedInputTokens *int64 `json:"cached_input_tokens,omitempty"`

	CacheWriteInputTokens   *int64 `json:"cache_write_input_tokens,omitempty"`
	CacheWrite5MInputTokens *int64 `json:"cache_write_5m_input_tokens,omitempty"`
	CacheWrite1HInputTokens *int64 `json:"cache_write_1h_input_tokens,omitempty"`
	AudioInputTokens        *int64 `json:"audio_input_tokens,omitempty"`
	CachedAudioInputTokens  *int64 `json:"cached_audio_input_tokens,omitempty"`
	AudioOutputTokens       *int64 `json:"audio_output_tokens,omitempty"`
	ImageInputTokens        *int64 `json:"image_input_tokens,omitempty"`
	CachedImageInputTokens  *int64 `json:"cached_image_input_tokens,omitempty"`
	ImageOutputTokens       *int64 `json:"image_output_tokens,omitempty"`
	// ToolCalls counts metered server-side tool invocations by tool name.
	ToolCalls       map[string]int64 `json:"tool_calls,omitempty"`
	ReasoningTokens *int64           `json:"reasoning_tokens,omitempty"`
	// MediaUnits is the provider-metered media quantity — images returned or
	// seconds processed — carried as decimal text so it survives the usage
	// event wire shape unchanged.
	MediaUnits *string `json:"media_units,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Completion summarises an observed generation, which may be partial on stream
// failure. Body is the rewritten unary document and is nil for streams.
type Completion struct {
	// Native retains the immutable upstream result before projection. Streams
	// use incremental OIF events instead of accumulating a result document.
	Native        oif.Result `json:"-"`
	Body          []byte
	UpstreamID    string
	ProviderModel string
	FinishReason  string
	OutputText    string
	Refusal       string
	ToolCalls     []ToolCall
	Usage         *Usage
	// UsageComplete reports authoritative terminal generation counters. A partial
	// stream may retain Usage without proving that no further work was billed.
	UsageComplete bool           `json:"-"`
	Terminal      bool           `json:"-"`
	Failure       *UpstreamError `json:"-"`
}

// NativeResponseOutcome reports a validated terminal Responses event,
// independently of usage disclosure and local projection or delivery.
func (c *Completion) NativeResponseOutcome() (terminal bool, failure *UpstreamError) {
	if c == nil {
		return false, nil
	}
	return c.Terminal, c.Failure
}

// ProtocolError reports an upstream document or stream that violates the
// native protocol. Truncated marks streams that ended before their terminal event.
type ProtocolError struct {
	Detail    string
	Truncated bool
}

func (e *ProtocolError) Error() string { return "upstream protocol violation: " + e.Detail }

// UpstreamError is an error object the upstream returned in place of a result.
type UpstreamError struct {
	Type    string
	Code    string
	Message string
}

func (e *UpstreamError) Error() string { return "upstream error: " + e.Message }

// Redact passes every provider-supplied field through redact, because a
// provider may echo request credentials in any of them. A nil error stays nil.
func (e *UpstreamError) Redact(redact func(string) string) *UpstreamError {
	if e != nil {
		e.Type, e.Code, e.Message = redact(e.Type), redact(e.Code), redact(e.Message)
	}
	return e
}

// Emit delivers one encoded stream frame.
type Emit func(frame []byte) error
