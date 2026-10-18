package estimate

import (
	"encoding/json"
	"strconv"
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
	if estimate.Input < 1 || estimate.InputExposure() != 4*estimate.Input || estimate.Reply() != 64 || estimate.Tokens() != estimate.InputExposure()+64 {
		t.Fatalf("exposure %+v", estimate)
	}
}

func TestLegacyCompletionCandidatesDoNotMultiplyContext(t *testing.T) {
	var input int64
	for _, copies := range []int64{1, 2, 4} {
		request, err := openai.Parse(wire.Completions, []byte(`{"model":"route","prompt":[1,2,3,4,5,6],"max_tokens":100,"n":`+strconv.FormatInt(copies, 10)+`}`))
		if err != nil {
			t.Fatal(err)
		}
		e := Walk(request).Estimate(ForModel("gpt-4o"), nil)
		if copies == 1 {
			input = e.Input
		}
		if e.Input != input || e.InputExposure() != input*copies || e.Tokens() != (input+100)*copies {
			t.Fatalf("%d candidates: context=%d exposure=%d tokens=%d", copies, e.Input, e.InputExposure(), e.Tokens())
		}
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
