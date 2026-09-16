//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestRoutingMeasurementsRequireEnoughFreshSuccessfulSamples(t *testing.T) {
	f := repSetup(t)
	now := time.Now().UTC()
	status := 200
	add := func(model string, observed time.Time, withOutput bool, failed bool) {
		id := access.NewID()
		routing := `{"mode":"streaming","first_output_ms":20}`
		if withOutput {
			routing = `{"mode":"streaming","first_output_ms":20,"streamed_output_tokens":10}`
		}
		var failure *string
		if failed {
			v := "upstream_server"
			failure = &v
		}
		f.request(repRequest{ID: id, StartedAt: observed, Route: "measurements", Operation: "generation", Surface: "openai", StatusCode: &status, AttemptCount: 1})
		f.attempt(repAttempt{RequestID: id, StartedAt: observed, Ordinal: 1, ProviderID: f.P1, Model: model, Committed: true, StatusCode: &status, ErrorClass: failure, Routing: &routing})
	}
	for i := 0; i < 20; i++ {
		add("complete", now.Add(-time.Minute), true, false)
		add("partial", now.Add(-time.Minute), i == 0, false)
		add("old", now.Add(-6*time.Minute), true, false)
		add("failed", now.Add(-time.Minute), true, true)
		if i < 19 {
			add("small", now.Add(-time.Minute), true, false)
		}
	}
	inputs, err := usage.LoadRoutingInputs(t.Context(), f.pool, now)
	if err != nil {
		t.Fatal(err)
	}
	complete := inputs.Metrics(f.P1, "complete", "generation", "streaming", now)
	if complete == nil || complete.SampleCount != 20 || complete.LatencyMS != 20 || complete.Throughput == nil || *complete.Throughput != 100 {
		t.Fatalf("complete measurement: %+v", complete)
	}
	partial := inputs.Metrics(f.P1, "partial", "generation", "streaming", now)
	if partial == nil || partial.Throughput != nil {
		t.Fatalf("one known output count became twenty throughput samples: %+v", partial)
	}
	for _, model := range []string{"old", "failed", "small"} {
		if inputs.Metrics(f.P1, model, "generation", "streaming", now) != nil {
			t.Fatalf("invalid measurement promoted: %s", model)
		}
	}
	if inputs.Metrics(f.P1, "complete", "generation", "streaming", now.Add(61*time.Second)) != nil {
		t.Fatal("stale refresh retained performance ordering")
	}
}
