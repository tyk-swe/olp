package estimate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

// The walker reads a request from its parsed document, and the walker it
// replaced decoded json.RawMessage level by level, which walk_reference_test.go
// keeps. Both are held to the same input, whole: every segment of text with what
// was kept of it, the roles, the counts of messages and names, the flat charges
// and whether the request is approximate. A count that is made from them cannot
// differ if they do not.

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
func referenceReading(request *wire.Request, retain int) *input {
	in := &input{retain: retain}
	refWalkInput(in, request.Family, func(name string) json.RawMessage {
		if raw := request.Field(name); len(raw) > 0 {
			return raw
		}
		return nil
	})
	return in
}

// The comparison is only worth something if the requests have something to
// read, which the families that read nothing but a tool catalogue do not.

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
func sameReadings(t testing.TB, family wire.Format, data []byte) {
	t.Helper()
	if doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 16 << 20, MaxNodes: 1 << 20, MaxDepth: 32}); err == nil && doc.Root().Kind() == oif.Object {
		request, err := wire.NewRequest(family, "route", false, doc)
		if err != nil {
			t.Fatal(err)
		}
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
