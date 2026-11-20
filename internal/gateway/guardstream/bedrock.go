package guardstream

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func (g *Stream) collectBedrock(data []byte, root map[string]json.RawMessage) (bool, error) {
	var event struct {
		Index int `json:"contentBlockIndex"`
		Start struct {
			Tool *struct{ Name string } `json:"toolUse"`
		} `json:"start"`
		Delta struct {
			Tool *struct{ Input string } `json:"toolUse"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false, guardrails.ErrSurface
	}
	touched := false
	id := strconv.Itoa(event.Index)
	if event.Start.Tool != nil {
		touched = true
		g.tool(id).name = event.Start.Tool.Name
	}
	if event.Delta.Tool != nil {
		touched = true
		g.tool(id).arguments += event.Delta.Tool.Input
	}
	_, start := root["start"]
	_, delta := root["delta"]
	if call := g.calls[id]; !start && !delta && call != nil {
		call.done = true
	}
	return touched, nil
}

func rewriteBedrock(root map[string]any, argument func(string) string) {
	if tool := jsonObject(jsonObject(root["delta"])["toolUse"]); tool != nil {
		tool["input"] = argument(jsonIndex(root["contentBlockIndex"]))
	}
}

func bedrockFields(frame []byte) ([]field, error) {
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
	return []field{{id: "stream", text: payload.Delta.Text}}, nil
}
