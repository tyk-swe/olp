package guardstream

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatChoice struct {
	Index  int     `json:"index"`
	Finish *string `json:"finish_reason"`
	Delta  struct {
		Tools []struct {
			Index    int          `json:"index"`
			Function chatFunction `json:"function"`
		} `json:"tool_calls"`
		Function *chatFunction `json:"function_call"`
	} `json:"delta"`
}

func (g *Stream) collectChat(encoded json.RawMessage) (bool, error) {
	var choices []chatChoice
	if json.Unmarshal(encoded, &choices) != nil {
		return false, guardrails.ErrSurface
	}
	touched := false
	for _, choice := range choices {
		if len(choice.Delta.Tools) > 0 || choice.Delta.Function != nil {
			touched = true
		}
		for _, part := range choice.Delta.Tools {
			call := g.tool(strconv.Itoa(choice.Index) + "_" + strconv.Itoa(part.Index))
			call.name += part.Function.Name
			call.arguments += part.Function.Arguments
			call.chunks = true
		}
		if part := choice.Delta.Function; part != nil {
			call := g.tool(strconv.Itoa(choice.Index) + "_legacy")
			call.name += part.Name
			call.arguments += part.Arguments
		}
		if choice.Finish != nil {
			g.finishChoice(strconv.Itoa(choice.Index) + "_")
		}
	}
	return touched, nil
}

func (g *Stream) finishChoice(prefix string) {
	for id, call := range g.calls {
		if strings.HasPrefix(id, prefix) {
			call.done = true
		}
	}
}

func (g *Stream) rewriteChat(root map[string]any, argument func(string) string) {
	for _, value := range jsonArray(root["choices"]) {
		choice := jsonObject(value)
		delta := jsonObject(choice["delta"])
		for _, value := range jsonArray(delta["tool_calls"]) {
			tool := jsonObject(value)
			fn := jsonObject(tool["function"])
			id := jsonIndex(choice["index"]) + "_" + jsonIndex(tool["index"])
			if fn != nil && g.calls[id] != nil {
				fn["arguments"] = argument(id)
			}
		}
		if fn := jsonObject(delta["function_call"]); fn != nil {
			fn["arguments"] = argument(jsonIndex(choice["index"]) + "_legacy")
		}
	}
}

func chatFields(doc oif.Document) ([]field, error) {
	var fields []field
	choices, _ := doc.Root().Lookup("choices")
	for i, choice := range choices.Elements() {
		if value, ok := choice.Lookup("logprobs"); ok && value.Kind() != oif.Null {
			return nil, guardrails.ErrSurface
		}
		prefix := "chat_" + index(choice, i) + "_"
		for _, key := range []string{"content", "refusal"} {
			fields = append(fields, text(doc, prefix+key, "/choices/"+strconv.Itoa(i)+"/delta/"+key, false)...)
		}
		if reason, ok := choice.Lookup("finish_reason"); ok && reason.Kind() == oif.String {
			fields = append(fields, field{id: prefix, complete: true})
		}
	}
	return fields, nil
}

func completionsFields(doc oif.Document) ([]field, error) {
	var fields []field
	choices, _ := doc.Root().Lookup("choices")
	for i, choice := range choices.Elements() {
		if value, ok := choice.Lookup("logprobs"); ok && value.Kind() != oif.Null {
			return nil, guardrails.ErrSurface
		}
		prefix := "completions_" + index(choice, i) + "_"
		fields = append(fields, text(doc, prefix+"text", "/choices/"+strconv.Itoa(i)+"/text", false)...)
		if reason, ok := choice.Lookup("finish_reason"); ok && reason.Kind() == oif.String && reason.Raw() != `""` {
			fields = append(fields, field{id: prefix, complete: true})
		}
	}
	return fields, nil
}
