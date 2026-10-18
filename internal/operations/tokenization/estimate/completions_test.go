package estimate

import (
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
