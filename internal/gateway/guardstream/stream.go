package guardstream

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Config struct {
	Engine        *guardrails.Engine
	Masks         *guardrails.Masking
	Input         []guardrails.Segment
	RequestID     string
	Route         string
	InternalModel bool
	Family        wire.Format
	Window        int
	Strict        bool
	ToolContext   context.Context
	Record        func([]guardrails.Evidence)
}

type Stream struct {
	config       Config
	calls        map[string]*toolCall
	pending      [][]byte
	pendingBytes int
	stream       *guardrails.Stream
}

func New(config Config) (*Stream, error) {
	s, err := guardrails.NewStream(config.Engine, config.request(), config.Window)
	if err != nil {
		return nil, err
	}
	if config.Engine.HasAction(guardrails.Input, guardrails.Mask) {
		s.Masking(config.Masks)
	}
	if !config.Strict {
		s.SegmentRewriter(func(frame []byte, texts map[string]string) ([]byte, error) {
			return rewriteFrame(config.Family, frame, texts)
		})
	}
	return &Stream{stream: s, config: config, calls: map[string]*toolCall{}}, nil
}

func (c Config) request() guardrails.Evaluation {
	return guardrails.Evaluation{RequestID: c.RequestID, Route: c.Route, InternalModel: c.InternalModel}
}

func (c Config) evaluation(segments []guardrails.Segment) guardrails.Evaluation {
	return guardrails.Evaluation{RequestID: c.RequestID, Route: c.Route, InternalModel: c.InternalModel, Phase: guardrails.OutputStream, Segments: segments}
}

func (g *Stream) Decisions() []guardrails.Evidence { return g.stream.Decisions() }

func (g *Stream) Push(ctx context.Context, frame []byte) ([][]byte, error) {
	touched, err := g.collectTools(frame)
	if err != nil {
		return nil, err
	}
	if touched || len(g.pending) > 0 {
		g.pendingBytes += len(frame)
		if g.pendingBytes > g.config.Window {
			return nil, guardrails.ErrSurface
		}
		g.pending = append(g.pending, bytes.Clone(frame))
		complete := len(g.calls) > 0
		for _, call := range g.calls {
			complete = complete && call.done
		}
		if !complete {
			return nil, nil
		}
		segments := []guardrails.Segment{}
		for id, call := range g.calls {
			if call.name == "" || !json.Valid([]byte(call.arguments)) {
				return nil, guardrails.ErrSurface
			}
			var data any
			if err := decodeJSON([]byte(call.arguments), &data); err != nil {
				return nil, guardrails.ErrSurface
			}
			canonical, _ := json.Marshal(data)
			arguments := g.config.Masks.Restore(string(canonical))
			segments = append(segments, guardrails.Segment{ID: "tool_" + id, Kind: guardrails.ToolCall, ToolName: call.name, Text: arguments, Role: guardrails.Assistant})
		}
		result, err := g.config.Engine.Evaluate(g.config.ToolContext, g.config.evaluation(slices.Concat(segments, guardrails.InputProvenance(g.config.Input))), g.config.Masks)
		g.config.Record(result.Decisions)
		if err != nil {
			return nil, err
		}
		restoreChanged := false
		for _, segment := range segments {
			if strings.HasPrefix(segment.ID, "tool_") {
				original := g.calls[strings.TrimPrefix(segment.ID, "tool_")].arguments
				restoreChanged = restoreChanged || g.config.Masks.Restore(original) != original
			}
		}
		if result.Changed || restoreChanged {
			if g.config.Strict {
				return nil, guardrails.ErrSurface
			}
			for _, segment := range result.Segments {
				if strings.HasPrefix(segment.ID, "tool_") {
					if !json.Valid([]byte(segment.Text)) {
						return nil, guardrails.ErrSurface
					}
					g.calls[strings.TrimPrefix(segment.ID, "tool_")].arguments = segment.Text
				}
			}
			if err := g.rewriteTools(); err != nil {
				return nil, err
			}
		}
		var out [][]byte
		for _, pending := range g.pending {
			frames, err := g.pushText(ctx, pending)
			if err != nil {
				return nil, err
			}
			out = append(out, frames...)
		}
		g.pending = nil
		g.pendingBytes = 0
		g.calls = map[string]*toolCall{}
		return out, nil
	}
	return g.pushText(ctx, frame)
}

func (g *Stream) pushText(ctx context.Context, frame []byte) ([][]byte, error) {
	fields, err := frameFields(g.config.Family, frame)
	if err != nil {
		return nil, err
	}
	var deltas []guardrails.Delta
	var snapshots []guardrails.Segment
	var completed []string
	originals := map[string]string{}
	for _, field := range fields {
		if field.complete {
			completed = append(completed, field.id)
			if field.pointer == "" {
				continue
			}
		}
		if field.snapshot {
			originals[field.id] = field.text
			snapshots = append(snapshots, guardrails.Segment{ID: field.id, Role: guardrails.Assistant, Text: g.config.Masks.Restore(field.text)})
		} else {
			deltas = append(deltas, guardrails.Delta{ID: field.id, Text: field.text})
		}
	}
	if len(snapshots) > 0 {
		result, err := g.config.Engine.Evaluate(ctx, g.config.evaluation(snapshots), nil)
		g.config.Record(result.Decisions)
		if err != nil {
			return nil, err
		}
		changes := map[string]string{}
		for _, segment := range result.Segments {
			if segment.Text != originals[segment.ID] {
				changes[segment.ID] = segment.Text
			}
		}
		if len(changes) > 0 {
			if g.config.Strict {
				return nil, guardrails.ErrSurface
			}
			frame, err = rewriteFrame(g.config.Family, frame, changes)
			if err != nil {
				return nil, err
			}
		}
	}
	return g.stream.PushSegments(ctx, frame, deltas, completed...)
}
func (g *Stream) Finish(ctx context.Context) ([][]byte, error) {
	if len(g.pending) > 0 {
		return nil, guardrails.ErrSurface
	}
	return g.stream.Finish(ctx)
}

func Terminal(family wire.Format, frame []byte) bool {
	switch family {
	case wire.Chat, wire.Completions:
		for line := range bytes.SplitSeq(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) && bytes.Equal(bytes.TrimSpace(line[5:]), []byte("[DONE]")) {
				return true
			}
		}
		return false
	case wire.Responses:
		return bytes.Contains(frame, []byte("response.completed")) || bytes.Contains(frame, []byte("response.failed"))
	case wire.Anthropic:
		return bytes.Contains(frame, []byte("message_stop"))
	case wire.Gemini, wire.GeminiStream:
		return bytes.Contains(frame, []byte("finishReason"))
	case wire.Bedrock:
		return bytes.Contains(frame, []byte("messageStop"))
	}
	return false
}
