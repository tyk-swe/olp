package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func parse(t *testing.T, family openai.Family, body string) *openai.Request {
	t.Helper()
	r, err := Parse(family, []byte(body), "route")
	if err != nil {
		t.Fatalf("Parse(%s): %v", family, err)
	}
	return r
}

func redactSecret(text string) (string, bool) {
	return strings.ReplaceAll(text, "s3cr3t", "[REDACTED]"), false
}

func blockOnForbid(text string) (string, bool) {
	return text, strings.Contains(text, "forbidden")
}

func fieldText(t *testing.T, r *openai.Request, path ...string) string {
	t.Helper()
	var node json.RawMessage = r.Field(path[0])
	for _, name := range path[1:] {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(node, &obj); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		node = obj[name]
	}
	var value string
	if err := json.Unmarshal(node, &value); err != nil {
		t.Fatalf("text at %v: %v", path, err)
	}
	return value
}

func TestInspectInputTextChat(t *testing.T) {
	r := parse(t, openai.FamilyChat, `{
		"model":"route",
		"messages":[
			{"role":"system","content":"keep s3cr3t safe"},
			{"role":"user","content":[{"type":"text","text":"say s3cr3t"},{"type":"image_url","image_url":{"url":"https://img/s3cr3t.png"}}]},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"q\":\"s3cr3t\"}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"result s3cr3t"}
		]}`)
	out := InspectInputText(r, redactSecret)
	doc := out.Document()
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(doc["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	var system string
	json.Unmarshal(messages[0]["content"], &system)
	if system != "keep [REDACTED] safe" {
		t.Fatalf("system=%q", system)
	}
	var parts []map[string]json.RawMessage
	json.Unmarshal(messages[1]["content"], &parts)
	var partText string
	json.Unmarshal(parts[0]["text"], &partText)
	if partText != "say [REDACTED]" {
		t.Fatalf("part=%q", partText)
	}
	var imageURL map[string]string
	json.Unmarshal(parts[1]["image_url"], &imageURL)
	if imageURL["url"] != "https://img/s3cr3t.png" {
		t.Fatalf("image url inspected: %v", imageURL)
	}
	var calls []map[string]json.RawMessage
	json.Unmarshal(messages[2]["tool_calls"], &calls)
	var fn map[string]json.RawMessage
	json.Unmarshal(calls[0]["function"], &fn)
	var args string
	json.Unmarshal(fn["arguments"], &args)
	if args != `{"q":"[REDACTED]"}` {
		t.Fatalf("arguments=%q", args)
	}
	var tool string
	json.Unmarshal(messages[3]["content"], &tool)
	if tool != "result [REDACTED]" {
		t.Fatalf("tool=%q", tool)
	}
	original := r.Document()
	if !strings.Contains(string(original["messages"]), "s3cr3t") {
		t.Fatal("original request was mutated")
	}
}

func TestInspectInputTextChatBlockStopsWalk(t *testing.T) {
	r := parse(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"forbidden"}]}`)
	out := InspectInputText(r, blockOnForbid)
	if out == nil {
		t.Fatal("nil envelope")
	}
}

func TestInspectInputTextResponses(t *testing.T) {
	r := parse(t, openai.FamilyResponses, `{
		"model":"route",
		"instructions":"guard s3cr3t",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"tell s3cr3t"}]},
			{"type":"function_call","name":"f","arguments":"{\"x\":\"s3cr3t\"}"},
			{"type":"function_call_output","output":"ran s3cr3t"}
		]}`)
	out := InspectInputText(r, redactSecret)
	doc := out.Document()
	if got := fieldText(t, out, "instructions"); got != "guard [REDACTED]" {
		t.Fatalf("instructions=%q", got)
	}
	var items []map[string]json.RawMessage
	json.Unmarshal(doc["input"], &items)
	var content []map[string]json.RawMessage
	json.Unmarshal(items[0]["content"], &content)
	var text string
	json.Unmarshal(content[0]["text"], &text)
	if text != "tell [REDACTED]" {
		t.Fatalf("input text=%q", text)
	}
	var arguments string
	json.Unmarshal(items[1]["arguments"], &arguments)
	if arguments != `{"x":"[REDACTED]"}` {
		t.Fatalf("arguments=%q", arguments)
	}
	var output string
	json.Unmarshal(items[2]["output"], &output)
	if output != "ran [REDACTED]" {
		t.Fatalf("output=%q", output)
	}
}

func TestInspectInputTextResponsesString(t *testing.T) {
	r := parse(t, openai.FamilyResponses, `{"model":"route","input":"raw s3cr3t"}`)
	out := InspectInputText(r, redactSecret)
	if got := fieldText(t, out, "input"); got != "raw [REDACTED]" {
		t.Fatalf("input=%q", got)
	}
}

func TestInspectInputTextEmbeddingsAndModeration(t *testing.T) {
	single := parse(t, openai.FamilyEmbeddings, `{"model":"route","input":"embed s3cr3t"}`)
	if got := fieldText(t, InspectInputText(single, redactSecret), "input"); got != "embed [REDACTED]" {
		t.Fatalf("embed input=%q", got)
	}
	list := parse(t, openai.FamilyEmbeddings, `{"model":"route","input":["one s3cr3t","two"]}`)
	var inputs []string
	json.Unmarshal(InspectInputText(list, redactSecret).Field("input"), &inputs)
	if inputs[0] != "one [REDACTED]" || inputs[1] != "two" {
		t.Fatalf("inputs=%v", inputs)
	}
	moderation := parse(t, openai.FamilyModeration, `{"model":"route","input":"mod s3cr3t"}`)
	if got := fieldText(t, InspectInputText(moderation, redactSecret), "input"); got != "mod [REDACTED]" {
		t.Fatalf("moderation input=%q", got)
	}
}

func TestInspectInputTextRerank(t *testing.T) {
	r := parse(t, openai.FamilyRerank, `{"model":"route","query":"find s3cr3t","documents":["doc s3cr3t","clean"]}`)
	out := InspectInputText(r, redactSecret)
	if got := fieldText(t, out, "query"); got != "find [REDACTED]" {
		t.Fatalf("query=%q", got)
	}
	var docs []string
	json.Unmarshal(out.Field("documents"), &docs)
	if docs[0] != "doc [REDACTED]" || docs[1] != "clean" {
		t.Fatalf("documents=%v", docs)
	}
}

func TestInspectInputTextAnthropic(t *testing.T) {
	r := parse(t, openai.FamilyAnthropic, `{
		"model":"route","max_tokens":16,
		"system":[{"type":"text","text":"sys s3cr3t"}],
		"messages":[
			{"role":"user","content":"user s3cr3t"},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"res s3cr3t"}]}]}
		]}`)
	out := InspectInputText(r, redactSecret)
	doc := out.Document()
	var system []map[string]json.RawMessage
	json.Unmarshal(doc["system"], &system)
	var sysText string
	json.Unmarshal(system[0]["text"], &sysText)
	if sysText != "sys [REDACTED]" {
		t.Fatalf("system=%q", sysText)
	}
	var messages []map[string]json.RawMessage
	json.Unmarshal(doc["messages"], &messages)
	var user string
	json.Unmarshal(messages[0]["content"], &user)
	if user != "user [REDACTED]" {
		t.Fatalf("user=%q", user)
	}
	var parts []map[string]json.RawMessage
	json.Unmarshal(messages[1]["content"], &parts)
	var nested []map[string]json.RawMessage
	json.Unmarshal(parts[0]["content"], &nested)
	var resText string
	json.Unmarshal(nested[0]["text"], &resText)
	if resText != "res [REDACTED]" {
		t.Fatalf("tool_result=%q", resText)
	}
}

func TestInspectInputTextGemini(t *testing.T) {
	r := parse(t, openai.FamilyGemini, `{
		"systemInstruction":{"parts":[{"text":"sys s3cr3t"}]},
		"contents":[{"role":"user","parts":[{"text":"hi s3cr3t"},{"inlineData":{"mimeType":"image/png","data":"s3cr3t"}}]}]}`)
	out := InspectInputText(r, redactSecret)
	doc := out.Document()
	var contents []map[string]json.RawMessage
	json.Unmarshal(doc["contents"], &contents)
	var parts []map[string]json.RawMessage
	json.Unmarshal(contents[0]["parts"], &parts)
	var text string
	json.Unmarshal(parts[0]["text"], &text)
	if text != "hi [REDACTED]" {
		t.Fatalf("part=%q", text)
	}
	var inline map[string]string
	json.Unmarshal(parts[1]["inlineData"], &inline)
	if inline["data"] != "s3cr3t" {
		t.Fatalf("binary inspected: %v", inline)
	}
	var sys map[string]json.RawMessage
	json.Unmarshal(doc["systemInstruction"], &sys)
	var sysParts []map[string]json.RawMessage
	json.Unmarshal(sys["parts"], &sysParts)
	var sysText string
	json.Unmarshal(sysParts[0]["text"], &sysText)
	if sysText != "sys [REDACTED]" {
		t.Fatalf("systemInstruction=%q", sysText)
	}
}

func outputText(t *testing.T, family openai.Family, body string) []byte {
	t.Helper()
	out, err := InspectOutputText(family, []byte(body), redactSecret)
	if err != nil {
		t.Fatalf("InspectOutputText: %v", err)
	}
	return out
}

func TestInspectOutputTextChat(t *testing.T) {
	out := outputText(t, openai.FamilyChat, `{
		"id":"chatcmpl-1","object":"chat.completion",
		"choices":[{"index":0,"message":{"role":"assistant","content":"the s3cr3t is safe","refusal":null,"reasoning_content":"s3cr3t thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"q\":\"s3cr3t\"}"}}]}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	var doc map[string]json.RawMessage
	json.Unmarshal(out, &doc)
	var choices []map[string]json.RawMessage
	json.Unmarshal(doc["choices"], &choices)
	var message map[string]json.RawMessage
	json.Unmarshal(choices[0]["message"], &message)
	var content string
	json.Unmarshal(message["content"], &content)
	if content != "the [REDACTED] is safe" {
		t.Fatalf("content=%q", content)
	}
	var reasoning string
	json.Unmarshal(message["reasoning_content"], &reasoning)
	if reasoning != "s3cr3t thinking" {
		t.Fatalf("reasoning inspected: %q", reasoning)
	}
	var calls []map[string]json.RawMessage
	json.Unmarshal(message["tool_calls"], &calls)
	var fn map[string]json.RawMessage
	json.Unmarshal(calls[0]["function"], &fn)
	var args string
	json.Unmarshal(fn["arguments"], &args)
	if args != `{"q":"s3cr3t"}` {
		t.Fatalf("tool arguments inspected: %q", args)
	}
	var usage map[string]int
	json.Unmarshal(doc["usage"], &usage)
	if usage["total_tokens"] != 2 {
		t.Fatalf("usage rewritten: %v", usage)
	}
}

