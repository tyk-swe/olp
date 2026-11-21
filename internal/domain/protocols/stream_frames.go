package protocols

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func (t *streamTranslator) frame(frame []byte) error {
	return sse.Decode(strings.NewReader(string(frame)), max(t.limit, len(frame)), func(event sse.Frame) error {
		if event.Data == "[DONE]" {
			return nil
		}
		f, e := object([]byte(event.Data))
		if e != nil {
			return protocolError("invalid translated event")
		}
		switch t.source {
		case wire.Chat:
			return t.chatFrame(f)
		case wire.Responses:
			return t.responsesFrame(f)
		case wire.Anthropic:
			return t.anthropicFrame(f)
		case wire.Gemini:
			return t.geminiFrame(f)
		case "bedrock":
			return t.bedrockFrame(f)
		}
		return nil
	})
}

func (t *streamTranslator) chatFrame(f Object) error {
	if e := t.start(str(f["id"])); e != nil {
		return e
	}
	for _, v := range arr(f["choices"]) {
		choice, _ := object(v)
		idx, _ := position(choice["index"])
		if idx != 0 {
			return protocolError("translation requires one candidate")
		}
		delta, _ := optionalObject(choice["delta"])
		if e := t.refusalDelta(str(delta["refusal"])); e != nil {
			return e
		}
		if e := t.textDelta(str(delta["content"])); e != nil {
			return e
		}
		for _, v := range arr(delta["tool_calls"]) {
			call, _ := object(v)
			index, _ := position(call["index"])
			fun, _ := optionalObject(call["function"])
			if e := t.toolDelta(index, str(call["id"]), str(fun["name"]), str(fun["arguments"])); e != nil {
				return e
			}
		}
	}
	return nil
}

func (t *streamTranslator) responsesFrame(f Object) error {
	if response, e := optionalObject(f["response"]); e != nil {
		return e
	} else if response != nil {
		if e := t.start(str(response["id"])); e != nil {
			return e
		}
	}
	switch str(f["type"]) {
	case "response.function_call_arguments.done":
		index, _ := position(f["output_index"])
		call, present := t.calls[index]
		if !present {
			return protocolError("Responses argument snapshot has no tool call")
		}
		return t.responseToolSnapshot(index, Object{"type": raw("function_call"), "call_id": raw(call.id), "name": raw(call.name), "arguments": f["arguments"]})
	case "response.output_item.done":
		item, _ := object(f["item"])
		if t.target == wire.Anthropic || t.target == wire.Chat {
			if err := validateResponsesItem(item); err != nil {
				return err
			}
		}
		index, _ := position(f["output_index"])
		if err := t.responseToolSnapshot(index, item); err != nil {
			return err
		}
		return t.responseMessageSnapshot(index, item)
	case "response.completed", "response.incomplete":
		response, _ := object(f["response"])
		if t.target == wire.Anthropic || t.target == wire.Chat {
			if err := validateResponsesOutput(response); err != nil {
				return err
			}
		}
		for index, value := range arr(response["output"]) {
			item, _ := object(value)
			if err := t.responseToolSnapshot(index, item); err != nil {
				return err
			}
			if err := t.responseMessageSnapshot(index, item); err != nil {
				return err
			}
		}
		return nil
	case "response.refusal.delta":
		index, _ := position(f["output_index"])
		return t.responseRefusalDelta(index, str(f["delta"]))
	case "response.output_text.delta":
		index, _ := position(f["output_index"])
		return t.responseTextDelta(index, str(f["delta"]))
	case "response.output_item.added":
		item, _ := optionalObject(f["item"])
		if t.target == wire.Anthropic || t.target == wire.Chat {
			if err := validateResponsesItem(item); err != nil {
				return err
			}
		}
		if str(item["type"]) == "function_call" {
			index, _ := position(f["output_index"])
			return t.toolDelta(index, str(item["call_id"]), str(item["name"]), str(item["arguments"]))
		}
	case "response.function_call_arguments.delta":
		index, _ := position(f["output_index"])
		return t.toolDelta(index, "", "", str(f["delta"]))
	}
	return nil
}

func (t *streamTranslator) anthropicFrame(f Object) error {
	index, _ := position(f["index"])
	switch str(f["type"]) {
	case "message_start":
		m, _ := object(f["message"])
		return t.start(str(m["id"]))
	case "content_block_start":
		b, _ := object(f["content_block"])
		switch str(b["type"]) {
		case "text":
			return t.textDelta(str(b["text"]))
		case "tool_use":
			input := ""
			if value, err := object(b["input"]); err == nil && len(value) > 0 {
				input = string(b["input"])
			}
			return t.toolDelta(index, str(b["id"]), str(b["name"]), input)
		}
	case "content_block_delta":
		d, _ := object(f["delta"])
		switch str(d["type"]) {
		case "text_delta":
			return t.textDelta(str(d["text"]))
		case "input_json_delta":
			return t.toolDelta(index, "", "", str(d["partial_json"]))
		}
	}
	return nil
}

func (t *streamTranslator) geminiFrame(f Object) error {
	if e := t.start(str(f["responseId"])); e != nil {
		return e
	}
	for _, v := range arr(f["candidates"]) {
		candidate, _ := object(v)
		index, _ := position(candidate["index"])
		if index != 0 {
			return protocolError("translation requires one candidate")
		}
		content, _ := optionalObject(candidate["content"])
		for _, v := range arr(content["parts"]) {
			part, _ := object(v)
			var thought bool
			_ = json.Unmarshal(part["thought"], &thought)
			if !thought {
				if e := t.textDelta(str(part["text"])); e != nil {
					return e
				}
			}
			if present(part["functionCall"]) {
				call, _ := object(part["functionCall"])
				index := len(t.calls)
				id := str(call["id"])
				if id == "" {
					id = "call_" + strconv.Itoa(index)
				}
				if e := t.toolDelta(index, id, str(call["name"]), string(geminiArguments(call["args"]))); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
