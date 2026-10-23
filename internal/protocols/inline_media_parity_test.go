package protocols

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

// ValidateInlineMedia reads a request's parsed document and legacyValidateInlineMedia
// decoded json.RawMessage level by level. They are held to the same answer, the
// same error to the code, message and parameter or none, for any request: in the
// shapes clients send, with media of every kind and every dialect, and in any JSON
// at all.

var inlineMediaFamilies = []wire.Format{
	wire.Chat, wire.Responses, wire.InputTokens, wire.Anthropic, wire.AnthropicCount,
	wire.Gemini, wire.GeminiStream, wire.GeminiCount, wire.Bedrock, wire.Embeddings,
	wire.GeminiInteractions, wire.ImageGeneration,
}

func sameMediaAnswer(t testing.TB, request *wire.Request, limits InlineMediaLimits, body any) (accepted bool) {
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
			request, err := wire.NewRequest(family, "route", false, doc)
			if err != nil {
				t.Fatal(err)
			}
			sameMediaAnswer(t, request, limits, json.RawMessage(data))
		}
	})
}
