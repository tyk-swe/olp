package guardstream

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type responsesItem struct{ ID, Type, Name, Arguments string }

func (g *Stream) collectResponses(data []byte) (bool, error) {
	var event struct {
		Type, Delta string
		ItemID      string        `json:"item_id"`
		Arguments   string        `json:"arguments"`
		Item        responsesItem `json:"item"`
		Response    struct {
			Output []responsesItem `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false, guardrails.ErrSurface
	}
	touched := false
	if event.Item.Type == "function_call" {
		touched = true
		call := g.tool(event.Item.ID)
		call.name = event.Item.Name
		if event.Item.Arguments != "" {
			call.arguments = event.Item.Arguments
		}
		if event.Type == "response.output_item.done" {
			call.done = true
		}
	}
	if event.Type == "response.function_call_arguments.delta" {
		touched = true
		g.tool(event.ItemID).arguments += event.Delta
		g.tool(event.ItemID).chunks = true
	}
	if event.Type == "response.function_call_arguments.done" {
		touched = true
		call := g.tool(event.ItemID)
		call.arguments = event.Arguments
		call.done = true
	}
	// Terminal responses repeat full tool calls, even when their deltas
	// and output_item.done frames have already been inspected and emitted.
	for _, item := range event.Response.Output {
		if item.Type == "function_call" {
			touched = true
			call := g.tool(item.ID)
			call.name, call.arguments, call.done = item.Name, item.Arguments, true
		}
	}
	return touched, nil
}

func (g *Stream) rewriteResponses(root map[string]any, argument func(string) string) {
	typ, id := jsonText(root["type"]), jsonText(root["item_id"])
	if typ == "response.function_call_arguments.delta" && g.calls[id] != nil {
		root["delta"] = argument(id)
	}
	if typ == "response.function_call_arguments.done" && g.calls[id] != nil {
		root["arguments"] = g.calls[id].arguments
	}
	if item := jsonObject(root["item"]); item != nil && jsonText(item["type"]) == "function_call" {
		if call := g.calls[jsonText(item["id"])]; call != nil {
			item["arguments"] = ""
			if typ == "response.output_item.done" || !call.chunks {
				item["arguments"] = call.arguments
			}
		}
	}
	for _, value := range jsonArray(jsonObject(root["response"])["output"]) {
		item := jsonObject(value)
		if call := g.calls[jsonText(item["id"])]; call != nil && jsonText(item["type"]) == "function_call" {
			item["arguments"] = call.arguments
		}
	}
}

func responsesContent(doc oif.Document, content oif.Value, output, pointer string) []field {
	var fields []field
	for j := range content.Elements() {
		for _, key := range []string{"text", "refusal"} {
			fields = append(fields, text(doc, "responses_"+output+"_"+strconv.Itoa(j)+"_"+key, pointer+"/"+strconv.Itoa(j)+"/"+key, true)...)
		}
	}
	return fields
}

func responsesFields(doc oif.Document) ([]field, error) {
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
	var fields []field
	switch event {
	case "response.output_text.delta":
		fields = text(doc, id+"_text", "/delta", false)
	case "response.refusal.delta":
		fields = text(doc, id+"_refusal", "/delta", false)
	case "response.output_text.done":
		fields = text(doc, id+"_text", "/text", true)
	case "response.refusal.done":
		fields = text(doc, id+"_refusal", "/refusal", true)
	}
	for _, key := range []string{"text", "refusal"} {
		fields = append(fields, text(doc, id+"_"+key, "/part/"+key, true)...)
	}
	item, _ := doc.Root().Lookup("item")
	content, _ := item.Lookup("content")
	fields = append(fields, responsesContent(doc, content, outputIndex, "/item/content")...)
	response, _ := doc.Root().Lookup("response")
	output, _ := response.Lookup("output")
	for i, item := range output.Elements() {
		content, _ := item.Lookup("content")
		fields = append(fields, responsesContent(doc, content, strconv.Itoa(i), "/response/output/"+strconv.Itoa(i)+"/content")...)
	}
	if strings.HasSuffix(event, ".done") || event == "response.completed" || event == "response.incomplete" {
		for i := range fields {
			fields[i].complete = fields[i].snapshot
		}
	}
	return fields, nil
}
