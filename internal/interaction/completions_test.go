package interaction

import (
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestCompletionInputPolicyCoverage(t *testing.T) {
	for _, tc := range []struct {
		prompt string
		ok     bool
	}{{`"hello"`, true}, {`["hello","world"]`, true}, {`[1,2]`, false}, {`[[1],[2]]`, false}} {
		doc, err := oif.ParseJSON([]byte(`{"model":"route","prompt":`+tc.prompt+`,"max_tokens":8}`), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckGuardrailCoverage(wire.Completions, doc); (err == nil) != tc.ok {
			t.Errorf("prompt %s: %v", tc.prompt, err)
		}
	}
}

func TestCompletionOutputPolicyRefusesTokenAlternatives(t *testing.T) {
	doc, err := oif.ParseJSON([]byte(`{"model":"route","prompt":"hello","logprobs":0}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := outputRequestCoverage(wire.Completions, doc); err == nil {
		t.Fatal("uninspectable token output admitted")
	}
	doc, err = oif.ParseJSON([]byte(`{"id":"c","object":"text_completion","model":"m","choices":[{"index":0,"text":"hello","finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLegacyOutputCoverage(wire.Completions, doc); err != nil {
		t.Fatal(err)
	}
}
