package estimate

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// This file is the walker as it read a request when every field of it was
// json.RawMessage, decoded level by level, before the walker read the request's
// parsed document instead. It is the reference the walker that replaced it is
// held to, field for field, by walk_reference_parity_test.go: the two must find
// the same text, roles, charges and flags in any request, whatever its shape.
// Nothing outside the tests calls it.

// refWalker reads the prompt of one request into an input, as the walker did.
type refWalker struct {
	in *input
}

// refWalkInput reads the prompt fields of a request of the given family, asking
// field for each one by name.
func refWalkInput(in *input, family openai.Family, field func(string) json.RawMessage) {
	w := refWalker{in}
	switch family {
	case openai.FamilyChat:
		w.items(field("messages"))
	case openai.FamilyEmbeddings:
		w.embedding(field("input"))
	case openai.FamilyResponses, openai.FamilyInputTokens, openai.FamilyModeration:
		w.items(field("input"))
		w.instructions(field("instructions"))
	}
	if family.Surface() != "openai" {
		w.dialectMessages(field("messages"))
		w.dialectSystem(field("system"))
		w.dialectMessages(field("contents"))
		w.dialectSystem(field("systemInstruction"))
		w.dialectRequest(field("generateContentRequest"))
		w.schema(field("tools"))
		if family.Surface() == "bedrock" {
			// Converse holds its tool catalogue beside the conversation, where
			// the other dialects hold it in tools; Gemini's toolConfig only
			// chooses among the tools it already holds.
			w.schema(field("toolConfig"))
		}
		w.structured(family, field)
		return
	}
	w.tools(field("tools"))
	w.structured(family, field)
}

// structured walks the schema a request asks the reply to follow: a model reads
// it as prompt text, as it reads a tool's. Each dialect has its own place for
// one, and a request without one costs nothing to look.
func (w refWalker) structured(family openai.Family, field func(string) json.RawMessage) {
	switch family {
	case openai.FamilyChat:
		w.schema(jsonObject(jsonObject(field("response_format"))["json_schema"])["schema"])
	case openai.FamilyResponses, openai.FamilyInputTokens:
		w.schema(jsonObject(jsonObject(field("text"))["format"])["schema"])
	}
	switch family.Surface() {
	case "anthropic":
		w.schema(jsonObject(jsonObject(field("output_config"))["format"])["schema"])
		w.schema(jsonObject(field("output_format"))["schema"])
	case "gemini":
		w.generationConfig(field("generationConfig"))
	case "bedrock":
		// Converse names the schema as a string of JSON, which is its text.
		schema := jsonObject(jsonObject(jsonObject(jsonObject(field("outputConfig"))["textFormat"])["structure"])["jsonSchema"])["schema"]
		if !w.toolText(schema) {
			w.schema(schema)
		}
	}
}

// generationConfig walks the response schema of a Gemini request, in either of
// the fields that carry one.
func (w refWalker) generationConfig(raw json.RawMessage) {
	config := jsonObject(raw)
	for _, member := range responseSchemas {
		w.schema(config[member])
	}
}

// text adds a JSON string as one segment, reporting whether the value was a
// string. Characters are counted rather than bytes so a multi-byte script is
// not overcharged.
func (w refWalker) text(raw json.RawMessage) bool {
	text, ok := textOf(raw)
	if ok {
		w.in.addText(text, false)
	}
	return ok
}

// toolText adds text that belongs to a tool call or its result. It counts as
// text, but the model reads it inside a rendering of the call that no provider
// documents, which the count does not include.
func (w refWalker) toolText(raw json.RawMessage) bool {
	ok := w.text(raw)
	if ok {
		w.in.approx = true
	}
	return ok
}

