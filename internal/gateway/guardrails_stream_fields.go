package gateway

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type guardrailStreamField struct {
	id, pointer, text string
	snapshot          bool
	complete          bool
}

// Parse the complete SSE data payload. Keep the original frame for passing
// streams; transformations replace only data, preserving event metadata.
func guardrailFrameDocument(frame []byte) (*oif.Document, [][]byte, []int, error) {
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

func guardrailFrameFields(family wire.Format, frame []byte) ([]guardrailStreamField, error) {
	if family == wire.Bedrock {
		message, err := eventstream.NewDecoder().Decode(bytes.NewReader(frame), nil)
		if err != nil {
			return nil, guardrails.ErrSurface
		}
		var payload struct {
			Delta struct{ Text string } `json:"delta"`
		}
		if json.Unmarshal(message.Payload, &payload) != nil {
			return nil, guardrails.ErrSurface
		}
		return []guardrailStreamField{{id: "stream", text: payload.Delta.Text}}, nil
	}
	doc, _, _, err := guardrailFrameDocument(frame)
	if err != nil || doc == nil {
		return nil, err
	}
	return guardrailDocumentFields(family, *doc)
}

func guardrailDocumentFields(family wire.Format, doc oif.Document) ([]guardrailStreamField, error) {
	var fields []guardrailStreamField
	add := func(id, pointer string, snapshot bool) {
		if value, ok := doc.Lookup(pointer); ok && value.Kind() == oif.String {
			text, _ := value.Text()
			fields = append(fields, guardrailStreamField{id: id, pointer: pointer, text: text, snapshot: snapshot})
		}
	}
	index := func(value oif.Value, fallback int) string {
		if v, ok := value.Lookup("index"); ok && v.Kind() == oif.Number {
			return v.Raw()
		}
		return strconv.Itoa(fallback)
	}
	switch family {
	case wire.Chat:
		choices, _ := doc.Root().Lookup("choices")
		for i, choice := range choices.Elements() {
			prefix := "chat_" + index(choice, i) + "_"
			for _, key := range []string{"content", "refusal"} {
				add(prefix+key, "/choices/"+strconv.Itoa(i)+"/delta/"+key, false)
			}
			if reason, ok := choice.Lookup("finish_reason"); ok && reason.Kind() == oif.String {
				fields = append(fields, guardrailStreamField{id: prefix, complete: true})
			}
		}
	case wire.Gemini, wire.GeminiStream:
		candidates, _ := doc.Root().Lookup("candidates")
		for i, candidate := range candidates.Elements() {
			prefix := "gemini_" + index(candidate, i) + "_"
			content, _ := candidate.Lookup("content")
			parts, _ := content.Lookup("parts")
			for j := range parts.Elements() {
				add(prefix+strconv.Itoa(j), "/candidates/"+strconv.Itoa(i)+"/content/parts/"+strconv.Itoa(j)+"/text", false)
			}
			if reason, ok := candidate.Lookup("finishReason"); ok && reason.Kind() == oif.String {
				fields = append(fields, guardrailStreamField{id: prefix, complete: true})
			}
		}
	case wire.Anthropic:
		prefix := "anthropic_" + index(doc.Root(), 0) + "_"
		add(prefix+"text", "/content_block/text", false)
		add(prefix+"text", "/delta/text", false)
		if typ, ok := doc.Root().Lookup("type"); ok {
			if event, _ := typ.Text(); event == "content_block_stop" {
				fields = append(fields, guardrailStreamField{id: prefix, complete: true})
			}
		}
	case wire.Responses:
		typ, _ := doc.Root().Lookup("type")
		event, _ := typ.Text()
		outputIndex, contentIndex := "0", "0"
		if v, ok := doc.Root().Lookup("output_index"); ok && v.Kind() == oif.Number {
			outputIndex = v.Raw()
		}
		if v, ok := doc.Root().Lookup("content_index"); ok && v.Kind() == oif.Number {
			contentIndex = v.Raw()
		}
		id := "responses_" + outputIndex + "_" + contentIndex
		switch event {
		case "response.output_text.delta":
			add(id+"_text", "/delta", false)
		case "response.refusal.delta":
			add(id+"_refusal", "/delta", false)
		case "response.output_text.done":
			add(id+"_text", "/text", true)
		case "response.refusal.done":
			add(id+"_refusal", "/refusal", true)
		}
		for _, key := range []string{"text", "refusal"} {
			add(id+"_"+key, "/part/"+key, true)
		}
		item, _ := doc.Root().Lookup("item")
		content, _ := item.Lookup("content")
		for j := range content.Elements() {
			for _, key := range []string{"text", "refusal"} {
				add("responses_"+outputIndex+"_"+strconv.Itoa(j)+"_"+key, "/item/content/"+strconv.Itoa(j)+"/"+key, true)
			}
		}
		response, _ := doc.Root().Lookup("response")
		output, _ := response.Lookup("output")
		for i, item := range output.Elements() {
			content, _ := item.Lookup("content")
			for j := range content.Elements() {
				for _, key := range []string{"text", "refusal"} {
					add("responses_"+strconv.Itoa(i)+"_"+strconv.Itoa(j)+"_"+key, "/response/output/"+strconv.Itoa(i)+"/content/"+strconv.Itoa(j)+"/"+key, true)
				}
			}
		}
		if strings.HasSuffix(event, ".done") || event == "response.completed" || event == "response.incomplete" {
			for i := range fields {
				fields[i].complete = fields[i].snapshot
			}
		}
	default:
		return nil, guardrails.ErrSurface
	}
	return fields, nil
}

func rewriteGuardrailFrame(family wire.Format, frame []byte, texts map[string]string) ([]byte, error) {
	if family == wire.Bedrock {
		return nil, guardrails.ErrSurface
	}
	doc, lines, indices, err := guardrailFrameDocument(frame)
	if err != nil || doc == nil {
		return frame, err
	}
	fields, err := guardrailDocumentFields(family, *doc)
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
