package protocols

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestValidateInlineMediaEnforcesDecodedLimits(t *testing.T) {
	const responsesBody = `{
		"model":"route",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_file","filename":"sample.pdf","file_data":"data:application/pdf;base64,YWI="},
			{"type":"input_image","image_url":"data:image/png;base64,YWJj"}
		]}]
	}`
	for _, tc := range []struct {
		name   string
		family wire.Format
		body   string
	}{
		{"Chat images and audio across messages", wire.Chat, `{
			"model":"route",
			"messages":[
				{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YWI="}}]},
				{"role":"user","content":[{"type":"input_audio","input_audio":{"format":"wav","data":"YWJj"}}]}
			]
		}`},
		{"Responses files and images", wire.Responses, responsesBody},
		{"Responses input tokens with files and images", wire.InputTokens, responsesBody},
		{"Anthropic nested tool results", wire.Anthropic, `{
			"model":"route","max_tokens":16,
			"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWI="}},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}
			]}]}]
		}`},
		{"Gemini wrapped count and function response", wire.GeminiCount, `{
			"generateContentRequest":{"contents":[{"role":"user","parts":[
				{"inlineData":{"mimeType":"image/png","data":"YWI="}},
				{"functionResponse":{"name":"lookup","response":{"found":true},"parts":[
					{"inline_data":{"mime_type":"image/png","data":"YWJj"}}
				]}}
			]}]}
		}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := Parse(tc.family, []byte(tc.body), "route")
			if err != nil {
				t.Fatal(err)
			}
			for _, boundary := range []struct {
				name     string
				limits   InlineMediaLimits
				rejected bool
			}{
				{"exact limits", InlineMediaLimits{Items: 2, ItemBytes: 3, TotalBytes: 5}, false},
				{"too many items", InlineMediaLimits{Items: 1, ItemBytes: 3, TotalBytes: 5}, true},
				{"item too large", InlineMediaLimits{Items: 2, ItemBytes: 2, TotalBytes: 5}, true},
				{"total too large", InlineMediaLimits{Items: 2, ItemBytes: 3, TotalBytes: 4}, true},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					err := ValidateInlineMedia(request, boundary.limits)
					if boundary.rejected {
						requireInlineMediaRejection(t, err)
						return
					}
					if err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestValidateInlineMediaRejectsMalformedMedia(t *testing.T) {
	for _, tc := range []struct {
		name string
		part string
	}{
		{"invalid base64", `{"type":"input_audio","input_audio":{"format":"wav","data":"!!!!"}}`},
		{"non-base64 data URL", `{"type":"image_url","image_url":{"url":"data:image/png,YWI="}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := Parse(wire.Chat, []byte(`{"model":"route","messages":[{"role":"user","content":[`+tc.part+`]}]}`), "")
			if err != nil {
				t.Fatal(err)
			}
			requireInlineMediaRejection(t, ValidateInlineMedia(request, InlineMediaLimits{Items: 1, ItemBytes: 8, TotalBytes: 8}))
		})
	}
}

func TestValidateInlineMediaIgnoresRemoteURLsAndOrdinaryText(t *testing.T) {
	request, err := Parse(wire.Chat, []byte(`{
		"model":"route",
		"messages":[
			{"role":"user","content":[
				{"type":"image_url","image_url":{"url":"https://example.test/sample.png"}},
				{"type":"text","text":"Explain data:image/png,YWI=."}
			]},
			{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{
				"name":"lookup","arguments":"{\"type\":\"input_audio\",\"input_audio\":{\"data\":\"!!!!\"}}"
			}}]}
		]
	}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateInlineMedia(request, InlineMediaLimits{}); err != nil {
		t.Fatal(err)
	}
}

func requireInlineMediaRejection(t *testing.T, err error) {
	t.Helper()
	rejection, ok := errors.AsType[*wire.RequestError](err)
	if !ok || rejection.Code != "unsupported_parameter" || rejection.Param != "content" {
		t.Fatalf("error=%#v, want unsupported_parameter for content", err)
	}
}
