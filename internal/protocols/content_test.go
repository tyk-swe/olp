package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func parse(t *testing.T, family wire.Format, body string) *wire.Request {
	t.Helper()
	r, err := Parse(family, []byte(body), "route")
	if err != nil {
		t.Fatalf("Parse(%s): %v", family, err)
	}
	return r
}

func inspectInputText(t *testing.T, r *wire.Request, fn TextSlot) *wire.Request {
	t.Helper()
	out, err := InspectInputText(r, fn)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func redactSecret(text string) (string, bool) {
	return strings.ReplaceAll(text, "s3cr3t", "[REDACTED]"), false
}

func blockOnForbid(text string) (string, bool) {
	return text, strings.Contains(text, "forbidden")
}

func fieldText(t *testing.T, r *wire.Request, path ...string) string {
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

func outputText(t *testing.T, family wire.Format, body string) []byte {
	t.Helper()
	out, err := InspectOutputText(family, []byte(body), redactSecret)
	if err != nil {
		t.Fatalf("InspectOutputText: %v", err)
	}
	return out
}

func TestInspectOutputTextGemini(t *testing.T) {
	out := outputText(t, wire.Gemini, `{
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

func TestInspectOutputTextCompletions(t *testing.T) {
	body := `{"choices":[{"index":0,"text":"first s3cr3t"},{"index":1,"text":"second s3cr3t"}],"usage":{"total_tokens":7}}`
	out := outputText(t, wire.Completions, body)
	if strings.Contains(string(out), "s3cr3t") || strings.Count(string(out), "[REDACTED]") != 2 || !strings.Contains(string(out), `"total_tokens":7`) {
		t.Fatalf("completion inspection: %s", out)
	}
	seen := false
	if _, err := InspectOutputText(wire.Completions, []byte(body), func(text string) (string, bool) { seen = seen || strings.Contains(text, "s3cr3t"); return text, seen }); err != nil || !seen {
		t.Fatalf("completion block surface missing: %v", err)
	}
}
