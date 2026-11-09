package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	redactionBudget = 100 * time.Millisecond
	maxJSONDepth    = 64
)

var errRedaction = errors.New("capture redaction failed")

type content struct {
	input  map[string]json.RawMessage
	output string
	tools  []json.RawMessage
}

type decoded struct {
	input  map[string]any
	output string
	tools  []any
}

type jsonContext uint8

const (
	structure jsonContext = iota
	call
	result
	literal
)

type rewrite func(string) (string, error)

func redact(ctx context.Context, engine *guardrails.Engine, c content) (content, error) {
	ctx, cancel := context.WithTimeout(ctx, redactionBudget)
	defer cancel()
	parsed, err := decode(c)
	if err != nil {
		return content{}, err
	}
	segments := []guardrails.Segment{}
	size := 0
	if _, err := parsed.rewrite(func(text string) (string, error) {
		size += len(text)
		if !utf8.ValidString(text) || len(segments) >= guardrails.MaxSegments || size > maxRecord {
			return "", errRedaction
		}
		segments = append(segments, guardrails.Segment{ID: "s" + strconv.Itoa(len(segments)), Text: text})
		return text, nil
	}); err != nil {
		return content{}, err
	}
	evaluated, err := engine.Evaluate(ctx, guardrails.Evaluation{Phase: guardrails.Input, Segments: segments}, nil)
	if err != nil || evaluated.Blocked || ctx.Err() != nil || len(evaluated.Segments) != len(segments) {
		return content{}, errRedaction
	}
	next := 0
	redacted, err := parsed.rewrite(func(string) (string, error) {
		next++
		return evaluated.Segments[next-1].Text, nil
	})
	if err != nil {
		return content{}, err
	}
	encoded, err := redacted.encode()
	if err != nil || ctx.Err() != nil {
		return content{}, errRedaction
	}
	return encoded, nil
}

func decode(c content) (decoded, error) {
	input := make(map[string]any, len(c.input))
	for key, raw := range c.input {
		value, err := parseJSON(raw)
		if err != nil {
			return decoded{}, err
		}
		input[key] = value
	}
	tools := make([]any, len(c.tools))
	for i, raw := range c.tools {
		value, err := parseJSON(raw)
		if err != nil {
			return decoded{}, err
		}
		tools[i] = value
	}
	return decoded{input: input, output: c.output, tools: tools}, nil
}

func (d decoded) rewrite(text rewrite) (decoded, error) {
	input := make(map[string]any, len(d.input))
	for _, key := range slices.Sorted(maps.Keys(d.input)) {
		value, err := walk(d.input[key], 0, structure, text)
		if err != nil {
			return decoded{}, err
		}
		input[key] = value
	}
	output, err := text(d.output)
	if err != nil {
		return decoded{}, err
	}
	tools := make([]any, len(d.tools))
	for i, tool := range d.tools {
		value, err := walk(tool, 0, call, text)
		if err != nil {
			return decoded{}, err
		}
		tools[i] = value
	}
	return decoded{input: input, output: output, tools: tools}, nil
}

func (d decoded) encode() (content, error) {
	if len(d.output) > fieldMax {
		return content{}, errRedaction
	}
	input := make(map[string]json.RawMessage, len(d.input))
	for key, value := range d.input {
		raw, err := json.Marshal(value)
		if err != nil || len(raw) > fieldMax {
			return content{}, errRedaction
		}
		input[key] = raw
	}
	tools := make([]json.RawMessage, len(d.tools))
	size := 0
	for i, value := range d.tools {
		raw, err := json.Marshal(value)
		size += len(raw)
		if err != nil || size > fieldMax {
			return content{}, errRedaction
		}
		tools[i] = raw
	}
	return content{input: input, output: d.output, tools: tools}, nil
}

func parseJSON(raw []byte) (any, error) {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: fieldMax, MaxDepth: maxJSONDepth, MaxNodes: 8192}); err != nil {
		return nil, errRedaction
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errRedaction
	}
	return value, nil
}

func walk(value any, depth int, c jsonContext, text rewrite) (any, error) {
	if depth > maxJSONDepth {
		return nil, errRedaction
	}
	switch v := value.(type) {
	case map[string]any:
		return walkObject(v, depth, c, text)
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			rewritten, err := walk(child, depth+1, c, text)
			if err != nil {
				return nil, err
			}
			out[i] = rewritten
		}
		return out, nil
	}
	if c == call {
		return nil, errRedaction
	}
	switch v := value.(type) {
	case string:
		return text(v)
	case json.Number:
		return scalar(v, v.String(), text)
	case bool:
		return scalar(v, strconv.FormatBool(v), text)
	case nil:
		return scalar(nil, "null", text)
	}
	return nil, errRedaction
}

func scalar(original any, rendered string, text rewrite) (any, error) {
	replaced, err := text(rendered)
	if err != nil || replaced == rendered {
		return original, err
	}
	return replaced, nil
}

func walkObject(v map[string]any, depth int, c jsonContext, text rewrite) (any, error) {
	if c != literal && opaqueMedia(v) {
		return nil, errRedaction
	}
	scope := c
	if c != literal && (v["type"] == "function_call" || v["type"] == "tool_use") {
		scope = call
	}
	out := make(map[string]any, len(v))
	for _, key := range slices.Sorted(maps.Keys(v)) {
		renamed, err := text(key)
		if err != nil {
			return nil, err
		}
		child, err := member(v[key], key, depth, scope, text)
		if err != nil {
			return nil, err
		}
		if _, taken := out[renamed]; taken {
			return nil, errRedaction
		}
		out[renamed] = child
	}
	return out, nil
}

func member(child any, key string, depth int, c jsonContext, text rewrite) (any, error) {
	raw, encoded := child.(string)
	if c != call || key != "arguments" || !encoded {
		return walk(child, depth+1, memberContext(c, key, child), text)
	}
	nested, err := parseJSON([]byte(raw))
	if err != nil {
		return nil, err
	}
	rewritten, err := walk(nested, depth+1, literal, text)
	if err != nil {
		return nil, err
	}
	reencoded, err := json.Marshal(rewritten)
	if err != nil {
		return nil, errRedaction
	}
	return string(reencoded), nil
}

func memberContext(c jsonContext, key string, child any) jsonContext {
	switch {
	case c == literal:
		return literal
	case c == call && (key == "arguments" || key == "input" || key == "args"):
		return literal
	case c == result && key == "response", key == "json":
		return literal
	case key == "function" || key == "function_call" || key == "functionCall" || key == "toolUse" || key == "tool_calls":
		if child != nil {
			return call
		}
		return structure
	case key == "functionResponse":
		return result
	}
	return structure
}

func opaqueMedia(value map[string]any) bool {
	if kind, ok := value["type"].(string); ok {
		switch kind {
		case "image_url", "image", "input_image", "audio", "input_audio", "output_audio", "file", "input_file", "document", "video":
			return true
		}
	}
	return slices.ContainsFunc([]string{"inlineData", "inline_data", "fileData", "file_data", "image", "audio", "document", "video"}, func(key string) bool {
		_, ok := value[key].(map[string]any)
		return ok
	})
}