func TestInspectOutputTextResponses(t *testing.T) {
	out := outputText(t, openai.FamilyResponses, `{
		"id":"resp-1","object":"response","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"s3cr3t chain"}],"content":[{"type":"reasoning_text","text":"s3cr3t hidden"}]},
			{"type":"function_call","name":"f","arguments":"{\"x\":\"s3cr3t\"}"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final s3cr3t"}]}
		]}`)
	var doc map[string]json.RawMessage
	json.Unmarshal(out, &doc)
	var items []map[string]json.RawMessage
	json.Unmarshal(doc["output"], &items)
	var summary []map[string]json.RawMessage
	json.Unmarshal(items[0]["summary"], &summary)
	var summaryText string
	json.Unmarshal(summary[0]["text"], &summaryText)
	if summaryText != "s3cr3t chain" {
		t.Fatalf("reasoning inspected: %q", summaryText)
	}
	var reasoningContent []map[string]json.RawMessage
	json.Unmarshal(items[0]["content"], &reasoningContent)
	var hidden string
	json.Unmarshal(reasoningContent[0]["text"], &hidden)
	if hidden != "s3cr3t hidden" {
		t.Fatalf("reasoning content inspected: %q", hidden)
	}
	var arguments string
	json.Unmarshal(items[1]["arguments"], &arguments)
	if arguments != `{"x":"s3cr3t"}` {
		t.Fatalf("tool arguments inspected: %q", arguments)
	}
	var content []map[string]json.RawMessage
	json.Unmarshal(items[2]["content"], &content)
	var text string
	json.Unmarshal(content[0]["text"], &text)
	if text != "final [REDACTED]" {
		t.Fatalf("output text=%q", text)
	}
}

