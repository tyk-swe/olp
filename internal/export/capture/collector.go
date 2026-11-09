package capture

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type Mode string

const (
	Unary     Mode = "unary"
	Streaming Mode = "streaming"
)

func ModeOf(streaming bool) Mode {
	if streaming {
		return Streaming
	}
	return Unary
}

type Identity struct {
	Route         string
	ProjectID     string
	KeyID         string
	EndUserDigest string
	TraceID       string
	SpanID        string
	Outcome       string
	Mode          Mode
}

type state uint8

const (
	collecting state = iota
	spoiled
	finished
	released
)

var (
	errClosed   = errors.New("capture already finished")
	errSpoiled  = errors.New("capture content unusable")
	errEmpty    = errors.New("capture has no content")
	errTooLarge = errors.New("capture record exceeds the policy size")
)

type Collector struct {
	manager   *Manager
	policyID  string
	etag      string
	requestID string
	maxBytes  int
	include   []Include
	redactor  *guardrails.Engine

	mu        sync.Mutex
	state     state
	input     map[string]json.RawMessage
	output    strings.Builder
	tools     []json.RawMessage
	toolBytes int
	stream    *protocols.CaptureStream
}

func (c *Collector) includes(field Include) bool {
	return slices.Contains(c.include, field)
}

func (c *Collector) capturesResponse() bool {
	return c.includes(Output) || c.includes(ToolCalls)
}

func (c *Collector) CaptureDocument(document map[string]json.RawMessage) {
	if !c.includes(Input) {
		return
	}
	if slices.ContainsFunc(inputFields, func(field string) bool { return len(document[field]) > fieldMax }) {
		c.spoil()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == collecting {
		c.input = inputOf(document)
	}
}

func (c *Collector) CaptureResponse(family wire.Format, body []byte) {
	if !c.capturesResponse() {
		return
	}
	output, calls, err := protocols.CaptureResponse(family, body, c.includes(Output), c.includes(ToolCalls))
	if err != nil {
		c.spoil()
		return
	}
	c.captureOutput(output)
	for _, call := range calls {
		c.captureToolCall(call)
	}
}

func (c *Collector) CaptureStreamFrame(family wire.Format, frame []byte) {
	if !c.capturesResponse() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != collecting {
		return
	}
	if c.stream == nil {
		c.stream = protocols.NewCaptureStream(family, fieldMax, c.includes(Output), c.includes(ToolCalls))
	}
	if c.stream.Push(frame) != nil {
		c.state, c.stream = spoiled, nil
	}
}

func (c *Collector) captureOutput(text string) {
	if !c.includes(Output) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != collecting {
		return
	}
	if c.output.Len()+len(text) > fieldMax {
		c.state = spoiled
		return
	}
	c.output.WriteString(text)
}

func (c *Collector) captureToolCall(raw json.RawMessage) {
	if !c.includes(ToolCalls) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != collecting {
		return
	}
	if c.toolBytes+len(raw) > fieldMax {
		c.state = spoiled
		return
	}
	c.tools = append(c.tools, slices.Clone(raw))
	c.toolBytes += len(raw)
}

func (c *Collector) spoil() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == collecting {
		c.state = spoiled
	}
}

func (c *Collector) Finish(ctx context.Context, identity Identity, at time.Time) bool {
	defer c.release()
	record, err := c.record(ctx, identity, at)
	switch {
	case errors.Is(err, errClosed):
		return false
	case errors.Is(err, errEmpty):
		c.manager.dropped.Add(1)
		return false
	case err != nil:
		c.manager.drop(c.redactor != nil)
		return false
	}
	return c.manager.enqueue(queued{policyID: c.policyID, etag: c.etag, requestID: c.requestID, projectID: identity.ProjectID, at: at, record: record})
}

