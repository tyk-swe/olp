package protocols

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestInlineMediaBoundsAcrossNativeFamilies(t *testing.T) {
	for i, tc := range []struct {
		family wire.Format
		body   string
	}{
		{wire.Chat, `{"model":"test","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"format":"wav","data":"%s"}}]}]}`},
		{wire.Responses, `{"model":"test","input":[{"role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,%s"}]}]}`},
		{wire.Anthropic, `{"model":"test","max_tokens":16,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"%s"}}]}]}`},
		{wire.Chat, `{"model":"test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`},
		{wire.Responses, `{"model":"test","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`},
		{wire.Anthropic, `{"model":"test","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}]}]}`},
		{wire.Gemini, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"%s"}}]}]}`},
		{wire.GeminiStream, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"%s"}}]}]}`},
		{wire.GeminiCount, `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"%s"}}]}]}}`},
		{wire.Anthropic, `{"model":"test","max_tokens":16,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%s"}}]}]}]}`},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.family, i), func(t *testing.T) {
			for _, data := range []string{"YWI=", "YWJj", "invalid!"} {
				r, err := Parse(tc.family, []byte(fmt.Sprintf(tc.body, data)), "test")
				if err != nil {
					t.Fatal(err)
				}
				err = ValidateInlineMedia(r, InlineMediaLimits{Items: 1, ItemBytes: 2, TotalBytes: 2})
				if (err != nil) != (data != "YWI=") {
					t.Fatalf("%q: %v", data, err)
				}
			}
		})
	}
	body := `{"model":"test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YWI="}},{"type":"image_url","image_url":{"url":"data:image/png;base64,YWI="}}]}]}`
	r, err := Parse(wire.Chat, []byte(body), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []InlineMediaLimits{{1, 2, 4}, {2, 2, 3}} {
		if ValidateInlineMedia(r, limit) == nil {
			t.Errorf("accepted excess media for %+v", limit)
		}
	}
	text, _ := Parse(wire.Chat, []byte(`{"model":"test","messages":[{"role":"user","content":"`+strings.Repeat("data:image/png;base64,", 20)+`"}]}`), "")
	if err := ValidateInlineMedia(text, InlineMediaLimits{1, 2, 2}); err != nil {
		t.Fatalf("prompt text counted as media: %v", err)
	}
}
