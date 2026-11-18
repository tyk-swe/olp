package protocols

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

const geminiRequestContents = `"contents":[{"role":"user","parts":[{"text":"hello"}]}]`

func TestParseNativeRequestEnvelope(t *testing.T) {
	const pathRoute = "path-model"
	for _, tc := range []struct {
		name   string
		family wire.Format
		body   string
		route  string
		stream bool
	}{
		{
			"Anthropic messages stream with a system message between turns",
			wire.Anthropic,
			`{"model":"body-model","stream":true,"max_tokens":16,"messages":[
				{"role":"user","content":"hello"},
				{"role":"assistant","content":"hi"},
				{"role":"system","content":"change the topic"},
				{"role":"user","content":"continue"}
			]}`,
			"body-model",
			true,
		},
		{
			"Anthropic count uses the body model without a generation limit",
			wire.AnthropicCount,
			`{"model":"body-model","messages":[{"role":"user","content":"hello"}]}`,
			"body-model",
			false,
		},
		{"Gemini count with contents", wire.GeminiCount, `{` + geminiRequestContents + `}`, pathRoute, false},
		{"Gemini count with a wrapped request", wire.GeminiCount, `{"generateContentRequest":{` + geminiRequestContents + `}}`, pathRoute, false},
		{"Gemini unary generation", wire.Gemini, `{` + geminiRequestContents + `}`, pathRoute, false},
		{"Gemini streaming generation", wire.GeminiStream, `{` + geminiRequestContents + `}`, pathRoute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := Parse(tc.family, []byte(tc.body), pathRoute)
			if err != nil {
				t.Fatal(err)
			}
			if request.Family != tc.family || request.Route != tc.route || request.Stream != tc.stream {
				t.Fatalf("family=%s route=%q stream=%t, want %s %q %t", request.Family, request.Route, request.Stream, tc.family, tc.route, tc.stream)
			}
		})
	}
}

func TestParseNativeRequestRejectsInvalidEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		family    wire.Format
		body      string
		pathRoute string
		param     string
	}{
		{
			"Anthropic count cannot stream",
			wire.AnthropicCount,
			`{"model":"route","stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			"",
			"stream",
		},
		{
			"Anthropic generation needs a token limit",
			wire.Anthropic,
			`{"model":"route","messages":[{"role":"user","content":"hello"}]}`,
			"",
			"max_tokens",
		},
		{
			"Anthropic messages cannot contain null",
			wire.Anthropic,
			`{"model":"route","max_tokens":16,"messages":[{"role":"user","content":"hello"},null]}`,
			"",
			"",
		},
		{
			"Gemini count cannot use both envelopes",
			wire.GeminiCount,
			`{` + geminiRequestContents + `,"generateContentRequest":{` + geminiRequestContents + `}}`,
			"route",
			"generateContentRequest",
		},
		{"Gemini wrapper must be an object", wire.GeminiCount, `{"generateContentRequest":"hello"}`, "route", ""},
		{"Gemini parts cannot be empty", wire.GeminiCount, `{"generateContentRequest":{"contents":[{"parts":[]}]}}`, "route", "contents.parts"},
		{
			"Gemini validates controls inside the wrapper",
			wire.GeminiCount,
			`{"generateContentRequest":{` + geminiRequestContents + `,"generationConfig":{"maxOutputTokens":-1}}}`,
			"route",
			"generationConfig.maxOutputTokens",
		},
		{"Gemini body cannot set the model", wire.Gemini, `{"model":"other",` + geminiRequestContents + `}`, "route", "model"},
		{"Gemini body cannot select streaming", wire.Gemini, `{"stream":true,` + geminiRequestContents + `}`, "route", "stream"},
		{"Gemini path model must be a route slug", wire.Gemini, `{` + geminiRequestContents + `}`, "Not A Slug", "model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := Parse(tc.family, []byte(tc.body), tc.pathRoute)
			rejection, ok := errors.AsType[*wire.RequestError](err)
			if request != nil || !ok || rejection.Code != "unsupported_parameter" || rejection.Param != tc.param {
				t.Fatalf("request=%v error=%#v, want unsupported_parameter for %q", request, err, tc.param)
			}
		})
	}
}
