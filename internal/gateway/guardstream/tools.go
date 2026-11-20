package guardstream

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type toolCall struct {
	chunks          bool
	name, arguments string
	done            bool
}

func (g *Stream) tool(id string) *toolCall {
	v := g.calls[id]
	if v == nil {
		v = &toolCall{}
		g.calls[id] = v
	}
	return v
}

type envelope struct {
	binary  bool
	message eventstream.Message
	lines   [][]byte
	line    int
}

func openFrame(family wire.Format, frame []byte) (envelope, []byte, error) {
	if family == wire.Bedrock {
		message, err := eventstream.NewDecoder().Decode(bytes.NewReader(frame), nil)
		if err != nil {
			return envelope{}, nil, guardrails.ErrSurface
		}
		return envelope{binary: true, message: message}, message.Payload, nil
	}
	lines := bytes.Split(frame, []byte("\n"))
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			return envelope{lines: lines, line: i}, bytes.TrimSpace(line[5:]), nil
		}
	}
	return envelope{}, nil, nil
}

func (e envelope) seal(payload []byte) ([]byte, error) {
	if !e.binary {
		e.lines[e.line] = append([]byte("data: "), payload...)
		return bytes.Join(e.lines, []byte("\n")), nil
	}
	e.message.Payload = payload
	var out bytes.Buffer
	if eventstream.NewEncoder().Encode(&out, e.message) != nil {
		return nil, guardrails.ErrSurface
	}
	return out.Bytes(), nil
}

func empty(data []byte) bool {
	return len(data) == 0 || bytes.Equal(data, []byte("[DONE]"))
}

func (g *Stream) collectTools(frame []byte) (bool, error) {
	_, data, err := openFrame(g.config.Family, frame)
	if err != nil || empty(data) {
		return false, err
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false, guardrails.ErrSurface
	}
	switch g.config.Family {
	case wire.Chat:
		return g.collectChat(root["choices"])
	case wire.Anthropic:
		return g.collectAnthropic(data)
	case wire.Responses:
		return g.collectResponses(data)
	case wire.Gemini, wire.GeminiStream:
		return g.collectGemini(data)
	case wire.Bedrock:
		return g.collectBedrock(data, root)
	}
	return false, nil
}

func jsonObject(v any) map[string]any { m, _ := v.(map[string]any); return m }
func jsonArray(v any) []any           { a, _ := v.([]any); return a }
func jsonText(v any) string           { s, _ := v.(string); return s }
func jsonIndex(v any) string {
	n, _ := v.(json.Number)
	i, _ := n.Int64()
	return strconv.FormatInt(i, 10)
}

func decodeJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(value)
}

// rewriteTools places each complete validated argument document in its first
// argument delta. Later deltas become empty, and terminal snapshots are kept
// consistent. This runs only on transformed routes, after the whole call is
// buffered; successful strict frames never enter this path.
func (g *Stream) rewriteTools() error {
	emitted := map[string]bool{}
	argument := func(id string) string {
		if emitted[id] {
			return ""
		}
		emitted[id] = true
		return g.calls[id].arguments
	}
	for i, frame := range g.pending {
		envelope, data, err := openFrame(g.config.Family, frame)
		if err != nil {
			return err
		}
		if empty(data) {
			continue
		}
		var root map[string]any
		if decodeJSON(data, &root) != nil {
			return guardrails.ErrSurface
		}
		if err := g.rewriteCall(root, argument); err != nil {
			return err
		}
		encoded, err := json.Marshal(root)
		if err != nil {
			return guardrails.ErrSurface
		}
		if g.pending[i], err = envelope.seal(encoded); err != nil {
			return err
		}
	}
	return nil
}

func (g *Stream) rewriteCall(root map[string]any, argument func(string) string) error {
	switch g.config.Family {
	case wire.Chat:
		g.rewriteChat(root, argument)
	case wire.Anthropic:
		return g.rewriteAnthropic(root, argument)
	case wire.Responses:
		g.rewriteResponses(root, argument)
	case wire.Gemini, wire.GeminiStream:
		return g.rewriteGemini(root)
	case wire.Bedrock:
		rewriteBedrock(root, argument)
	}
	return nil
}
