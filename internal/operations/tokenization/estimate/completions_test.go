package estimate

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestLegacyCompletionExposureIncludesEveryGeneratedCandidate(t *testing.T) {
	request, err := openai.Parse(wire.Completions, []byte(`{"model":"route","prompt":["one","two"],"max_tokens":8,"n":2,"best_of":4}`))
	if err != nil {
		t.Fatal(err)
	}
	output, candidates := OutputBounds(request, nil)
	if output == nil || *output != 8 || candidates != 8 {
		t.Fatalf("bounds %v %d", output, candidates)
	}
	estimate := Walk(request).Estimate(ForModel("gpt-4o"), nil)
	if estimate.Input < 4 || estimate.Reply() != 64 {
		t.Fatalf("exposure %+v", estimate)
	}
}

func TestLegacyCompletionOutputBoundIgnoresChatAlias(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		defaults     map[string]json.RawMessage
		want         *int64
	}{
		{name: "explicit", fields: `,"max_tokens":1000,"max_completion_tokens":1`, want: new(int64(1000))},
		{name: "default", fields: `,"max_completion_tokens":1`, defaults: map[string]json.RawMessage{"max_tokens": json.RawMessage(`1000`)}, want: new(int64(1000))},
		{name: "unbounded", fields: `,"max_completion_tokens":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := openai.Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello"`+tc.fields+`}`))
			if err != nil {
				t.Fatal(err)
			}
			output, _ := OutputBounds(request, tc.defaults)
			if (output == nil) != (tc.want == nil) || output != nil && *output != *tc.want {
				t.Fatalf("output bound %v; want %v", output, tc.want)
			}
		})
	}
}
