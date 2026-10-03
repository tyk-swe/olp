package protocols

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Parse reads the conversation of an Anthropic or Gemini request from the
// request's parsed document, and legacyParse decoded json.RawMessage level by
// level. They are held to the same answer for any body: the same error to the
// code, message and parameter, or the same request.

var nativeFamilies = []openai.Family{
	openai.FamilyAnthropic, openai.FamilyAnthropicCount,
	openai.FamilyGemini, openai.FamilyGeminiStream, openai.FamilyGeminiCount,
}

type nativeGen struct{ r *rand.Rand }

func (g nativeGen) intn(n int) int { return g.r.IntN(n) }

func (g nativeGen) pick(options ...any) any { return options[g.intn(len(options))] }

func (g nativeGen) wild(depth int) any {
	switch g.intn(8) {
	case 0:
		return g.pick("text", "", "user", "line\n\"quoted\"")
	case 1:
		return g.pick(0, 1, 2.5, -1)
	case 2, 3:
		return nil
	case 4, 5:
		if depth <= 0 {
			return true
		}
		items := make([]any, g.intn(3))
		for i := range items {
			items[i] = g.wild(depth - 1)
		}
		return items
	}
	if depth <= 0 {
		return false
	}
	out := map[string]any{}
	for range g.intn(4) {
		out[g.pick("role", "content", "parts", "text", "contents").(string)] = g.wild(depth - 1)
	}
	return out
}

func (g nativeGen) anthropicMessage() any {
	if g.intn(10) == 0 {
		return g.wild(2)
	}
	message := map[string]any{}
	if g.intn(10) != 0 {
		message["role"] = g.pick("user", "assistant", "system", "tool", "", nil, 4)
	}
	if g.intn(8) != 0 {
		message["content"] = g.pick("hello \"there\"\n", []any{map[string]any{"type": "text", "text": "hi"}}, nil, 7, []any{}, g.wild(2))
	}
	return message
}

func (g nativeGen) geminiContent() any {
	if g.intn(10) == 0 {
		return g.wild(2)
	}
	content := map[string]any{"role": g.pick("user", "model")}
	if g.intn(8) != 0 {
		content["parts"] = g.pick([]any{map[string]any{"text": "hi"}}, []any{}, "text", nil, g.wild(2))
	}
	return content
}

func (g nativeGen) list(item func() any) any {
	switch g.intn(12) {
	case 0:
		return g.wild(2)
	case 1:
		return []any{}
	}
	items := make([]any, 1+g.intn(3))
	for i := range items {
		items[i] = item()
	}
	return items
}

func (g nativeGen) controls(gemini bool) map[string]any {
	controls := map[string]any{}
	names := []string{"max_tokens", "temperature", "top_p", "top_k"}
	if gemini {
		names = []string{"maxOutputTokens", "temperature", "topP", "topK", "candidateCount"}
	}
	for _, name := range names {
		if g.intn(3) == 0 {
			switch name {
			case "temperature", "top_p", "topP":
				controls[name] = g.pick(0.5, 1, 0)
			default:
				controls[name] = g.pick(1, 5)
			}
		}
	}
	if g.intn(6) == 0 {
		controls[names[g.intn(len(names))]] = g.pick(-1, "x", 100000000000, 3, nil)
	}
	return controls
}

// badConfig is a generation config that the validation of the controls refuses.
func (g nativeGen) badConfig() any {
	return g.pick(
		map[string]any{"maxOutputTokens": -1},
		map[string]any{"candidateCount": 0},
		map[string]any{"temperature": 3},
		map[string]any{"topP": "x"},
		"x", []any{})
}

// tameBody is a request a client could send, which has at most one thing wrong
// with it, in the place the validation reads from the document.
func (g nativeGen) tameBody(family openai.Family) []byte {
	raw := map[string]any{}
	messages := make([]any, 1+g.intn(3))
	var mutate func(any)
	content, wrapped := raw, false
	if family.Surface() == "anthropic" {
		raw["model"], raw["max_tokens"] = "route", 16
		for i := range messages {
			messages[i] = map[string]any{"role": g.pick("user", "assistant", "system"), "content": g.pick("hi", []any{map[string]any{"type": "text", "text": "hi"}})}
		}
		raw["messages"] = messages
		mutate = func(v any) { messages[g.intn(len(messages))] = v }
	} else {
		if family == openai.FamilyGeminiCount && g.intn(2) == 0 {
			content, wrapped = map[string]any{}, true
			raw["generateContentRequest"] = content
		}
		for i := range messages {
			messages[i] = map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}
		}
		content["contents"] = messages
		if g.intn(2) == 0 {
			content["generationConfig"] = map[string]any{"maxOutputTokens": 5, "temperature": 0.5}
		}
		mutate = func(v any) { messages[g.intn(len(messages))] = v }
	}
	switch g.intn(7) {
	case 0:
		if family.Surface() == "anthropic" {
			mutate(g.anthropicMessage())
		} else {
			mutate(g.geminiContent())
		}
	case 1:
		mutate(g.wild(2))
	case 2:
		// The controls are read from the request that holds the conversation,
		// which is the wrapped one for a countTokens request that wraps it.
		if family.Surface() != "anthropic" {
			content["generationConfig"] = g.badConfig()
		}
	case 3:
		// A config beside the wrapper is not read, whatever it holds.
		if wrapped {
			raw["generationConfig"] = g.badConfig()
		}
	}
	data, _ := json.Marshal(raw)
	return data
}

