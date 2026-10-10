package protocols

import (
	"bytes"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func CaptureStreamFrame(family openai.Family, frame []byte, text func(string), tool func(json.RawMessage)) {
	if family.Surface() == "bedrock" {
		captureBedrockFrame(frame, text, tool)
		return
	}
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		f, e := object(bytes.TrimSpace(line[5:]))
		if e != nil {
			continue
		}
		switch family.Surface() {
		case "anthropic":
			kind := str(f["type"])
			switch kind {
			case "content_block_start":
				if block, _ := object(f["content_block"]); str(block["type"]) == "tool_use" {
					tool(f["content_block"])
				}
			case "content_block_delta":
				delta, _ := object(f["delta"])
				switch str(delta["type"]) {
				case "text_delta":
					text(str(delta["text"]))
				case "input_json_delta":
					if present(delta["partial_json"]) {
						tool(f["delta"])
					}
				}
			}
		case "gemini":
			for _, v := range arr(f["candidates"]) {
				candidate, _ := object(v)
				content, _ := object(candidate["content"])
				for _, part := range arr(content["parts"]) {
					p, _ := object(part)
					var thought bool
					_ = json.Unmarshal(p["thought"], &thought)
					if thought {
						continue
					}
					if t := str(p["text"]); t != "" {
						text(t)
					}
					if present(p["functionCall"]) {
						tool(p["functionCall"])
					}
				}
			}
		case "native":
			if family == openai.FamilyCohereChat {
				delta, _ := object(f["delta"])
				message, _ := object(delta["message"])
				switch str(f["type"]) {
				case "content-delta":
					content, _ := object(message["content"])
					if t := str(content["text"]); t != "" {
						text(t)
					}
				case "tool-call-start", "tool-call-delta":
					if present(message["tool_calls"]) {
						tool(message["tool_calls"])
					}
				}
				continue
			}
			for _, v := range arr(f["choices"]) {
				choice, _ := object(v)
				delta, _ := object(choice["delta"])
				if t := str(delta["content"]); t != "" {
					text(t)
				}
				if t := str(delta["refusal"]); t != "" {
					text(t)
				}
				for _, call := range arr(delta["tool_calls"]) {
					tool(call)
				}
			}
		default:
			if family == openai.FamilyResponses {
				switch str(f["type"]) {
				case "response.output_text.delta", "response.refusal.delta":
					if t := str(f["delta"]); t != "" {
						text(t)
					}
				case "response.function_call_arguments.delta", "response.function_call_arguments.done":
					if present(f["delta"]) || present(f["arguments"]) {
						tool(bytes.TrimSpace(line[5:]))
					}
				case "response.output_item.added", "response.output_item.done":
					if item, _ := object(f["item"]); str(item["type"]) == "function_call" {
						tool(f["item"])
					}
				}
				continue
			}
			for _, v := range arr(f["choices"]) {
				choice, _ := object(v)
				delta, _ := object(choice["delta"])
				if t := str(delta["content"]); t != "" {
					text(t)
				}
				if t := str(delta["refusal"]); t != "" {
					text(t)
				}
				for _, call := range arr(delta["tool_calls"]) {
					tool(call)
				}
			}
		}
	}
}

func captureBedrockFrame(frame []byte, text func(string), tool func(json.RawMessage)) {
	message, err := ReadBedrockEvent(bytes.NewReader(frame), len(frame))
	if err != nil {
		return
	}
	header := func(name string) string {
		if v := message.Headers.Get(name); v != nil {
			return v.String()
		}
		return ""
	}
	if header(":message-type") != "event" {
		return
	}
	f, err := object(message.Payload)
	if err != nil {
		return
	}
	switch header(":event-type") {
	case "contentBlockStart":
		if start, e := optionalObject(f["start"]); e == nil {
			if use, e := optionalObject(start["toolUse"]); e == nil && use != nil {
				tool(start["toolUse"])
			}
		}
	case "contentBlockDelta":
		delta, _ := optionalObject(f["delta"])
		if t := str(delta["text"]); t != "" {
			text(t)
		}
		if use, e := optionalObject(delta["toolUse"]); e == nil && use != nil {
			if present(use["input"]) {
				tool(f["delta"])
			}
		}
	}
}

func CaptureDocumentToolCalls(family openai.Family, body []byte) []json.RawMessage {
	fields, err := object(body)
	if err != nil {
		return nil
	}
	var calls []json.RawMessage
	switch family.Surface() {
	case "anthropic":
		for _, block := range arr(fields["content"]) {
			if b, _ := object(block); str(b["type"]) == "tool_use" {
				calls = append(calls, block)
			}
		}
	case "gemini":
		for _, v := range arr(fields["candidates"]) {
			candidate, _ := object(v)
			content, _ := object(candidate["content"])
			for _, part := range arr(content["parts"]) {
				if p, _ := object(part); present(p["functionCall"]) {
					calls = append(calls, p["functionCall"])
				}
			}
		}
	case "bedrock":
		message, _ := optionalObject(fields["output"])
		if message != nil {
			msg, _ := optionalObject(message["message"])
			for _, block := range arr(msg["content"]) {
				if b, _ := object(block); present(b["toolUse"]) {
					calls = append(calls, b["toolUse"])
				}
			}
		}
	case "native":
		if family == openai.FamilyCohereChat {
			message, _ := object(fields["message"])
			for _, call := range arr(message["tool_calls"]) {
				calls = append(calls, call)
			}
			return calls
		}
		for _, v := range arr(fields["choices"]) {
			choice, _ := object(v)
			message, _ := object(choice["message"])
			for _, call := range arr(message["tool_calls"]) {
				calls = append(calls, call)
			}
		}
	default:
		if family == openai.FamilyResponses {
			for _, item := range arr(fields["output"]) {
				if i, _ := object(item); str(i["type"]) == "function_call" {
					calls = append(calls, item)
				}
			}
			return calls
		}
		for _, v := range arr(fields["choices"]) {
			choice, _ := object(v)
			message, _ := object(choice["message"])
			for _, call := range arr(message["tool_calls"]) {
				calls = append(calls, call)
			}
		}
	}
	return calls
}
