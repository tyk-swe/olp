package health

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
)

func TestHalfOpenProbeIsReleasedOnlyByItsOwnCompletion(t *testing.T) {
	const provider = "provider"
	for _, test := range []struct {
		name       string
		probeClass string
		wantOpen   bool
	}{
		{"success closes the circuit", events.ClassSuccess, false},
		{"failure reopens the circuit", "upstream_server", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := time.Date(2026, time.March, 10, 12, 0, 0, 0, time.UTC)
			tracker := NewTracker(func() time.Time { return clock })
			for range circuitFailures {
				tracker.Record(provider, events.AttemptFact{Class: "upstream_server"}, false)
			}
			if !tracker.Open(provider) {
				t.Fatal("repeated failures did not open the circuit")
			}
			clock = clock.Add(CircuitOpenFor)
			if probed, granted := tracker.Claim(provider); !probed || !granted {
				t.Fatalf("recovering circuit refused its probe: probed=%v granted=%v", probed, granted)
			}
			for _, unrelated := range []string{"rate_limit", "upstream_server"} {
				tracker.Record(provider, events.AttemptFact{Class: unrelated}, false)
				if _, granted := tracker.Claim(provider); granted {
					t.Fatalf("in-flight %s completion admitted a second probe", unrelated)
				}
			}
			tracker.Record(provider, events.AttemptFact{Class: test.probeClass}, true)
			if open := tracker.Open(provider); open != test.wantOpen {
				t.Fatalf("open after probe = %v, want %v", open, test.wantOpen)
			}
		})
	}
}
