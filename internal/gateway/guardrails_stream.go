package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
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
		s.Rewriter(func(frame []byte, text string) ([]byte, error) { return rewriteGuardrailFrame(x.family, frame, text) })
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
			_ = json.Unmarshal([]byte(call.arguments), &data)
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
			text, err := guardrailFrameText(g.family, pending)
			if err != nil {
				return nil, err
			}
			frames, err := g.stream.Push(ctx, pending, text)
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
	text, err := guardrailFrameText(g.family, frame)
	if err != nil {
		return nil, err
	}
	return g.stream.Push(ctx, frame, text)
}
func (g *guardrailStream) finish(ctx context.Context) ([][]byte, error) {
	if len(g.pending) > 0 {
		return nil, guardrails.ErrSurface
	}
	return g.stream.Finish(ctx)
}

// Only native text deltas enter the holdback window. Tool arguments require a
// complete structured value and are refused rather than inspected partially.
func guardrailFrameText(family wire.Format, frame []byte) (string, error) {
	if family == wire.Bedrock {
		message, err := eventstream.NewDecoder().Decode(bytes.NewReader(frame), nil)
		if err != nil {
			return "", guardrails.ErrSurface
		}
		var payload struct {
			Delta struct {
				Text string          `json:"text"`
				Tool json.RawMessage `json:"toolUse"`
			} `json:"delta"`
		}
		if json.Unmarshal(message.Payload, &payload) != nil {
			return "", guardrails.ErrSurface
		}

		return payload.Delta.Text, nil
	}
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(line[5:])...)
		}
	}
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return "", nil
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(data, &value) != nil {
		return "", guardrails.ErrSurface
	}
	var text strings.Builder
	switch family {
	case wire.Chat:
		var choices []struct {
			Delta struct {
				Content  string          `json:"content"`
				Refusal  string          `json:"refusal"`
				Tools    json.RawMessage `json:"tool_calls"`
				Function json.RawMessage `json:"function_call"`
			} `json:"delta"`
		}
		if json.Unmarshal(value["choices"], &choices) != nil {
			return "", guardrails.ErrSurface
		}
		for _, c := range choices {

			text.WriteString(c.Delta.Content)
			text.WriteString(c.Delta.Refusal)
		}
	case wire.Responses:
		var typ string
		_ = json.Unmarshal(value["type"], &typ)

		if typ == "response.output_text.delta" || typ == "response.refusal.delta" {
			var delta string
			if json.Unmarshal(value["delta"], &delta) != nil {
				return "", guardrails.ErrSurface
			}
			text.WriteString(delta)
		}
	case wire.Anthropic:
		var delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		}
		_ = json.Unmarshal(value["delta"], &delta)

		text.WriteString(delta.Text)
	case wire.Gemini, wire.GeminiStream:
		var candidates []struct {
			Content struct {
				Parts []struct {
					Text     string          `json:"text"`
					Function json.RawMessage `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		}
		if json.Unmarshal(value["candidates"], &candidates) != nil {
			return "", guardrails.ErrSurface
		}
		for _, c := range candidates {
			for _, p := range c.Content.Parts {

				text.WriteString(p.Text)
			}
		}
	default:
		return "", guardrails.ErrSurface
	}
	return text.String(), nil
}

func rewriteGuardrailFrame(family wire.Format, frame []byte, text string) ([]byte, error) {
	if family == wire.Bedrock {
		return nil, guardrails.ErrSurface
	}
	lines := bytes.Split(frame, []byte("\n"))
	dataIndex := -1
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if dataIndex >= 0 {
				return nil, guardrails.ErrSurface
			}
			dataIndex = i
		}
	}
	if dataIndex < 0 {
		return frame, nil
	}
	data := bytes.TrimSpace(lines[dataIndex][5:])
	if bytes.Equal(data, []byte("[DONE]")) {
		return frame, nil
	}
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: guardrails.MaxWindowBytes})
	if err != nil {
		return nil, guardrails.ErrSurface
	}
	var paths []string
	switch family {
	case wire.Chat:
		choices, _ := doc.Root().Lookup("choices")
		for i, c := range choices.Elements() {
			delta, _ := c.Lookup("delta")
			for _, key := range []string{"content", "refusal"} {
				if v, ok := delta.Lookup(key); ok && v.Kind() == oif.String {
					paths = append(paths, "/choices/"+strconv.Itoa(i)+"/delta/"+key)
				}
			}
		}
	case wire.Responses:
		if v, ok := doc.Root().Lookup("delta"); ok && v.Kind() == oif.String {
			paths = []string{"/delta"}
		}
	case wire.Anthropic:
		if v, ok := doc.Lookup("/delta/text"); ok && v.Kind() == oif.String {
			paths = []string{"/delta/text"}
		}
	case wire.Gemini, wire.GeminiStream:
		candidates, _ := doc.Root().Lookup("candidates")
		for i, c := range candidates.Elements() {
			content, _ := c.Lookup("content")
			parts, _ := content.Lookup("parts")
			for j, p := range parts.Elements() {
				if v, ok := p.Lookup("text"); ok && v.Kind() == oif.String {
					paths = append(paths, "/candidates/"+strconv.Itoa(i)+"/content/parts/"+strconv.Itoa(j)+"/text")
				}
			}
		}
	}
	changes := []oif.Change{}
	for i, path := range paths {
		value := ""
		if i == 0 {
			value = text
		}
		raw, _ := json.Marshal(value)
		changes = append(changes, oif.Change{Pointer: path, Value: string(raw), Origin: oif.ExplicitTransform, Reason: "stream guardrail redaction"})
	}
	next, err := oif.Apply(doc, changes)
	if err != nil {
		return nil, guardrails.ErrSurface
	}
	lines[dataIndex] = append([]byte("data: "), next.Bytes()...)
	return bytes.Join(lines, []byte("\n")), nil
}

func terminalGuardrailFrame(family wire.Format, frame []byte) bool {
	switch family {
	case wire.Chat:
		return bytes.Contains(frame, []byte("data: [DONE]"))
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