// items walks the conversation one request carries: the messages of a chat
// request, or the input of a responses request, which may also be one plain
// string. A shape this gateway does not recognise is charged nothing rather
// than guessed at.
func (w refWalker) items(raw json.RawMessage) {
	if w.text(raw) {
		// A plain string is what a caller who sent no messages means by one.
		w.in.messages++
		w.in.roles = append(w.in.roles, "user")
		return
	}
	for _, raw := range jsonArray(raw) {
		item := jsonObject(raw)
		if item == nil {
			continue
		}
		w.message(item)
		if string(item["type"]) == `"reasoning"` {
			w.reasoning(item)
		}
		// A responses tool result carries what it returned in `output`.
		w.content(item["content"])
		w.content(item["output"])
		// The identifiers and arguments a message travels with are prompt text
		// like any other; `call_id` and `arguments` are the responses spelling
		// of the chat tool-call fields.
		w.text(item["name"])
		for _, name := range [...]string{"tool_call_id", "call_id", "arguments"} {
			w.toolText(item[name])
		}
		for _, raw := range jsonArray(item["tool_calls"]) {
			call := jsonObject(raw)
			if function := jsonObject(call["function"]); function != nil {
				call = function
			}
			w.toolText(call["name"])
			w.toolText(call["arguments"])
		}
	}
}

// reasoning walks what a reasoning item of a responses conversation says that a
// model reads again: the summary it wrote. Its reasoning text is a content part,
// read with the item's content. What else it carries, the encrypted content, is
// the rest of the reasoning in a form no count reads, and is charged nothing, so
// the count is a guess.
func (w refWalker) reasoning(item map[string]json.RawMessage) {
	for _, raw := range jsonArray(item["summary"]) {
		w.toolText(jsonObject(raw)["text"])
	}
	w.in.approx = true
}

// message records the framing of one message: it exists, it has a role, and
// possibly a name.
func (w refWalker) message(item map[string]json.RawMessage) {
	w.in.messages++
	if role, ok := refRoleOf(item["role"]); ok {
		w.in.roles = append(w.in.roles, role)
	}
	if _, ok := textOf(item["name"]); ok {
		w.in.names++
	}
}

// refRoleOf reads a message role. The usual ones are answered without decoding,
// since a request carries a role for every message.
func refRoleOf(raw json.RawMessage) (string, bool) {
	switch string(raw) {
	case `"user"`:
		return "user", true
	case `"assistant"`:
		return "assistant", true
	case `"system"`:
		return "system", true
	case `"tool"`:
		return "tool", true
	case `"developer"`:
		return "developer", true
	}
	return textOf(raw)
}

