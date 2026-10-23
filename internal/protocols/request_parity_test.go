package protocols

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// Parse reads the conversation of an Anthropic or Gemini request from the
// request's parsed document, and legacyParse decoded json.RawMessage level by
// level. They are held to the same answer for any body: the same error to the
// code, message and parameter, or the same request.

var nativeFamilies = []wire.Format{
	wire.Anthropic, wire.AnthropicCount,
	wire.Gemini, wire.GeminiStream, wire.GeminiCount,
}

// sameParseError compares two errors. A request that has two controls out of
// range is refused for either, whichever the validation reaches first, which
// map order decides, so the controls are not told apart by their parameter.
func sameParseError(a, b error) bool {
	x, ok := a.(*wire.RequestError)
	y, other := b.(*wire.RequestError)
	if ok && other && x.Code == y.Code && x.Message == y.Message {
		switch x.Message {
		case "expected a positive integer within the protocol range", "numeric parameter is outside the protocol range":
			return true
		}
	}
	return reflect.DeepEqual(a, b)
}

func sameParse(t testing.TB, family wire.Format, data []byte) (accepted bool) {
	t.Helper()
	for _, model := range []string{"route", "Not A Slug"} {
		got, gotErr := Parse(family, data, model)
		want, wantErr := legacyParse(family, data, model)
		if !sameParseError(gotErr, wantErr) || (got == nil) != (want == nil) {
			t.Fatalf("%s as %q\nbody %.1500s\nparse:  %v\nlegacy: %v", family, model, data, gotErr, wantErr)
		}
		if got != nil && (got.Family != want.Family || got.Route != want.Route || got.Stream != want.Stream || got.OIF().Document().Raw() != want.OIF().Document().Raw()) {
			t.Fatalf("%s as %q\nbody %.1500s\nparse:  %+v\nlegacy: %+v", family, model, data, got, want)
		}
		accepted = accepted || got != nil
	}
	return accepted
}

func FuzzNativeRequestsAreParsedAsTheyWereParsed(f *testing.F) {
	f.Add([]byte(`{"model":"route","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"route","max_tokens":16,"stream":true,"messages":[{"role":"system","content":[{"type":"text","text":"x"}]},null]}`))
	f.Add([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":5,"temperature":0.5}}`))
	f.Add([]byte(`{"generateContentRequest":{"contents":[{"parts":[]}],"generationConfig":null}}`))
	f.Add([]byte(`{"generateContentRequest":{"contents":[{"parts":[{"text":"a"}]}]},"contents":[]}`))
	f.Add([]byte(`{"generateContentRequest":"x"}`))
	f.Add([]byte(`{"generateContentRequest":{"contents":[{"parts":[{"text":"a"}]}],"generationConfig":{"maxOutputTokens":-1}}}`))
	f.Add([]byte(`{"generateContentRequest":{"contents":[{"parts":[{"text":"a"}]}]},"generationConfig":{"maxOutputTokens":-1}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, family := range nativeFamilies {
			sameParse(t, family, data)
		}
	})
}
