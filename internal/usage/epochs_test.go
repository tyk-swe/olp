package usage

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func ingestSnapshot(epoch string) Snapshot {
	return Snapshot{ProcessEpoch: epoch, StartedAt: time.Unix(1700000000, 0).UTC()}
}

func TestValidateCheckpointRejectsUnaccountableCounters(t *testing.T) {
	t.Parallel()
	epoch := "018f3a5c-0000-7000-8000-000000000001"
	drained := Snapshot{ProcessEpoch: epoch, Accepted: 2, Persisted: 1, Abandoned: 1, Closed: true}
	cases := []struct {
		name     string
		instance string
		snapshot Snapshot
		graceful bool
		reason   string
	}{
		{name: "a bounded label is kept", instance: "  gateway-1  ",
			snapshot: ingestSnapshot(epoch)},
		{name: "a blank label cannot identify a gateway", instance: "   ",
			snapshot: ingestSnapshot(epoch), reason: "gateway instance label"},
		{name: "an oversized label would not fit the row",
			instance: strings.Repeat("g", 201), snapshot: ingestSnapshot(epoch),
			reason: "gateway instance label"},
		{name: "a control character would corrupt a log line", instance: "gate\x07way",
			snapshot: ingestSnapshot(epoch), reason: "gateway instance label"},
		{name: "negative counters are impossible", instance: "gateway-1",
			snapshot: Snapshot{ProcessEpoch: epoch, Dropped: -1}, reason: "negative counters"},
		{name: "more persisted than accepted is impossible", instance: "gateway-1",
			snapshot: Snapshot{ProcessEpoch: epoch, Accepted: 1, Persisted: 2},
			reason:   "counters exceed accepted events"},
		{name: "more abandoned than outstanding is impossible", instance: "gateway-1",
			snapshot: Snapshot{ProcessEpoch: epoch, Accepted: 2, Persisted: 2, Abandoned: 1},
			reason:   "counters exceed accepted events"},
		{name: "a graceful close requires a drained writer", instance: "gateway-1",
			snapshot: Snapshot{ProcessEpoch: epoch, Accepted: 1}, graceful: true,
			reason: "writer has not drained"},
		{name: "a drained writer may close gracefully", instance: "gateway-1",
			snapshot: drained, graceful: true},
		{name: "a process epoch must be a uuid", instance: "gateway-1",
			snapshot: ingestSnapshot("not-a-uuid"), reason: "process epoch"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			label, processEpoch, err := validateCheckpoint(test.instance, test.snapshot, test.graceful)
			if test.reason != "" {
				if !errors.Is(err, ErrInvalidCheckpoint) {
					t.Fatalf("error = %v, want ErrInvalidCheckpoint", err)
				}
				if !strings.Contains(err.Error(), test.reason) {
					t.Fatalf("error = %v, want reason %q", err, test.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCheckpoint: %v", err)
			}
			if label != strings.TrimSpace(test.instance) {
				t.Fatalf("label = %q, want %q", label, strings.TrimSpace(test.instance))
			}
			if processEpoch != epoch {
				t.Fatalf("process epoch = %q, want %q", processEpoch, epoch)
			}
		})
	}
}

func TestLossWindowStartNarrowsToWhatTheEvidenceSupports(t *testing.T) {
	t.Parallel()
	started := time.Unix(1700000000, 0).UTC()
	checkpoint := started.Add(30 * time.Second)
	early := started.Add(10 * time.Second)
	late := started.Add(45 * time.Second)
	cases := []struct {
		name        string
		firstLossAt *time.Time
		changed     bool
		want        time.Time
	}{
		{name: "loss within one epoch cannot predate the last checkpoint",
			firstLossAt: &early, want: checkpoint},
		{name: "loss after the last checkpoint keeps its own timestamp",
			firstLossAt: &late, want: late},
		{name: "loss without a timestamp opens at the last checkpoint", want: checkpoint},
		{name: "a new epoch reports from its own first loss",
			firstLossAt: &early, changed: true, want: early},
		{name: "a new epoch without a loss timestamp opens when it started",
			changed: true, want: started},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := Snapshot{StartedAt: started, FirstLossAt: test.firstLossAt}
			if got := lossWindowStart(snapshot, test.changed, checkpoint); !got.Equal(test.want) {
				t.Fatalf("lossWindowStart = %s, want %s", got, test.want)
			}
		})
	}
}

func TestAttemptRoutingReadsThePricingPin(t *testing.T) {
	t.Parallel()
	revision := "018f3a5c-0000-7000-8000-00000000000a"
	pinned := "018f3a5c-0000-7000-8000-00000000000b"
	policy := func(raw string) *json.RawMessage {
		message := json.RawMessage(raw)
		return &message
	}
	cases := []struct {
		name       string
		routing    *Routing
		wantPinned bool
		wantVendor string
		wantErr    bool
	}{
		{name: "an attempt without routing has no pin"},
		{name: "routing alone pins nothing",
			routing: &Routing{ProviderRevisionID: revision}},
		{name: "a recorded pricing revision pins the catalogue",
			routing:    &Routing{ProviderRevisionID: revision, PricingRevisionID: &pinned},
			wantPinned: true},
		{name: "a policy may pin pricing without naming a revision",
			routing: &Routing{ProviderRevisionID: revision,
				Policy: policy(`{"pricing_pinned":true,"vendor_id":"google"}`)},
			wantPinned: true, wantVendor: "google"},
		{name: "an unpinned policy carries only its vendor",
			routing: &Routing{ProviderRevisionID: revision,
				Policy: policy(`{"pricing_pinned":false,"vendor_id":"openai"}`)},
			wantVendor: "openai"},
		{name: "a malformed policy is not guessed at",
			routing: &Routing{ProviderRevisionID: revision, Policy: policy(`{"pricing_pinned":`)},
			wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pin, err := attemptRouting(&Attempt{Routing: test.routing})
			if test.wantErr {
				if !errors.Is(err, ErrInvalidEvent) {
					t.Fatalf("error = %v, want ErrInvalidEvent", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("attemptRouting: %v", err)
			}
			if pin.pinned != test.wantPinned {
				t.Fatalf("pinned = %v, want %v", pin.pinned, test.wantPinned)
			}
			vendor := ""
			if pin.vendorID != nil {
				vendor = *pin.vendorID
			}
			if vendor != test.wantVendor {
				t.Fatalf("vendor = %q, want %q", vendor, test.wantVendor)
			}
			if test.routing == nil {
				if pin.providerRevisionID != nil {
					t.Fatalf("provider revision = %v, want nil", *pin.providerRevisionID)
				}
				return
			}
			if pin.providerRevisionID == nil || *pin.providerRevisionID != revision {
				t.Fatalf("provider revision = %v, want %s", pin.providerRevisionID, revision)
			}
		})
	}
}