// instructions walks the instructions of a responses request, which a model
// reads as a message of their own.
func (w refWalker) instructions(raw json.RawMessage) {
	if text, ok := textOf(raw); ok && text != "" {
		w.in.addText(text, false)
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// content walks one message body: plain text, or the parts a multimodal
// message carries.
func (w refWalker) content(raw json.RawMessage) {
	if w.text(raw) {
		return
	}
	for _, raw := range jsonArray(raw) {
		part := jsonObject(raw)
		if part == nil {
			// A bare string among the parts is text.
			w.text(raw)
			continue
		}
		kind, _ := textOf(part["type"])
		switch kind {
		case "image_url", "input_image":
			w.in.addMedia(ImageTokens)
		case "input_audio", "input_file", "file":
			w.in.addMedia(MediaTokens)
		default:
			w.text(part["text"])
			w.text(part["refusal"])
		}
	}
}

// tools walks the tool catalogue a request carries: a schema the model has to
// read costs what any other prompt text costs.
func (w refWalker) tools(raw json.RawMessage) {
	for _, raw := range jsonArray(raw) {
		tool := jsonObject(raw)
		// Chat nests the definition under `function`; responses holds it flat.
		if function := jsonObject(tool["function"]); function != nil {
			tool = function
		}
		w.toolText(tool["name"])
		w.toolText(tool["description"])
		w.schema(tool["parameters"])
	}
}

// schema adds a JSON document as the text it is, with whatever formatting the
// caller happened to send removed.
func (w refWalker) schema(raw json.RawMessage) {
	if text := SchemaText(raw); text != "" {
		w.in.addText(text, false)
		w.in.approx = true
	}
}

// embedding walks the input of an embeddings request: texts, or the token ids
// a caller already has, which are one token each.
func (w refWalker) embedding(raw json.RawMessage) {
	if w.text(raw) {
		return
	}
	for _, item := range jsonArray(raw) {
		var token uint32
		if json.Unmarshal(item, &token) == nil {
			w.in.addFlat(1)
		} else {
			w.embedding(item)
		}
	}
}

// dialectMessages walks the conversation of a native Anthropic, Gemini or
// Bedrock request, whose entries are messages in a shape of their own.
func (w refWalker) dialectMessages(raw json.RawMessage) {
	w.dialect(raw)
	for _, raw := range jsonArray(raw) {
		if item := jsonObject(raw); item != nil {
			w.in.messages++
			if role, ok := refRoleOf(item["role"]); ok {
				w.in.roles = append(w.in.roles, role)
			}
		}
	}
}

// dialectSystem walks a system prompt, which a model reads as a message.
func (w refWalker) dialectSystem(raw json.RawMessage) {
	w.dialect(raw)
	if len(raw) > 0 && string(raw) != "null" {
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// dialectRequest walks the request a Gemini count wraps.
func (w refWalker) dialectRequest(raw json.RawMessage) {
	w.dialect(raw)
	inner := jsonObject(raw)
	w.schema(inner["tools"])
	w.generationConfig(inner["generationConfig"])
	for _, raw := range jsonArray(inner["contents"]) {
		if item := jsonObject(raw); item != nil {
			w.in.messages++
			if role, ok := refRoleOf(item["role"]); ok {
				w.in.roles = append(w.in.roles, role)
			}
		}
	}
	if raw := inner["systemInstruction"]; len(raw) > 0 && string(raw) != "null" {
		w.in.messages++
		w.in.roles = append(w.in.roles, "system")
	}
}

// dialect charges native content the same text and media units as OpenAI's.
// Blob bytes are never mistaken for text tokens.
func (w refWalker) dialect(raw json.RawMessage) {
	if len(raw) == 0 || w.text(raw) {
		return
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		for _, item := range items {
			w.dialect(item)
		}
		return
	}
	f := jsonObject(raw)
	if f == nil {
		return
	}
	if f["inlineData"] != nil || f["fileData"] != nil || string(f["type"]) == `"image"` {
		w.in.addMedia(ImageTokens)
		return
	}
	switch string(f["type"]) {
	case `"thinking"`:
		// The text the model reasoned in, which it reads again in a tool loop.
		w.toolText(f["thinking"])
		w.in.approx = true
		return
	case `"redacted_thinking"`:
		// Its reasoning, encrypted: nothing a count can read.
		w.in.approx = true
		return
	case `"document"`:
		w.document(f)
		return
	}
	// A tool call or its result, whichever keys carry its text.
	if kind := string(f["type"]); kind == `"tool_use"` || kind == `"tool_result"` {
		w.in.approx = true
	}
	for _, key := range [...]string{"text", "content", "parts", "contents", "systemInstruction", "functionResponse", "functionCall", "input", "output"} {
		if value := f[key]; value != nil {
			switch key {
			case "functionResponse", "functionCall", "input", "output":
				w.in.approx = true
			}
			w.dialect(value)
		}
	}
}

// document walks an Anthropic document block: the text of one given as text, or
// as content blocks, and for any other source, a PDF or a file as base64 or a
// URL, the flat charge of a media part, whatever the bytes it carries.
func (w refWalker) document(block map[string]json.RawMessage) {
	source := jsonObject(block["source"])
	switch string(source["type"]) {
	case `"text"`:
		w.toolText(source["data"])
	case `"content"`:
		w.dialect(source["content"])
	default:
		w.in.addMedia(MediaTokens)
		return
	}
	w.in.approx = true
}
