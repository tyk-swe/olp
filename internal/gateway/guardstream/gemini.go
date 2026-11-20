package guardstream

import (
	"encoding/json"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func (g *Stream) collectGemini(data []byte) (bool, error) {
	var event struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Function *struct {
						Name string
						Args json.RawMessage
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false, guardrails.ErrSurface
	}
	touched := false
	for i, candidate := range event.Candidates {
		for j, part := range candidate.Content.Parts {
			if part.Function == nil {
				continue
			}
			touched = true
			call := g.tool(strconv.Itoa(i) + "_" + strconv.Itoa(j))
			call.name, call.arguments, call.done = part.Function.Name, string(part.Function.Args), true
		}
	}
	return touched, nil
}

func (g *Stream) rewriteGemini(root map[string]any) error {
	for ci, value := range jsonArray(root["candidates"]) {
		content := jsonObject(jsonObject(value)["content"])
		for pi, value := range jsonArray(content["parts"]) {
			fn := jsonObject(jsonObject(value)["functionCall"])
			if fn == nil {
				continue
			}
			var args any
			if decodeJSON([]byte(g.calls[strconv.Itoa(ci)+"_"+strconv.Itoa(pi)].arguments), &args) != nil {
				return guardrails.ErrSurface
			}
			fn["args"] = args
		}
	}
	return nil
}

func geminiFields(doc oif.Document) ([]field, error) {
	var fields []field
	candidates, _ := doc.Root().Lookup("candidates")
	for i, candidate := range candidates.Elements() {
		prefix := "gemini_" + index(candidate, i) + "_"
		content, _ := candidate.Lookup("content")
		parts, _ := content.Lookup("parts")
		for j := range parts.Elements() {
			fields = append(fields, text(doc, prefix+strconv.Itoa(j), "/candidates/"+strconv.Itoa(i)+"/content/parts/"+strconv.Itoa(j)+"/text", false)...)
		}
		if reason, ok := candidate.Lookup("finishReason"); ok && reason.Kind() == oif.String {
			fields = append(fields, field{id: prefix, complete: true})
		}
	}
	return fields, nil
}
