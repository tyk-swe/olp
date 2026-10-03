package estimate

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// The walker reads a request from its parsed document, and the walker it
// replaced decoded json.RawMessage level by level, which walk_reference_test.go
// keeps. Both are held to the same input, whole: every segment of text with what
// was kept of it, the roles, the counts of messages and names, the flat charges
// and whether the request is approximate. A count that is made from them cannot
// differ if they do not.

// extraSchema is a JSON schema of the kind a structured output or a tool carries.
func (g requestGen) extraSchema() any {
	if g.intn(6) == 0 {
		return g.wild(2)
	}
	return map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string", "description": g.text()}}, "required": []string{"city"}}
}

// extraBlock is a member of a native conversation that only the walker that has
// read the structured outputs, reasoning and documents of every dialect reads.
func (g requestGen) extraBlock(depth int) any {
	switch g.intn(9) {
	case 0:
		return map[string]any{"type": "thinking", "thinking": g.text(), "signature": g.text()}
	case 1:
		return map[string]any{"type": "redacted_thinking", "data": g.text()}
	case 2:
		return map[string]any{"type": "document", "source": map[string]any{"type": "text", "data": g.text()}}
	case 3:
		return map[string]any{"type": "document", "source": map[string]any{"type": "content", "content": g.dialect(depth)}}
	case 4:
		return map[string]any{"type": "document", "source": map[string]any{"type": "base64", "data": g.text()}}
	case 5:
		return map[string]any{"type": "tool_use", "id": g.text(), "input": g.wild(2)}
	case 6:
		return map[string]any{"type": "tool_result", "content": g.dialect(depth)}
	case 7:
		return map[string]any{"type": "document", "source": g.wild(1)}
	}
	return g.dialect(depth)
}

// richFields is what fields makes, with the members the legacy walker never read.
func (g requestGen) richFields(family openai.Family) map[string]json.RawMessage {
	fields := g.fields(family)
	put := func(name string, v any) { fields[name], _ = json.Marshal(v) }
	if g.intn(2) == 0 {
		put("response_format", g.maybe(map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "n", "schema": g.extraSchema()}}))
	}
	if g.intn(2) == 0 {
		put("text", g.maybe(map[string]any{"format": g.maybe(map[string]any{"type": "json_schema", "schema": g.extraSchema()})}))
	}
	if g.intn(3) == 0 {
		put("output_config", g.maybe(map[string]any{"format": g.maybe(map[string]any{"schema": g.extraSchema()})}))
	}
	if g.intn(3) == 0 {
		put("output_format", g.maybe(map[string]any{"schema": g.extraSchema()}))
	}
	if g.intn(3) == 0 {
		var schema any = g.extraSchema()
		if g.intn(2) == 0 {
			schema, _ = json.Marshal(schema)
			schema = string(schema.([]byte))
		}
		put("outputConfig", g.maybe(map[string]any{"textFormat": map[string]any{"structure": map[string]any{"jsonSchema": map[string]any{"schema": schema}}}}))
	}
	if g.intn(3) == 0 {
		put("toolConfig", g.wild(3))
	}
	if g.intn(2) == 0 {
		put("generationConfig", g.maybe(map[string]any{"maxOutputTokens": g.number(), "candidateCount": g.number(), "responseSchema": g.extraSchema(), "responseJsonSchema": g.extraSchema()}))
	}
	if g.intn(3) == 0 {
		put("generateContentRequest", map[string]any{"contents": g.extraBlock(2), "systemInstruction": g.extraBlock(2), "tools": g.tools(), "generationConfig": map[string]any{"responseSchema": g.extraSchema()}})
	}
	if g.intn(2) == 0 {
		items := make([]any, g.intn(4))
		for i := range items {
			switch g.intn(3) {
			case 0:
				items[i] = map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": g.text()}, g.wild(1)}, "encrypted_content": g.text()}
			case 1:
				items[i] = map[string]any{"type": "reasoning", "summary": g.wild(2), "content": g.content()}
			default:
				items[i] = g.message()
			}
		}
		put([]string{"input", "messages"}[g.intn(2)], items)
	}
	for _, name := range []string{"system", "contents", "systemInstruction"} {
		if g.intn(3) == 0 {
			put(name, g.extraBlock(3))
		}
	}
	if g.intn(3) == 0 {
		put("contents", []any{map[string]any{"role": "user", "parts": []any{g.extraBlock(2), g.extraBlock(2)}}})
	}
	return fields
}

