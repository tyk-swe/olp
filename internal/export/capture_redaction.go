package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
)

const captureRedactionBudget = 100 * time.Millisecond

var errCaptureRedaction = errors.New("capture redaction failed")

// Each JSON scalar is decoded before evaluation. Nested JSON argument strings
// are decoded as well; replacements are serialized, never spliced into JSON.
// The tree and setters exist only in bounded request memory.
type captureTree struct {
	segments []guardrails.Segment
	setters  []func(string)
	bytes    int
}

func (t *captureTree) scalar(value string, set func(string)) error {
	t.bytes += len(value)
	if !utf8.ValidString(value) || len(t.segments) >= guardrails.MaxSegments || t.bytes > captureMaxRecord {
		return errCaptureRedaction
	}
	t.segments = append(t.segments, guardrails.Segment{ID: "s" + strconv.Itoa(len(t.segments)), Text: value})
	t.setters = append(t.setters, set)
	return nil
}

func captureJSON(raw []byte) (any, error) {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: captureFieldMax, MaxDepth: 64, MaxNodes: 8192}); err != nil {
		return nil, errCaptureRedaction
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, errCaptureRedaction
	}
	return value, nil
}

func (t *captureTree) walk(value any, set func(any), depth int) error {
	if depth > 64 {
		return errCaptureRedaction
	}
	switch v := value.(type) {
	case string:
		return t.scalar(v, func(s string) { set(s) })
	case json.Number:
		return t.scalar(v.String(), func(s string) {
			if s != v.String() {
				set(s)
			}
		})
	case []any:
		for i, child := range v {
			if err := t.walk(child, func(x any) { v[i] = x }, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			// Opaque media is not a text inspection surface.
			switch key {
			case "image_url", "image", "audio", "input_audio", "file_data", "file_id", "inlineData", "inline_data", "fileData", "file_data_uri", "source", "document", "video":
				return errCaptureRedaction
			}
			child := v[key]
			// Keys can contain user content too. A changed key is applied after values
			// below; collision is checked when rebuilding the final document.
			if err := t.scalar(key, func(s string) {
				if s != key {
					v["\x00capture-key:"+key] = s
				}
			}); err != nil {
				return err
			}
			if key == "arguments" {
				if raw, ok := child.(string); ok {
					nested, err := captureJSON([]byte(raw))
					if err != nil {
						return err
					}
					v[key] = &captureArguments{value: nested}
					box := v[key].(*captureArguments)
					if err := t.walk(nested, func(x any) { box.value = x }, depth+1); err != nil {
						return err
					}
					continue
				}
			}
			if strings.HasPrefix(key, "\x00capture-key:") {
				return errCaptureRedaction
			}
			if err := t.walk(child, func(x any) { v[key] = x }, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

type captureArguments struct{ value any }

func (a *captureArguments) MarshalJSON() ([]byte, error) {
	if err := finishCaptureTree(a.value); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(a.value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(raw))
}
func finishCaptureTree(value any) error {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !strings.HasPrefix(key, "\x00capture-key:") {
				if err := finishCaptureTree(v[key]); err != nil {
					return err
				}
			}
		}
		for _, key := range keys {
			if strings.HasPrefix(key, "\x00capture-key:") {
				original := strings.TrimPrefix(key, "\x00capture-key:")
				replacement := v[key].(string)
				if _, exists := v[replacement]; exists {
					return errCaptureRedaction
				}
				v[replacement] = v[original]
				delete(v, original)
				delete(v, key)
			}
		}
	case []any:
		for _, child := range v {
			if err := finishCaptureTree(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func redactCapture(ctx context.Context, engine *guardrails.Engine, input map[string]json.RawMessage, output string, calls []json.RawMessage) (map[string]json.RawMessage, string, []json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, captureRedactionBudget)
	defer cancel()
	tree := &captureTree{}
	values := map[string]any{}
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := captureJSON(input[key])
		if err != nil {
			return nil, "", nil, err
		}
		values[key] = value
		if err := tree.walk(value, func(x any) { values[key] = x }, 0); err != nil {
			return nil, "", nil, err
		}
	}
	if err := tree.scalar(output, func(s string) { output = s }); err != nil {
		return nil, "", nil, err
	}
	tools := make([]any, len(calls))
	for i, raw := range calls {
		value, err := captureJSON(raw)
		if err != nil {
			return nil, "", nil, err
		}
		tools[i] = value
		if err := tree.walk(value, func(x any) { tools[i] = x }, 0); err != nil {
			return nil, "", nil, err
		}
	}
	result, err := engine.Evaluate(ctx, guardrails.Evaluation{Phase: guardrails.Input, Segments: tree.segments}, nil)
	if err != nil || result.Blocked || ctx.Err() != nil || len(result.Segments) != len(tree.setters) {
		return nil, "", nil, errCaptureRedaction
	}
	for i, segment := range result.Segments {
		tree.setters[i](segment.Text)
	}
	redactedInput := map[string]json.RawMessage{}
	for _, key := range keys {
		if err := finishCaptureTree(values[key]); err != nil {
			return nil, "", nil, err
		}
		raw, err := json.Marshal(values[key])
		if err != nil || len(raw) > captureFieldMax {
			return nil, "", nil, errCaptureRedaction
		}
		redactedInput[key] = raw
	}
	redactedTools := make([]json.RawMessage, len(tools))
	size := 0
	for i, value := range tools {
		if err := finishCaptureTree(value); err != nil {
			return nil, "", nil, err
		}
		raw, err := json.Marshal(value)
		size += len(raw)
		if err != nil || size > captureFieldMax {
			return nil, "", nil, errCaptureRedaction
		}
		redactedTools[i] = raw
	}
	if len(output) > captureFieldMax || ctx.Err() != nil {
		return nil, "", nil, errCaptureRedaction
	}
	return redactedInput, output, redactedTools, nil
}
