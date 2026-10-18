package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type guardrailStream struct {
	x            *call
	calls        map[string]*streamToolCall
	pending      [][]byte
	pendingBytes int
	stream       *guardrails.Stream
	family       wire.Format
}

func newGuardrailStream(x *call) (*guardrailStream, error) {
	s, err := guardrails.NewStream(x.guardrailEngine, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.route.Slug, InternalModel: x.internalGuardrail}, x.snapshot().Guardrails.Window())
	if err != nil {
		return nil, err
	}
	if x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask) {
		s.Masking(x.guardrailMasks)
	}
	if !x.strict() {
		s.SegmentRewriter(func(frame []byte, texts map[string]string) ([]byte, error) {
			return rewriteGuardrailFrame(x.family, frame, texts)
		})
	}
	return &guardrailStream{stream: s, family: x.family, x: x, calls: map[string]*streamToolCall{}}, nil
}
func (g *guardrailStream) push(ctx context.Context, frame []byte) ([][]byte, error) {
	touched, err := g.collectTools(frame)
	if err != nil {
		return nil, err
	}
	if touched || len(g.pending) > 0 {
		g.pendingBytes += len(frame)
		if g.pendingBytes > g.x.snapshot().Guardrails.Window() {
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
			if err := decodeGuardrailJSON([]byte(call.arguments), &data); err != nil {
				return nil, guardrails.ErrSurface
			}
			canonical, _ := json.Marshal(data)
			arguments := g.x.guardrailMasks.Restore(string(canonical))
			segments = append(segments, guardrails.Segment{ID: "tool_" + id, Kind: "tool_call", ToolName: call.name, Text: arguments, Role: "assistant"})
		}
		for _, segment := range g.x.guardrailInput {
			if segment.Kind == "tool_result" {
				segment.ID = "input_" + segment.ID
				segments = append(segments, segment)
			}
		}
		result, err := g.x.guardrailEngine.Evaluate(g.x.guardrailCtx, guardrails.Evaluation{RequestID: g.x.request.accountingID(), Route: g.x.route.Slug, Phase: guardrails.OutputStream, Segments: segments, InternalModel: g.x.internalGuardrail}, g.x.guardrailMasks)
		g.x.recordGuardrails(result.Decisions)
		if err != nil {
			return nil, err
		}
		restoreChanged := false
		for _, segment := range segments {
			if strings.HasPrefix(segment.ID, "tool_") {
				original := g.calls[strings.TrimPrefix(segment.ID, "tool_")].arguments
				restoreChanged = restoreChanged || g.x.guardrailMasks.Restore(original) != original
			}
		}
		if result.Changed || restoreChanged {
			if g.x.strict() {
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
		g.calls = map[string]*streamToolCall{}
		return out, nil
	}
	return g.pushText(ctx, frame)
}

func (g *guardrailStream) pushText(ctx context.Context, frame []byte) ([][]byte, error) {
	fields, err := guardrailFrameFields(g.family, frame)
	if err != nil {
		return nil, err
	}
	var deltas, snapshots []guardrails.Segment
	var completed []string
	originals := map[string]string{}
	for _, field := range fields {
		if field.complete {
			completed = append(completed, field.id)
			if field.pointer == "" {
				continue
			}
		}
		segment := guardrails.Segment{ID: field.id, Role: "assistant", Text: field.text}
		if field.snapshot {
			originals[field.id] = field.text
			segment.Text = g.x.guardrailMasks.Restore(segment.Text)
			snapshots = append(snapshots, segment)
		} else {
			deltas = append(deltas, segment)
		}
	}
	if len(snapshots) > 0 {
		result, err := g.x.guardrailEngine.Evaluate(ctx, guardrails.Evaluation{RequestID: g.x.request.accountingID(), Route: g.x.route.Slug, Phase: guardrails.OutputStream, Segments: snapshots, InternalModel: g.x.internalGuardrail}, nil)
		g.x.recordGuardrails(result.Decisions)
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
			if g.x.strict() {
				return nil, guardrails.ErrSurface
			}
			frame, err = rewriteGuardrailFrame(g.family, frame, changes)
			if err != nil {
				return nil, err
			}
		}
	}
	return g.stream.PushSegments(ctx, frame, deltas, completed...)
}
func (g *guardrailStream) finish(ctx context.Context) ([][]byte, error) {
	if len(g.pending) > 0 {
		return nil, guardrails.ErrSurface
	}
	return g.stream.Finish(ctx)
}

func terminalGuardrailFrame(family wire.Format, frame []byte) bool {
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