// richDefaults is what defaults makes, with the structured outputs a provider can
// default.
func (g requestGen) richDefaults() map[string]json.RawMessage {
	defaults := g.defaults()
	if defaults == nil {
		defaults = map[string]json.RawMessage{}
	}
	put := func(name string, v any) { defaults[name], _ = json.Marshal(v) }
	if g.intn(3) == 0 {
		put("response_format", map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "n", "schema": g.extraSchema()}})
	}
	if g.intn(3) == 0 {
		put("text", map[string]any{"format": map[string]any{"type": "json_schema", "schema": g.extraSchema()}})
	}
	if g.intn(4) == 0 {
		put("output_config", map[string]any{"format": map[string]any{"schema": g.extraSchema()}, "effort": "high"})
	}
	if g.intn(4) == 0 {
		put("generationConfig", map[string]any{"responseSchema": g.extraSchema(), "temperature": 0.2})
	}
	if g.intn(6) == 0 {
		put("outputConfig", map[string]any{"textFormat": map[string]any{"structure": map[string]any{"jsonSchema": map[string]any{"schema": g.extraSchema()}}}})
	}
	if g.intn(6) == 0 {
		put("toolConfig", g.wild(2))
	}
	return defaults
}

// sameInput fails the test if the walker did not read what the reference did.
func sameInput(t testing.TB, got, want *input, what string, body any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		shown, ok := body.(json.RawMessage)
		if !ok {
			shown, _ = json.Marshal(body)
		}
		t.Fatalf("%s\nrequest %.1500s\nwalker    %+v\nreference %+v", what, shown, *got, *want)
	}
}

// referenceReading walks the fields of a request with the reference walker, as
// the walker it replaced was asked for them: by name, from the document.
func referenceReading(request *openai.Request, retain int) *input {
	in := &input{retain: retain}
	refWalkInput(in, request.Family, func(name string) json.RawMessage {
		if raw := request.Field(name); len(raw) > 0 {
			return raw
		}
		return nil
	})
	return in
}

// TestWalkerReadsWhatTheReferenceReads walks generated requests of every family,
// in the shapes clients send and in any JSON at all, and requires the input the
// walker reads from the document to be the one the reference reads from the
// fields, whether the walk keeps all of the text, none of it or a few bytes.
func TestWalkerReadsWhatTheReferenceReads(t *testing.T) {
	cases := 300
	if testing.Short() {
		cases = 50
	}
	// The comparison is only worth something if the requests have something to
	// read, which the families that read nothing but a tool catalogue do not.
	var read, flat, approx int
	defer func() {
		if read < cases*3 || flat < cases/2 || approx < cases/2 {
			t.Errorf("the generator read text in %d cases, a flat charge in %d and an approximate request in %d: it is not exercising the walker", read, flat, approx)
		}
	}()
	for _, family := range walkerFamilies {
		t.Run(string(family), func(t *testing.T) {
			g := requestGen{rand.New(rand.NewPCG(31, uint64(len(family))))}
			for i := range cases {
				fields := g.richFields(family)
				request := openai.NewEnvelope(family, "route", false, fields)
				for _, retain := range []int{0, 1 << 40, 5, 64} {
					got := &input{retain: retain}
					walkInput(got, family, requestFields(request))
					want := referenceReading(request, retain)
					sameInput(t, got, want, fmt.Sprintf("case %d, retaining %d bytes", i, retain), fields)
					if retain == 1<<40 {
						if got.bytes > 0 {
							read++
						}
						if got.flat > 0 {
							flat++
						}
						if got.approx {
							approx++
						}
					}
				}
			}
		})
	}
}