func TestInspectOutputTextAnthropic(t *testing.T) {
	out := outputText(t, openai.FamilyAnthropic, `{
		"id":"msg-1","type":"message","role":"assistant",
		"content":[{"type":"thinking","thinking":"s3cr3t chain"},{"type":"text","text":"the s3cr3t"}],
		"stop_reason":"end_turn"}`)
	var doc map[string]json.RawMessage
	json.Unmarshal(out, &doc)
	var parts []map[string]json.RawMessage
	json.Unmarshal(doc["content"], &parts)
	var thinking string
	json.Unmarshal(parts[0]["thinking"], &thinking)
	if thinking != "s3cr3t chain" {
		t.Fatalf("thinking inspected: %q", thinking)
	}
	var text string
	json.Unmarshal(parts[1]["text"], &text)
	if text != "the [REDACTED]" {
		t.Fatalf("text=%q", text)
	}
}

func TestInspectOutputTextGemini(t *testing.T) {
	out := outputText(t, openai.FamilyGemini, `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"gem s3cr3t"},{"thought":true,"text":"s3cr3t chain"}]}}],
		"usageMetadata":{"totalTokenCount":2}}`)
	var doc map[string]json.RawMessage
	json.Unmarshal(out, &doc)
	var candidates []map[string]json.RawMessage
	json.Unmarshal(doc["candidates"], &candidates)
	var content map[string]json.RawMessage
	json.Unmarshal(candidates[0]["content"], &content)
	var parts []map[string]json.RawMessage
	json.Unmarshal(content["parts"], &parts)
	var text string
	json.Unmarshal(parts[0]["text"], &text)
	if text != "gem [REDACTED]" {
		t.Fatalf("text=%q", text)
	}
	var thoughtText string
	json.Unmarshal(parts[1]["text"], &thoughtText)
	if thoughtText != "s3cr3t chain" {
		t.Fatalf("reasoning part inspected: %q", thoughtText)
	}
}

func TestInspectOutputTextNonObjectAndUnknownFamily(t *testing.T) {
	if out, err := InspectOutputText(openai.FamilyChat, []byte(`"str"`), redactSecret); err == nil || string(out) != `"str"` {
		t.Fatalf("non-object: %v %s", err, out)
	}
	body := []byte(`{"data":[{"embedding":[0.1]}]}`)
	out, err := InspectOutputText(openai.FamilyEmbeddings, body, redactSecret)
	if err != nil || string(out) != string(body) {
		t.Fatalf("embeddings body rewritten: %v %s", err, out)
	}
}

func TestInspectOutputTextStop(t *testing.T) {
	body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"forbidden"}}]}`)
	out, err := InspectOutputText(openai.FamilyChat, body, blockOnForbid)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatal("blocked walk rewrote the body")
	}
}
