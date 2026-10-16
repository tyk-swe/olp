package protocols

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestNativeGenerationRequestsAreValidatedForTheirRoute(t *testing.T) {
	for _, test := range []struct {
		family wire.Format
		body   string
		valid  bool
	}{
		{wire.MistralFIM, `{"prompt":"def f():","suffix":"\n","max_tokens":16,"stream":true}`, true},
		{wire.MistralFIM, `{"model":"code","prompt":"x"}`, true},
		{wire.MistralFIM, `{"model":"other","prompt":"x"}`, false},
		{wire.MistralFIM, `{"suffix":"x"}`, false},
		{wire.MistralFIM, `{"prompt":"x","max_tokens":-1}`, false},
		{wire.MistralFIM, `{"prompt":"x","stream":"yes"}`, false},
		{wire.CohereChat, `{"messages":[{"role":"user","content":"hi"}],"documents":[{"data":{"text":"d"}}]}`, true},
		{wire.CohereChat, `{"messages":[]}`, false},
		{wire.CohereChat, `{"messages":[{"role":"developer","content":"hi"}]}`, false},
	} {
		request, err := Parse(test.family, []byte(test.body), "code")
		if (err == nil) != test.valid {
			t.Fatalf("%s %s: %v", test.family, test.body, err)
		}
		if err == nil && (request.Route != "code" || request.Family.Surface() != "native" || request.Stream != strings.Contains(test.body, `"stream":true`)) {
			t.Fatalf("%s request = %+v", test.family, request)
		}
	}
	if family, ok := NativeGenerationFamily("mistral-fim"); !ok || family != wire.MistralFIM {
		t.Fatal("mistral-fim is not a native generation dialect")
	}
	if _, ok := NativeGenerationFamily("cohere-embed-v2"); ok {
		t.Fatal("an operation dialect was taken for generation")
	}
}

func cohereStream(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		kind := event[strings.Index(event, `"type":"`)+8:]
		kind = kind[:strings.Index(kind, `"`)]
		b.WriteString("event: " + kind + "\ndata: " + event + "\n\n")
	}
	return b.String()
}