// TestWalkerReadsTheDefaultsTheReferenceReads is the same for what a provider's
// defaults supply, which is raw JSON that nothing has checked: the fields the
// caller left out are read from it, and the caller's own are not.
func TestWalkerReadsTheDefaultsTheReferenceReads(t *testing.T) {
	cases := 300
	if testing.Short() {
		cases = 50
	}
	var supplied int
	defer func() {
		if supplied < cases {
			t.Errorf("defaults supplied prompt text in %d cases: the generator is not exercising them", supplied)
		}
	}()
	for _, family := range walkerFamilies {
		t.Run(string(family), func(t *testing.T) {
			g := requestGen{rand.New(rand.NewPCG(37, uint64(len(family))))}
			for i := range cases {
				request := openai.NewEnvelope(family, "route", false, g.richFields(family))
				prompt := Walk(request)
				for range 3 {
					defaults := g.richDefaults()
					got, ok := prompt.defaulted(defaults)
					want := &input{}
					refWalkInput(want, family, func(name string) json.RawMessage {
						fallback := defaults[name]
						if !mayHoldPrompt(name, fallback) {
							return nil
						}
						own := request.Field(name)
						if name == "generationConfig" {
							return unsetMembers(own, fallback)
						}
						if len(own) > 0 {
							return nil
						}
						return fallback
					})
					if ok != !want.empty() {
						t.Fatalf("case %d: the walker found defaulted text: %v, the reference: %v", i, ok, !want.empty())
					}
					if ok {
						supplied++
						sameInput(t, &got, want, fmt.Sprintf("case %d, defaults", i), defaults)
					}
				}
			}
		})
	}
}

// FuzzWalkerReadsWhatTheReferenceReads reads any JSON object as a request of any
// family, and any bytes at all as the value of every field of a provider's
// defaults, where the reference is as lenient as encoding/json is.
func FuzzWalkerReadsWhatTheReferenceReads(f *testing.F) {
	f.Add(byte(0), []byte(`{"model":"m","messages":[{"role":"user","content":"hi \"there\"\n"},{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}]}`))
	f.Add(byte(1), []byte(`{"model":"m","input":[{"type":"reasoning","summary":[{"text":"x"}]},{"type":"message","content":[{"type":"input_image"},"bare"]}],"instructions":"i","text":{"format":{"schema":{"a":1}}}}`))
	f.Add(byte(3), []byte(`{"input":["a",[1,2],null,[[3]]]}`))
	f.Add(byte(6), []byte(`{"system":[{"type":"thinking","thinking":"t"},{"type":"document","source":{"type":"text","data":"d"}}],"messages":[{"role":"user","content":[{"type":"image","source":{}},{"type":"tool_result","content":"r"}]}],"tools":[{"name":"n"}],"output_config":{"format":{"schema":{}}}}`))
	f.Add(byte(8), []byte(`{"contents":[{"role":"user","parts":[{"text":"x"},{"inlineData":{"data":"AAAA"}},{"functionResponse":{"response":"y"}}]}],"systemInstruction":"s","generationConfig":{"responseSchema":{"type":"object"},"maxOutputTokens":3}}`))
	f.Add(byte(13), []byte(`{"messages":[{"role":"user","content":[{"text":"a"}]}],"toolConfig":{"tools":[{"toolSpec":{}}]},"outputConfig":{"textFormat":{"structure":{"jsonSchema":{"schema":"{}"}}}}}`))
	f.Fuzz(func(t *testing.T, which byte, data []byte) {
		sameReadings(t, walkerFamilies[int(which)%len(walkerFamilies)], data)
	})
}

// sameReadings holds the walker to the reference for one body read as a request
// of one family, and for the body read as every field of a provider's defaults.
func sameReadings(t testing.TB, family openai.Family, data []byte) {
	t.Helper()
	if doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 16 << 20, MaxNodes: 1 << 20, MaxDepth: 32}); err == nil && doc.Root().Kind() == oif.Object {
		request := openai.NewSourceEnvelope(family, "route", false, doc)
		for _, retain := range []int{0, 1 << 40, 7} {
			got := &input{retain: retain}
			walkInput(got, family, requestFields(request))
			sameInput(t, got, referenceReading(request, retain), fmt.Sprintf("%s, retaining %d bytes", family, retain), json.RawMessage(data))
		}
		// The members of the document are as good a default as any bytes.
		defaults := doc.Fields()
		got, want := &input{}, &input{}
		walkInput(got, family, func(name string) node { return rawNode(defaults[name]) })
		refWalkInput(want, family, func(name string) json.RawMessage { return defaults[name] })
		sameInput(t, got, want, family.Surface()+" defaults", json.RawMessage(data))
	}
	got, want := &input{}, &input{}
	walkInput(got, family, func(string) node { return rawNode(data) })
	refWalkInput(want, family, func(string) json.RawMessage { return data })
	sameInput(t, got, want, string(family)+" raw", json.RawMessage(data))
}