func (g nativeGen) body(family openai.Family) []byte {
	raw := map[string]any{}
	if family.Surface() == "anthropic" {
		for name, v := range g.controls(false) {
			raw[name] = v
		}
		if g.intn(10) != 0 {
			raw["model"] = g.pick("route", "Not A Slug", nil, 3)
		}
		if g.intn(10) != 0 {
			raw["max_tokens"] = g.pick(16, 16, 16, 0, "x", nil)
		}
		if g.intn(6) == 0 {
			raw["stream"] = g.pick(true, false, "yes", nil)
		}
		raw["messages"] = g.list(g.anthropicMessage)
	} else {
		content, wrapped := raw, g.intn(3) == 0
		if wrapped {
			content = map[string]any{}
			raw["generateContentRequest"] = g.pick(content, content, content, nil, "x", []any{}, g.wild(2))
			if g.intn(8) == 0 {
				raw["contents"] = g.list(g.geminiContent)
			}
		}
		if g.intn(4) == 0 {
			controls := g.controls(true)
			content["generationConfig"] = g.pick(controls, controls, nil, "x", []any{})
		}
		if wrapped && g.intn(3) == 0 {
			content["generationConfig"] = g.badConfig()
		}
		if wrapped && g.intn(3) == 0 {
			raw["generationConfig"] = g.pick(g.badConfig(), g.controls(true))
		}
		content["contents"] = g.list(g.geminiContent)
		if g.intn(12) == 0 {
			raw["model"] = "route"
		}
		if g.intn(12) == 0 {
			raw["stream"] = g.pick(true, nil)
		}
	}
	data, _ := json.Marshal(raw)
	return data
}

// sameParseError compares two errors. A request that has two controls out of
// range is refused for either, whichever the validation reaches first, which
// map order decides, so the controls are not told apart by their parameter.
func sameParseError(a, b error) bool {
	x, ok := a.(*openai.RequestError)
	y, other := b.(*openai.RequestError)
	if ok && other && x.Code == y.Code && x.Message == y.Message {
		switch x.Message {
		case "expected a positive integer within the protocol range", "numeric parameter is outside the protocol range":
			return true
		}
	}
	return reflect.DeepEqual(a, b)
}

func sameParse(t testing.TB, family openai.Family, data []byte) (accepted bool) {
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

func TestNativeRequestsAreParsedAsTheyWereParsed(t *testing.T) {
	cases := 600
	if testing.Short() {
		cases = 100
	}
	var accepted, refused int
	for _, family := range nativeFamilies {
		g := nativeGen{rand.New(rand.NewPCG(59, uint64(len(family))))}
		for range cases {
			body := g.body(family)
			if g.intn(2) == 0 {
				body = g.tameBody(family)
			}
			if sameParse(t, family, body) {
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

// A countTokens request may wrap the request whose tokens it counts, and the
// controls it is refused for are the wrapped request's. The generator reaches
// these cases by chance, so each is also held to its answer here.
func TestCountTokensControlsAreTheWrappedRequests(t *testing.T) {
	const contents = `"contents":[{"parts":[{"text":"a"}]}]`
	for _, tc := range []struct {
		name, body, param string
	}{
		{"wrapped control out of range", `{"generateContentRequest":{` + contents + `,"generationConfig":{"maxOutputTokens":-1}}}`, "generationConfig.maxOutputTokens"},
		{"wrapped control of the wrong type", `{"generateContentRequest":{` + contents + `,"generationConfig":{"temperature":"x"}}}`, "generationConfig.temperature"},
		{"wrapped config is not an object", `{"generateContentRequest":{` + contents + `,"generationConfig":"x"}}`, "generationConfig"},
		{"wrapped config is an array", `{"generateContentRequest":{` + contents + `,"generationConfig":[]}}`, "generationConfig"},
		{"wrapped config is valid", `{"generateContentRequest":{` + contents + `,"generationConfig":{"maxOutputTokens":5}}}`, ""},
		{"config beside the wrapper is not read", `{"generateContentRequest":{` + contents + `},"generationConfig":{"maxOutputTokens":-1}}`, ""},
		{"config beside the wrapper does not excuse the wrapped one", `{"generateContentRequest":{` + contents + `,"generationConfig":{"topK":0}},"generationConfig":{"topK":1}}`, "generationConfig.topK"},
		{"unwrapped control out of range", `{` + contents + `,"generationConfig":{"candidateCount":0}}`, "generationConfig.candidateCount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sameParse(t, openai.FamilyGeminiCount, []byte(tc.body))
			_, err := Parse(openai.FamilyGeminiCount, []byte(tc.body), "route")
			if tc.param == "" {
				if err != nil {
					t.Fatalf("the request was refused: %v", err)
				}
				return
			}
			var refused *openai.RequestError
			if !errors.As(err, &refused) || refused.Param != tc.param {
				t.Fatalf("got %v, want a request error for %q", err, tc.param)
			}
		})
	}
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
