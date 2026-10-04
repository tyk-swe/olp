package protocols

import (
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// ValidateInlineMedia reads a request's parsed document and legacyValidateInlineMedia
// decoded json.RawMessage level by level. They are held to the same answer, the
// same error to the code, message and parameter or none, for any request: in the
// shapes clients send, with media of every kind and every dialect, and in any JSON
// at all.

var inlineMediaFamilies = []openai.Family{
	openai.FamilyChat, openai.FamilyResponses, openai.FamilyInputTokens, openai.FamilyAnthropic, openai.FamilyAnthropicCount,
	openai.FamilyGemini, openai.FamilyGeminiStream, openai.FamilyGeminiCount, openai.FamilyBedrock, openai.FamilyEmbeddings,
	openai.FamilyGeminiInteractions, openai.FamilyImageGeneration,
}

type mediaGen struct{ r *rand.Rand }

func (g mediaGen) intn(n int) int { return g.r.IntN(n) }

func (g mediaGen) pick(options ...string) string { return options[g.intn(len(options))] }

// payload is base64 text, mostly valid, and sometimes not.
func (g mediaGen) payload() string {
	raw := make([]byte, g.intn(12))
	for i := range raw {
		raw[i] = byte(g.intn(256))
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	switch g.intn(8) {
	case 0:
		return encoded + "!"
	case 1:
		return strings.TrimRight(encoded, "=")
	case 2:
		return ""
	}
	return encoded
}

func (g mediaGen) dataURL() string {
	switch g.intn(6) {
	case 0:
		return "https://example.test/cat.png"
	case 1:
		return "data:image/png," + g.payload()
	case 2:
		return "data:image/png;base64"
	case 3:
		return "text \"quoted\"\nline"
	}
	return "data:image/png;base64," + g.payload()
}

func (g mediaGen) wild(depth int) any {
	switch g.intn(8) {
	case 0:
		return g.dataURL()
	case 1:
		return g.intn(5)
	case 2:
		return nil
	case 3, 4:
		if depth <= 0 {
			return true
		}
		items := make([]any, g.intn(3))
		for i := range items {
			items[i] = g.wild(depth - 1)
		}
		return items
	case 5, 6:
		if depth <= 0 {
			return false
		}
		out := map[string]any{}
		for range g.intn(4) {
			out[g.pick("type", "content", "parts", "data", "url", "source", "image_url", "inlineData")] = g.wild(depth - 1)
		}
		return out
	}
	return g.payload()
}

// part is a content part of any dialect that may carry media, or something else.
func (g mediaGen) part(depth int) any {
	switch g.intn(16) {
	case 0:
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": g.dataURL()}}
	case 1:
		return map[string]any{"type": "image_url", "image_url": g.wild(1)}
	case 2:
		return map[string]any{"type": "input_image", "image_url": g.dataURL()}
	case 3:
		return map[string]any{"type": "input_audio", "input_audio": map[string]any{"format": "wav", "data": g.payload()}}
	case 4:
		return map[string]any{"type": "input_file", "file_data": g.dataURL()}
	case 5:
		return map[string]any{"type": "file", "file": map[string]any{"file_data": g.dataURL()}}
	case 6:
		return map[string]any{"type": g.pick("image", "document"), "source": map[string]any{"type": g.pick("base64", "url", "text"), "data": g.payload(), "media_type": "image/png"}}
	case 7:
		return map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": g.payload()}}
	case 8:
		return map[string]any{"inline_data": map[string]any{"mime_type": "image/png", "data": g.payload()}}
	case 9:
		if depth > 0 {
			return map[string]any{"type": "tool_result", "tool_use_id": "t", "content": g.parts(depth - 1)}
		}
	case 10:
		if depth > 0 {
			return map[string]any{"functionResponse": map[string]any{"name": "f", "parts": g.parts(depth - 1)}}
		}
	case 11:
		return g.wild(2)
	case 12:
		return map[string]any{"type": "text", "text": g.pick("data:image/png;base64,AAAA", "hello \"there\"\n")}
	}
	return map[string]any{"type": "text", "text": "hello"}
}

// nested wraps a part in tool results and function responses, as many as levels
// say, so that the depth a validation looks to is the depth that matters.
func (g mediaGen) nested(levels int, inner any) any {
	for range levels {
		if g.intn(2) == 0 {
			inner = map[string]any{"type": "tool_result", "tool_use_id": "t", "content": []any{inner}}
		} else {
			inner = map[string]any{"functionResponse": map[string]any{"name": "f", "parts": []any{inner}}}
		}
	}
	return inner
}

func (g mediaGen) parts(depth int) any {
	if g.intn(10) == 0 {
		return []any{g.nested(g.intn(5), g.part(0))}
	}
	if g.intn(8) == 0 {
		return g.pick("plain text", "data:image/png;base64,AAAA")
	}
	if g.intn(12) == 0 {
		return g.wild(2)
	}
	parts := make([]any, g.intn(4))
	for i := range parts {
		parts[i] = g.part(depth)
	}
	return parts
}

func (g mediaGen) messages(partKey string) any {
	if g.intn(15) == 0 {
		return g.wild(2)
	}
	messages := make([]any, g.intn(4))
	for i := range messages {
		if g.intn(12) == 0 {
			messages[i] = g.wild(2)
			continue
		}
		messages[i] = map[string]any{"role": "user", partKey: g.parts(3)}
	}
	return messages
}

func (g mediaGen) fields() map[string]json.RawMessage {
	raw := map[string]any{"model": "m"}
	if g.intn(2) == 0 {
		raw["messages"] = g.messages("content")
	}
	if g.intn(2) == 0 {
		raw["input"] = g.messages("content")
	}
	if g.intn(2) == 0 {
		raw["contents"] = g.messages("parts")
	}
	switch g.intn(4) {
	case 0:
		raw["generateContentRequest"] = map[string]any{"contents": g.messages("parts")}
	case 1:
		raw["generateContentRequest"] = g.wild(2)
	}
	fields := make(map[string]json.RawMessage, len(raw))
	for name, v := range raw {
		fields[name], _ = json.Marshal(v)
	}
	return fields
}

func (g mediaGen) limits() InlineMediaLimits {
	if g.intn(4) == 0 {
		return InlineMediaLimits{Items: 100, ItemBytes: 1 << 20, TotalBytes: 1 << 20}
	}
	return InlineMediaLimits{Items: g.intn(4), ItemBytes: int64(g.intn(10)), TotalBytes: int64(g.intn(20))}
}

func sameMediaAnswer(t testing.TB, request *openai.Request, limits InlineMediaLimits, body any) (accepted bool) {
	t.Helper()
	got, want := ValidateInlineMedia(request, limits), legacyValidateInlineMedia(request, limits)
	if !reflect.DeepEqual(got, want) {
		shown, ok := body.(json.RawMessage)
		if !ok {
			shown, _ = json.Marshal(body)
		}
		t.Fatalf("%s with %+v\nrequest %.1500s\nanswer  %v\nlegacy  %v", request.Family, limits, shown, got, want)
	}
	return got == nil
}

func TestInlineMediaIsValidatedAsTheLegacyValidationDid(t *testing.T) {
	cases := 400
	if testing.Short() {
		cases = 80
	}
	var accepted, refused int
	for _, family := range inlineMediaFamilies {
		g := mediaGen{rand.New(rand.NewPCG(41, uint64(len(family))))}
		for range cases {
			fields := g.fields()
			request := openai.NewEnvelope(family, "route", false, fields)
			if sameMediaAnswer(t, request, g.limits(), fields) {
				accepted++
			} else {
				refused++
			}
		}
	}
	// The comparison is only worth something if both answers are given.
	if accepted < cases || refused < cases {
		t.Errorf("%d requests were accepted and %d refused: the generator is not exercising the validation", accepted, refused)
	}
}

func FuzzInlineMediaIsValidatedAsTheLegacyValidationDid(f *testing.F) {
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YWI="}},{"type":"input_audio","input_audio":{"data":"YWJj"}}]}]}`), uint8(2))
	f.Add([]byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,YWI="},"bare"]}]}`), uint8(5))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","data":"YWI="}}]}]}]}`), uint8(9))
	f.Add([]byte(`{"generateContentRequest":{"contents":[{"parts":[{"inlineData":{"data":"YWI="}},{"functionResponse":{"parts":[{"inline_data":{"data":"!"}}]}}]}]}}`), uint8(1))
	f.Add([]byte(`{"contents":[{"parts":"text"},null,[1],{"parts":[{"inlineData":null}]}]}`), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, which uint8) {
		doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 16 << 10, MaxNodes: 1024, MaxDepth: 32})
		if err != nil {
			return
		}
		limits := []InlineMediaLimits{{Items: 1, ItemBytes: 2, TotalBytes: 2}, {Items: 100, ItemBytes: 1 << 20, TotalBytes: 1 << 20}, {}}[int(which)%3]
		for _, family := range inlineMediaFamilies {
			sameMediaAnswer(t, openai.NewSourceEnvelope(family, "route", false, doc), limits, json.RawMessage(data))
		}
	})
}
