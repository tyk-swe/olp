package guardstream

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type field struct {
	id, pointer, text string
	snapshot          bool
	complete          bool
}

// Parse the complete SSE data payload. Keep the original frame for passing
// streams; transformations replace only data, preserving event metadata.
func frameDocument(frame []byte) (*oif.Document, [][]byte, []int, error) {
	lines := bytes.Split(frame, []byte("\n"))
	var indices []int
	var data []byte
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if len(indices) > 0 {
				data = append(data, '\n')
			}
			indices = append(indices, i)
			data = append(data, bytes.TrimSpace(line[5:])...)
		}
	}
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil, lines, indices, nil
	}
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: guardrails.MaxWindowBytes})
	if err != nil {
		return nil, nil, nil, guardrails.ErrSurface
	}
	return &doc, lines, indices, nil
}

func frameFields(family wire.Format, frame []byte) ([]field, error) {
	if family == wire.Bedrock {
		return bedrockFields(frame)
	}
	doc, _, _, err := frameDocument(frame)
	if err != nil || doc == nil {
		return nil, err
	}
	return documentFields(family, *doc)
}

func text(doc oif.Document, id, pointer string, snapshot bool) []field {
	value, ok := doc.Lookup(pointer)
	if !ok || value.Kind() != oif.String {
		return nil
	}
	content, _ := value.Text()
	return []field{{id: id, pointer: pointer, text: content, snapshot: snapshot}}
}

func index(value oif.Value, fallback int) string {
	if v, ok := value.Lookup("index"); ok && v.Kind() == oif.Number {
		return v.Raw()
	}
	return strconv.Itoa(fallback)
}

func documentFields(family wire.Format, doc oif.Document) ([]field, error) {
	switch family {
	case wire.Completions:
		return completionsFields(doc)
	case wire.Chat:
		return chatFields(doc)
	case wire.Gemini, wire.GeminiStream:
		return geminiFields(doc)
	case wire.Anthropic:
		return anthropicFields(doc)
	case wire.Responses:
		return responsesFields(doc)
	}
	return nil, guardrails.ErrSurface
}

func rewriteFrame(family wire.Format, frame []byte, texts map[string]string) ([]byte, error) {
	if family == wire.Bedrock {
		return nil, guardrails.ErrSurface
	}
	doc, lines, indices, err := frameDocument(frame)
	if err != nil || doc == nil {
		return frame, err
	}
	fields, err := documentFields(family, *doc)
	if err != nil {
		return nil, err
	}
	var changes []oif.Change
	for _, field := range fields {
		if text, ok := texts[field.id]; ok && text != field.text {
			raw, _ := json.Marshal(text)
			changes = append(changes, oif.Change{Pointer: field.pointer, Value: string(raw), Origin: oif.ExplicitTransform, Reason: "stream guardrail redaction"})
		}
	}
	if len(changes) == 0 {
		return frame, nil
	}
	next, err := oif.Apply(*doc, changes)
	if err != nil {
		return nil, guardrails.ErrSurface
	}
	lines[indices[0]] = append([]byte("data: "), next.Bytes()...)
	for i := len(indices) - 1; i > 0; i-- {
		lines = append(lines[:indices[i]], lines[indices[i]+1:]...)
	}
	return bytes.Join(lines, []byte("\n")), nil
}
