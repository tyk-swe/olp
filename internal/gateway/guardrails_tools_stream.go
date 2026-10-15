package gateway

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type streamToolCall struct {
	chunks          bool
	name, arguments string
	done            bool
}

func (g *guardrailStream) tool(id string) *streamToolCall {
	v := g.calls[id]
	if v == nil {
		v = &streamToolCall{}
		g.calls[id] = v
	}
	return v
}
func (g *guardrailStream) collectTools(frame []byte) (bool, error) {
	var data []byte
	if g.family == wire.Bedrock {
		message, err := eventstream.NewDecoder().Decode(bytes.NewReader(frame), nil)
		if err != nil {
			return false, guardrails.ErrSurface
		}
		data = message.Payload
	} else {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data = bytes.TrimSpace(line[5:])
				break
			}
		}
	}
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return false, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false, guardrails.ErrSurface
	}
	touched := false
	switch g.family {
	case wire.Chat:
		var choices []struct {
			Index  int     `json:"index"`
			Finish *string `json:"finish_reason"`
			Delta  struct {
				Tools []struct {
					Index    int `json:"index"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function_call"`
			} `json:"delta"`
		}
		if json.Unmarshal(root["choices"], &choices) != nil {
			return false, guardrails.ErrSurface
		}
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
				for _, call := range g.calls {
					call.done = true
				}
			}
		}
	case wire.Anthropic:
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
		if event.Type == "content_block_stop" {
			if call := g.calls[id]; call != nil {
				if call.arguments == "" {
					call.arguments = "{}"
				}
				call.done = true
			}
		}
	case wire.Responses:
		type item struct{ ID, Type, Name, Arguments string }
		var event struct {
			Type, Delta string
			ItemID      string `json:"item_id"`
			Arguments   string `json:"arguments"`
			Item        item   `json:"item"`
			Response    struct {
				Output []item `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &event) != nil {
			return false, guardrails.ErrSurface
		}
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
	case wire.Gemini, wire.GeminiStream:
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
		for i, c := range event.Candidates {
			for j, p := range c.Content.Parts {
				if p.Function != nil {
					touched = true
					call := g.tool(strconv.Itoa(i) + "_" + strconv.Itoa(j))
					call.name = p.Function.Name
					call.arguments = string(p.Function.Args)
					call.done = true
				}
			}
		}
	case wire.Bedrock:
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
		id := strconv.Itoa(event.Index)
		if event.Start.Tool != nil {
			touched = true
			g.tool(id).name = event.Start.Tool.Name
		}
		if event.Delta.Tool != nil {
			touched = true
			g.tool(id).arguments += event.Delta.Tool.Input
		}
		if _, start := root["start"]; !start {
			if _, delta := root["delta"]; !delta {
				if call := g.calls[id]; call != nil {
					call.done = true
				}
			}
		}
	}
	return touched, nil
}

func jsonObject(v any) map[string]any { m, _ := v.(map[string]any); return m }
func jsonArray(v any) []any           { a, _ := v.([]any); return a }
func jsonText(v any) string           { s, _ := v.(string); return s }
func jsonIndex(v any) string {
	n, _ := v.(json.Number)
	i, _ := n.Int64()
	return strconv.FormatInt(i, 10)
}

func decodeGuardrailJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(value)
}

// rewriteTools places each complete validated argument document in its first
// argument delta. Later deltas become empty, and terminal snapshots are kept
// consistent. This runs only on transformed routes, after the whole call is
// buffered; successful strict frames never enter this path.
func (g *guardrailStream) rewriteTools() error {
	emitted := map[string]bool{}
	argument := func(id string) string {
		if emitted[id] {
			return ""
		}
		emitted[id] = true
		return g.calls[id].arguments
	}
	for i, frame := range g.pending {
		var data []byte
		var message eventstream.Message
		var lines [][]byte
		lineIndex := -1
		if g.family == wire.Bedrock {
			var err error
			message, err = eventstream.NewDecoder().Decode(bytes.NewReader(frame), nil)
			if err != nil {
				return guardrails.ErrSurface
			}
			data = message.Payload
		} else {
			lines = bytes.Split(frame, []byte("\n"))
			for j, line := range lines {
				if bytes.HasPrefix(line, []byte("data:")) {
					lineIndex = j
					data = bytes.TrimSpace(line[5:])
					break
				}
			}
		}
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var root map[string]any
		if decodeGuardrailJSON(data, &root) != nil {
			return guardrails.ErrSurface
		}
		switch g.family {
		case wire.Chat:
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
		case wire.Anthropic:
			id := jsonIndex(root["index"])
			if call := g.calls[id]; call != nil {
				if block := jsonObject(root["content_block"]); jsonText(block["type"]) == "tool_use" {
					var input any
					if call.chunks {
						input = map[string]any{}
					} else {
						if decodeGuardrailJSON([]byte(call.arguments), &input) != nil {
							return guardrails.ErrSurface
						}
					}
					block["input"] = input
				}
				if delta := jsonObject(root["delta"]); jsonText(delta["type"]) == "input_json_delta" {
					delta["partial_json"] = argument(id)
				}
			}
		case wire.Responses:
			typ, id := jsonText(root["type"]), jsonText(root["item_id"])
			if typ == "response.function_call_arguments.delta" && g.calls[id] != nil {
				root["delta"] = argument(id)
			}
			if typ == "response.function_call_arguments.done" && g.calls[id] != nil {
				root["arguments"] = g.calls[id].arguments
			}
			if item := jsonObject(root["item"]); item != nil && jsonText(item["type"]) == "function_call" {
				id = jsonText(item["id"])
				if call := g.calls[id]; call != nil {
					if typ == "response.output_item.done" || !call.chunks {
						item["arguments"] = call.arguments
					} else {
						item["arguments"] = ""
					}
				}
			}
			for _, value := range jsonArray(jsonObject(root["response"])["output"]) {
				item := jsonObject(value)
				if jsonText(item["type"]) == "function_call" {
					if call := g.calls[jsonText(item["id"])]; call != nil {
						item["arguments"] = call.arguments
					}
				}
			}
		case wire.Gemini, wire.GeminiStream:
			for ci, value := range jsonArray(root["candidates"]) {
				content := jsonObject(jsonObject(value)["content"])
				for pi, value := range jsonArray(content["parts"]) {
					if fn := jsonObject(jsonObject(value)["functionCall"]); fn != nil {
						var args any
						if decodeGuardrailJSON([]byte(g.calls[strconv.Itoa(ci)+"_"+strconv.Itoa(pi)].arguments), &args) != nil {
							return guardrails.ErrSurface
						}
						fn["args"] = args
					}
				}
			}
		case wire.Bedrock:
			if tool := jsonObject(jsonObject(root["delta"])["toolUse"]); tool != nil {
				tool["input"] = argument(jsonIndex(root["contentBlockIndex"]))
			}
		}
		encoded, err := json.Marshal(root)
		if err != nil {
			return guardrails.ErrSurface
		}
		if g.family == wire.Bedrock {
			message.Payload = encoded
			var out bytes.Buffer
			if eventstream.NewEncoder().Encode(&out, message) != nil {
				return guardrails.ErrSurface
			}
			g.pending[i] = out.Bytes()
		} else {
			lines[lineIndex] = append([]byte("data: "), encoded...)
			g.pending[i] = bytes.Join(lines, []byte("\n"))
		}
	}
	return nil
}