func (c *Collector) record(ctx context.Context, identity Identity, at time.Time) ([]byte, error) {
	sealed, err := c.seal(identity)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	collected, err := c.redacted(ctx, sealed)
	if err != nil {
		return nil, err
	}
	body := payload{
		RequestID: c.requestID, Route: identity.Route, ProjectID: identity.ProjectID, KeyID: identity.KeyID,
		EndUserDigest: identity.EndUserDigest, Mode: identity.Mode,
		TraceID: traceHex(identity.TraceID), SpanID: traceHex(identity.SpanID),
		Input: included(c.includes(Input), collected.input), Output: included(c.includes(Output), collected.output),
		ToolCalls: included(c.includes(ToolCalls), collected.tools),
	}
	if len(body.Input) == 0 && body.Output == "" && len(body.ToolCalls) == 0 {
		return nil, errEmpty
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	record, err := json.Marshal(envelope{
		Data: data, EventID: access.NewID(), OccurredAt: at.UTC().Format(time.RFC3339Nano), Outcome: identity.Outcome,
		ProjectID: identity.ProjectID, Route: identity.Route, SourceID: c.requestID, Stream: "captures", Version: 1,
	})
	if err != nil {
		return nil, err
	}
	if len(record) > c.maxBytes {
		return nil, errTooLarge
	}
	return record, ctx.Err()
}

func (c *Collector) seal(identity Identity) (content, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case finished, released:
		return content{}, errClosed
	case spoiled:
		c.state = finished
		return content{}, errSpoiled
	}
	c.state = finished
	// The serving decoder can reject framing or usage after the last captured
	// content, so a failed stream spoils even input-only capture.
	if identity.Mode == Streaming && identity.Outcome != "success" {
		return content{}, errSpoiled
	}
	output, tools, err := c.response(identity.Mode)
	if err != nil || c.redactor != nil && c.includes(Input) && len(c.input) == 0 {
		return content{}, errSpoiled
	}
	return content{input: c.input, output: output, tools: tools}, nil
}

func (c *Collector) response(mode Mode) (string, []json.RawMessage, error) {
	if mode != Streaming || !c.capturesResponse() {
		return c.output.String(), c.tools, nil
	}
	if c.stream == nil {
		return "", nil, errSpoiled
	}
	return c.stream.Finish()
}

func (c *Collector) redacted(ctx context.Context, collected content) (content, error) {
	if c.redactor == nil {
		return collected, nil
	}
	return redact(ctx, c.redactor, collected)
}

func (c *Collector) release() {
	c.mu.Lock()
	previous := c.state
	c.state = released
	c.input, c.stream, c.tools = nil, nil, nil
	c.output.Reset()
	c.mu.Unlock()
	if previous != released {
		c.manager.collectors.Add(-1)
	}
}

type payload struct {
	EndUserDigest string                     `json:"end_user_digest,omitempty"`
	Input         map[string]json.RawMessage `json:"input,omitempty"`
	KeyID         string                     `json:"key_id,omitempty"`
	Mode          Mode                       `json:"mode,omitempty"`
	Output        string                     `json:"output,omitempty"`
	ProjectID     string                     `json:"project_id,omitempty"`
	RequestID     string                     `json:"request_id"`
	Route         string                     `json:"route,omitempty"`
	SpanID        string                     `json:"span_id,omitempty"`
	ToolCalls     []json.RawMessage          `json:"tool_calls,omitempty"`
	TraceID       string                     `json:"trace_id,omitempty"`
}

type envelope struct {
	Data       json.RawMessage `json:"data"`
	EventID    string          `json:"event_id"`
	OccurredAt string          `json:"occurred_at"`
	Outcome    string          `json:"outcome"`
	ProjectID  string          `json:"project_id"`
	Route      string          `json:"route"`
	SourceID   string          `json:"source_id"`
	Stream     string          `json:"stream"`
	Version    int             `json:"version"`
}

func included[T any](include bool, value T) T {
	if include {
		return value
	}
	var zero T
	return zero
}

func traceHex(id string) string {
	if len(id) != 16 && len(id) != 32 || strings.Trim(id, "0") == "" {
		return ""
	}
	return id
}
