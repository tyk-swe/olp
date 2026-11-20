package guardstream

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func (g *Stream) collectAnthropic(data []byte) (bool, error) {
	var event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Block struct {
			Type, Name string
			Input      json.RawMessage `json:"input"`
		} `json:"content_block"`
		Delta struct {
			Partial string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false, guardrails.ErrSurface
	}
	touched := false
	id := strconv.Itoa(event.Index)
	if event.Type == "content_block_start" && event.Block.Type == "tool_use" {
		touched = true
		call := g.tool(id)
		call.name = event.Block.Name
		if len(event.Block.Input) > 0 && !bytes.Equal(event.Block.Input, []byte("{}")) {
			call.arguments = string(event.Block.Input)
		}
	}
	if event.Delta.Partial != "" {
		touched = true
		g.tool(id).arguments += event.Delta.Partial
		g.tool(id).chunks = true
	}
	if call := g.calls[id]; event.Type == "content_block_stop" && call != nil {
		if call.arguments == "" {
			call.arguments = "{}"
		}
		call.done = true
	}
	return touched, nil
}

func (g *Stream) rewriteAnthropic(root map[string]any, argument func(string) string) error {
	id := jsonIndex(root["index"])
	call := g.calls[id]
	if call == nil {
		return nil
	}
	if block := jsonObject(root["content_block"]); jsonText(block["type"]) == "tool_use" {
		var input any = map[string]any{}
		if !call.chunks && decodeJSON([]byte(call.arguments), &input) != nil {
			return guardrails.ErrSurface
		}
		block["input"] = input
	}
	if delta := jsonObject(root["delta"]); jsonText(delta["type"]) == "input_json_delta" {
		delta["partial_json"] = argument(id)
	}
	return nil
}

func anthropicFields(doc oif.Document) ([]field, error) {
	prefix := "anthropic_" + index(doc.Root(), 0) + "_"
	fields := append(text(doc, prefix+"text", "/content_block/text", false), text(doc, prefix+"text", "/delta/text", false)...)
	if typ, ok := doc.Root().Lookup("type"); ok {
		if event, _ := typ.Text(); event == "content_block_stop" {
			fields = append(fields, field{id: prefix, complete: true})
		}
	}
	return fields, nil
}
