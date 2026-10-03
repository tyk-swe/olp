//go:build bench

package bench_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
)

// These tests cover how a run is judged: what makes it a reference run, what
// makes it valid, and how the figures its targets rest on are counted. Like
// harness_test.go they need no services.

// judged is s1Result taken through everything runScenario does after the load:
// the error rates, the validity and the targets.
func judged(r *result) *result {
	computeErrors(r)
	judgeValidity(r)
	r.Targets = evaluateTargets(r)
	return r
}

func TestHarnessAccountsForRequestMetadata(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		sent, rejected, delivered  int64
		admitted, missing, surplus int64
		completeness               float64
	}{
		{name: "all delivered", sent: 100, delivered: 100, admitted: 100, completeness: 1},
		{name: "shed requests leave no metadata", sent: 100, rejected: 3, delivered: 97, admitted: 97, completeness: 1},
		{name: "some missing", sent: 100, rejected: 3, delivered: 90, admitted: 97, missing: 7, completeness: 90.0 / 97},
		{name: "more than were sent", sent: 100, rejected: 3, delivered: 100, admitted: 97, surplus: 3, completeness: 1},
		{name: "nothing sent", admitted: 0, completeness: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := metadataResult{Sent: tc.sent, RejectedBeforeAdmission: tc.rejected, Delivered: tc.delivered}
			m.account()
			if m.Admitted != tc.admitted || m.Missing != tc.missing || m.Surplus != tc.surplus || m.Completeness != tc.completeness {
				t.Errorf("got admitted %d, missing %d, surplus %d, completeness %v; want %d, %d, %d, %v",
					m.Admitted, m.Missing, m.Surplus, m.Completeness, tc.admitted, tc.missing, tc.surplus, tc.completeness)
			}
		})
	}
	if got := admittedRequests(1000, 40); got != 960 {
		t.Errorf("admittedRequests = %d, want the 960 the gateway did not shed", got)
	}
}

func TestHarnessJudgesWhetherARunIsAReferenceRun(t *testing.T) {
	pinned := settings{GatewayCPUs: "0-1", MockCPUs: "2-3", LoadgenCPUs: "4-7"}
	for _, tc := range []struct {
		name  string
		scale float64
		s     settings
		// base and run are whether each load run delivered its schedule.
		base, run bool
		want      referenceConditions
	}{
		{name: "a full-scale pinned run", scale: 1, s: pinned, base: true, run: true,
			want: referenceConditions{FullScale: true, GatewayPinned: true, MockPinned: true, LoadgenPinned: true, PinsDisjoint: true, ValidRuns: true, All: true}},
		{name: "a smoke run", scale: 0.02, s: pinned, base: true, run: true,
			want: referenceConditions{GatewayPinned: true, MockPinned: true, LoadgenPinned: true, PinsDisjoint: true, ValidRuns: true}},
		{name: "an unpinned gateway", scale: 1, s: settings{MockCPUs: "2-3", LoadgenCPUs: "4-7"}, base: true, run: true,
			want: referenceConditions{FullScale: true, MockPinned: true, LoadgenPinned: true, PinsDisjoint: true, ValidRuns: true}},
		{name: "an unpinned mock", scale: 1, s: settings{GatewayCPUs: "0-1", LoadgenCPUs: "4-7"}, base: true, run: true,
			want: referenceConditions{FullScale: true, GatewayPinned: true, LoadgenPinned: true, PinsDisjoint: true, ValidRuns: true}},
		{name: "an unpinned load generator", scale: 1, s: settings{GatewayCPUs: "0-1", MockCPUs: "2-3"}, base: true, run: true,
			want: referenceConditions{FullScale: true, GatewayPinned: true, MockPinned: true, PinsDisjoint: true, ValidRuns: true}},
		{name: "nothing pinned", scale: 1, s: settings{}, base: true, run: true,
			want: referenceConditions{FullScale: true, PinsDisjoint: true, ValidRuns: true}},
		{name: "a gateway and a mock on one CPU", scale: 1, s: settings{GatewayCPUs: "0-1", MockCPUs: "1", LoadgenCPUs: "4-7"}, base: true, run: true,
			want: referenceConditions{FullScale: true, GatewayPinned: true, MockPinned: true, LoadgenPinned: true, ValidRuns: true}},
		{name: "every process on the same CPUs", scale: 1, s: settings{GatewayCPUs: "0-1", MockCPUs: "0-1", LoadgenCPUs: "0-1"}, base: true, run: true,
			want: referenceConditions{FullScale: true, GatewayPinned: true, MockPinned: true, LoadgenPinned: true, ValidRuns: true}},
		{name: "an invalid baseline", scale: 1, s: pinned, base: false, run: true,
			want: referenceConditions{FullScale: true, GatewayPinned: true, MockPinned: true, LoadgenPinned: true, PinsDisjoint: true}},
		{name: "an invalid gateway run", scale: 1, s: pinned, base: true, run: false,
			want: referenceConditions{FullScale: true, GatewayPinned: true, MockPinned: true, LoadgenPinned: true, PinsDisjoint: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s1Result(1, 1)
			r.Scale, r.Baseline.Valid, r.Run.Valid = tc.scale, tc.base, tc.run
			judgeReference(r, tc.s)
			if r.Reference != tc.want {
				t.Errorf("got %+v, want %+v", r.Reference, tc.want)
			}
		})
	}
}

func TestHarnessFindsCPUsThatProcessesShare(t *testing.T) {
	set, err := cpuSet("0-2,5")
	if err != nil || len(set) != 4 || !set[0] || !set[2] || !set[5] || set[3] {
		t.Errorf("cpuSet = %v, %v", set, err)
	}
	if _, err := cpuSet("2-1"); err == nil {
		t.Error("a descending range was accepted")
	}
	for _, tc := range []struct {
		name string
		s    settings
		want []string
	}{
		{name: "disjoint", s: settings{GatewayCPUs: "0-1", MockCPUs: "2-3", LoadgenCPUs: "4-7"}},
		{name: "adjacent is not shared", s: settings{GatewayCPUs: "0-1", MockCPUs: "2"}},
		{name: "nothing pinned", s: settings{}},
		{name: "one pinned process shares with nothing", s: settings{GatewayCPUs: "0-7"}},
		{name: "gateway and mock", s: settings{GatewayCPUs: "0-3", MockCPUs: "3-4", LoadgenCPUs: "6"}, want: []string{"the gateway and the mock upstream both run on CPU 3"}},
		{name: "mock and generator", s: settings{GatewayCPUs: "0", MockCPUs: "2-5", LoadgenCPUs: "4,5,9"}, want: []string{"the mock upstream and the load generator both run on CPU 4,5"}},
		{name: "all three", s: settings{GatewayCPUs: "0-1", MockCPUs: "1-2", LoadgenCPUs: "0,2"}, want: []string{
			"the gateway and the mock upstream both run on CPU 1",
			"the gateway and the load generator both run on CPU 0",
			"the mock upstream and the load generator both run on CPU 2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharedCPUs(tc.s); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// A run that shares a CPU is not a reference run, and enforcement says so.
	r := s1Result(1, 1)
	judgeReference(r, settings{GatewayCPUs: "0-1", MockCPUs: "1", LoadgenCPUs: "2-3"})
	if r.Reference.All || r.Reference.PinsDisjoint {
		t.Fatalf("a gateway and a mock on one CPU made a reference run: %+v", r.Reference)
	}
	if failures := strings.Join(enforcementFailures(r), "\n"); !strings.Contains(failures, "CPU in common") {
		t.Errorf("enforcement does not name the shared CPU:\n%s", failures)
	}
}

// withFailures makes some of a full-scale run's requests fail, as an
// overloaded or broken gateway would.
func withFailures(r *result, failed int64) {
	r.Run.Requests.Failed = failed
	r.Run.Requests.Succeeded = r.Run.Requests.Sent - failed
	r.Run.Rates.ErrorRate = float64(failed) / float64(r.Run.Requests.Sent)
	r.Run.Rates.SuccessRate = float64(r.Run.Requests.Succeeded) / float64(r.Run.Requests.Scheduled)
}

// TestHarnessDoesNotJudgeLatencyOfRequestsThatFailed is the survivor problem:
// a gateway that rejects most requests quickly answers the rest as fast as it
// can, and a latency target judged on those answers would be met by failing.
func TestHarnessDoesNotJudgeLatencyOfRequestsThatFailed(t *testing.T) {
	broken := s1Result(1.5, 4)
	withFailures(broken, 59400)
	judged(broken)
	if broken.Validity.Valid || !strings.Contains(strings.Join(broken.Validity.Problems, "\n"), "99.00%") {
		t.Fatalf("a run that failed 99%% of its requests was valid: %+v", broken.Validity)
	}
	for _, id := range []string{"s1-2vcpu-added-p95", "s1-2vcpu-added-p99"} {
		if got := findTarget(t, broken.Targets, id); got.Status != statusNotChecked || got.MeetsTarget != nil || !strings.Contains(got.Reason, "99.00%") {
			t.Errorf("%s was judged on the survivors: %+v", id, got)
		}
	}
	if failures := enforcementFailures(broken); len(failures) < 3 {
		t.Errorf("enforcement lets a failing run through: %v", failures)
	}

	// The limit is a tenth of a percent of the requests, on either run.
	for _, tc := range []struct {
		failed int64
		valid  bool
	}{{0, true}, {60, true}, {61, false}} {
		r := s1Result(1, 1)
		withFailures(r, tc.failed)
		if judged(r); r.Validity.Valid != tc.valid {
			t.Errorf("%d of 60,000 requests failed: valid %v, want %v: %v", tc.failed, r.Validity.Valid, tc.valid, r.Validity.Problems)
		}
	}
	baseline := s1Result(1, 1)
	baseline.Baseline.Rates.ErrorRate = 0.5
	if judged(baseline); baseline.Validity.Valid || !strings.Contains(strings.Join(baseline.Validity.Problems, "\n"), "baseline") {
		t.Errorf("a baseline that failed half its requests was valid: %+v", baseline.Validity)
	}

	// A failing S3 run still misses its success target rather than leaving it
	// unchecked: the failures are the finding.
	s3 := s1Result(1, 1)
	s3.Scenario = "S3"
	s3.Run.Rates = loadgen.Rates{TargetRPS: 3000, OfferedRPS: 3000, Ratio: 1, SuccessRate: 0.9}
	s3.Run.Rates.ErrorRate = 0.1
	judged(s3)
	if s3.Validity.Valid {
		t.Error("an S3 run that failed a tenth of its requests was valid")
	}
	if got := findTarget(t, s3.Targets, "s3-success-rate"); got.Status != statusMissed {
		t.Errorf("failures must miss the success target, not leave it unchecked: %+v", got)
	}

	// S6's streams are canceled when it ends, which is how it ends and not a
	// failure, and a stream that ended some other way is that scenario's
	// finding, reported with it, so neither makes the run doubtful.
	held := s1Result(1, 1)
	held.Plan.SlowReadBPS = 8192
	held.Run.Requests.Failed = held.Run.Requests.Sent
	held.Run.Errors.ByKind = map[string]int64{"canceled": held.Run.Requests.Sent - 600, "truncated": 600}
	judged(held)
	if joined := strings.Join(held.Validity.Problems, "\n"); strings.Contains(joined, "failed") {
		t.Errorf("held streams were counted as failures: %s", joined)
	}
}

// TestHarnessDoesNotJudgeNegativeAddedLatency: a gateway cannot be faster than
// the upstream it calls, so a negative difference means the baseline was the
// slower session, and meeting a target by it would be meeting it by noise.
func TestHarnessDoesNotJudgeNegativeAddedLatency(t *testing.T) {
	// The baseline's p95 is 21 ms, so jitter up to 2.1 ms is allowed.
	for _, tc := range []struct {
		name         string
		p95, p99     float64
		valid        bool
		warned       bool
		p99Unchecked bool
		mentionsMsg  string
	}{
		{name: "positive", p95: 1.5, p99: 4, valid: true},
		{name: "jitter at p95", p95: -1.5, p99: 1, valid: true},
		{name: "negative at p95", p95: -3, p99: 1, mentionsMsg: "-3.00 ms at p95"},
		{name: "a negative tail is only noted, and its target is left unchecked", p95: 1, p99: -5, valid: true, warned: true, p99Unchecked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := judged(s1Result(tc.p95, tc.p99))
			if r.Validity.Valid != tc.valid {
				t.Fatalf("valid %v, want %v: %v", r.Validity.Valid, tc.valid, r.Validity.Problems)
			}
			if tc.mentionsMsg != "" && !strings.Contains(strings.Join(r.Validity.Problems, "\n"), tc.mentionsMsg) {
				t.Errorf("the problems do not say what was wrong: %v", r.Validity.Problems)
			}
			if got := strings.Contains(strings.Join(r.Validity.Warnings, "\n"), "below zero"); got != tc.warned {
				t.Errorf("warned %v, want %v: %v", got, tc.warned, r.Validity.Warnings)
			}
			target := findTarget(t, r.Targets, "s1-2vcpu-added-p95")
			if tc.valid && target.Status != statusMet {
				t.Errorf("a valid run was not judged: %+v", target)
			}
			if !tc.valid && (target.Status != statusNotChecked || target.MeetsTarget != nil) {
				t.Errorf("the target was judged on a negative added latency: %+v", target)
			}
			// A tail that is only noted still cannot meet its own limit by being
			// negative.
			if p99 := findTarget(t, r.Targets, "s1-2vcpu-added-p99"); tc.p99Unchecked && (p99.Status != statusNotChecked || p99.MeetsTarget != nil || !strings.Contains(p99.Reason, "below zero")) {
				t.Errorf("the p99 target was judged on a negative added latency: %+v", p99)
			}
		})
	}

	// A negative p50 gates as well, and the population judged is the scenario's:
	// a streaming scenario reads its stream latencies.
	stream := s1Result(1, 1)
	stream.Plan.StreamShare = 1
	stream.Baseline.Latency.Stream = summary1(100, 120, 130)
	stream.Run.Latency.Stream = summary1(85, 121, 131)
	computeAdded(stream)
	if judged(stream); stream.Validity.Valid || !strings.Contains(strings.Join(stream.Validity.Problems, "\n"), "-15.00 ms at p50") {
		t.Errorf("a stream p50 15 ms under its baseline was valid: %+v", stream.Validity)
	}
}

func TestHarnessCountsOnlySuccessfulRequestsPerCPUSecond(t *testing.T) {
	run := report(1000, true, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{})
	run.Requests.Sent, run.Requests.Succeeded, run.Requests.Failed = 1000, 600, 400
	run.Requests.WarmupSent, run.Requests.WarmupFailed = 100, 20
	if got := handledRequests(run, false); got != 680 {
		t.Errorf("600 measured and 80 warmup requests succeeded, and %d were counted", got)
	}
	// Held streams end by cancellation, so each stream the gateway took counts.
	if got := handledRequests(run, true); got != 1100 {
		t.Errorf("held streams counted %d, want all 1100 taken", got)
	}

	r := &result{Baseline: report(1000, true, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{}), Run: run, Gateway: gatewayInfo{VCPUs: 2}}
	computeThroughput(r, resourceUse{Gateway: cpuUse{Seconds: 68}, WallSeconds: 70})
	// 68 CPU seconds for the 680 requests it answered: 100 ms each, ten to a
	// CPU second, whatever it did with the 420 it failed.
	if tp := r.Throughput; tp.CPUMsPerRequestGross != 100 || tp.RequestsPerCPUSecond != 10 {
		t.Errorf("throughput %+v", tp)
	}
}

func TestHarnessPeakRSSFollowsTheResetOnlyWhenItWorked(t *testing.T) {
	if got := peakRSS(true, 300, 180); got != 300 {
		t.Errorf("a reset high-water mark of 300 MiB was read as %v", got)
	}
	// Refused, the mark covers start-up too and says nothing of the run.
	if got := peakRSS(false, 900, 180); got != 180 {
		t.Errorf("an unreset mark of 900 MiB was read as %v, want the sampled 180", got)
	}
	r := judged(s1Result(1, 1))
	if !strings.Contains(strings.Join(r.Validity.Warnings, "\n"), "refused to reset") {
		t.Errorf("a run whose peak was not reset carries no warning: %v", r.Validity.Warnings)
	}
	r = s1Result(1, 1)
	r.Resources.PeakResetOK = true
	if judged(r); strings.Contains(strings.Join(r.Validity.Warnings, "\n"), "refused to reset") {
		t.Errorf("a reset peak is warned of: %v", r.Validity.Warnings)
	}
}

// TestHarnessKnowsWhenASnapshotIsCertainlyNewer: the age is served in whole
// seconds, so a snapshot that reports an age of 0 may be a second old and is
// not known to be newer than something half a second ago.
func TestHarnessKnowsWhenASnapshotIsCertainlyNewer(t *testing.T) {
	now := time.Now()
	at := func(age float64) metrics { return metrics{seriesSnapshotAge: age} }
	for _, tc := range []struct {
		name  string
		m     metrics
		after time.Time
		want  bool
	}{
		{"an age of 0 against half a second ago", at(0), now.Add(-500 * time.Millisecond), false},
		{"an age of 0 against a second ago", at(0), now.Add(-time.Second), false},
		{"an age of 0 against two seconds ago", at(0), now.Add(-2 * time.Second), true},
		{"an age of 1 against two seconds ago", at(1), now.Add(-2 * time.Second), false},
		{"an age of 1 against three seconds ago", at(1), now.Add(-3 * time.Second), true},
		{"an age before the end", at(30), now.Add(-3 * time.Second), false},
		{"no snapshot yet", at(1.8446744073709552e19), now.Add(-time.Hour), false},
	} {
		if got := snapshotAfter(tc.m, now, tc.after); got != tc.want {
			t.Errorf("%s: snapshotAfter = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHarnessReadsTheAgeOfTheGatewaysMetricsSnapshot(t *testing.T) {
	now := time.Now()
	taken, known := snapshotTaken(metrics{seriesSnapshotAge: 7.5}, now)
	if !known || !taken.Equal(now.Add(-7500*time.Millisecond)) {
		t.Errorf("taken %v, known %v", taken, known)
	}
	// A gateway that has taken no snapshot reports 2^64-1 seconds.
	if _, known := snapshotTaken(metrics{seriesSnapshotAge: 1.8446744073709552e19}, now); known {
		t.Error("an age of 2^64-1 seconds was taken for a snapshot")
	}
}

// TestHarnessWaitsForMetricsTakenAfterTheRun serves a gateway whose metrics
// snapshot is older than the end of the run on its first scrapes and newer on a
// later one, and one whose snapshot never is.
func TestHarnessWaitsForMetricsTakenAfterTheRun(t *testing.T) {
	var scrapes atomic.Int32
	serve := func(freshAfter int32) *gatewayProcess {
		scrapes.Store(0)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := scrapes.Add(1)
			age := 30.0
			if freshAfter > 0 && n >= freshAfter {
				age = 0
			}
			fmt.Fprintf(w, "%s %v\n%s %v\n", seriesSnapshotAge, age, seriesFailOpen, n)
		}))
		t.Cleanup(server.Close)
		return &gatewayProcess{private: server.URL}
	}

	g := serve(3)
	m, fresh := metricsAfter(t, g, time.Now().Add(-3*time.Second), 10*time.Second)
	if !fresh || m[seriesFailOpen] != 3 {
		t.Errorf("fresh %v after reading %v: want the third scrape, the first taken after the run", fresh, m)
	}

	// A snapshot that reports an age of 0 may be a second old, so within the
	// second after a run ended it is not known to be newer than the run.
	g = serve(1)
	if m, fresh = metricsAfter(t, g, time.Now(), 300*time.Millisecond); fresh {
		t.Errorf("fresh after reading %v: a snapshot that may predate the end of the run was taken for one after it", m)
	}

	// A snapshot that is never newer comes back as such, with what was read, so
	// that the target is left unchecked rather than judged on it.
	g = serve(0)
	m, fresh = metricsAfter(t, g, time.Now(), 1200*time.Millisecond)
	if fresh || m[seriesFailOpen] < 2 {
		t.Errorf("fresh %v after reading %v: a snapshot older than the run was taken for a fresh one", fresh, m)
	}
}

func TestHarnessSummarizesFailover(t *testing.T) {
	p := plan{Models: []string{"model-a", "model-b"}, Baseline: "model-b", Failover: true}
	first := p.failingModel()
	rows := []attemptRow{
		{UpstreamModel: first, Ordinal: 1, Attempts: 100},
		{UpstreamModel: "model-b", Ordinal: 2, Attempts: 90},
		{UpstreamModel: "model-b", Ordinal: 1, Attempts: 10},
		{UpstreamModel: "other", Ordinal: 3, Attempts: 5},
	}
	f := summarizeFailover(p, rows, 100)
	if f.FirstTargetAttempts != 100 || f.SecondTargetAttempts != 100 {
		t.Errorf("attempts by target: %+v", f)
	}
	// The attempts after a request's first are the ones that failed over: the
	// second attempt of 90 requests and the third of 5, and not the first.
	if f.RequestsFailedOver != 95 || f.FailedOverShare != 0.95 {
		t.Errorf("failed over %d (%.2f), want 95", f.RequestsFailedOver, f.FailedOverShare)
	}
	if empty := summarizeFailover(p, nil, 0); empty.FailedOverShare != 0 || empty.RequestsFailedOver != 0 {
		t.Errorf("no requests: %+v", empty)
	}
}

// TestHarnessJudgesS1OnlyForAGatewayWithCPUsOfItsOwn: the target is stated for a
// gateway on two CPUs, and a gateway that may use two, on a machine that has
// only two, shares them with the mock upstream and the load generator unless
// all three are pinned apart.
func TestHarnessJudgesS1OnlyForAGatewayWithCPUsOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*referenceConditions)
		// reason is what the target says is missing, "" when it is judged.
		reason string
	}{
		{"every process pinned apart", func(*referenceConditions) {}, ""},
		{"an unpinned gateway", func(c *referenceConditions) { c.GatewayPinned = false }, envGatewayCPUs},
		{"an unpinned mock upstream", func(c *referenceConditions) { c.MockPinned = false }, envMockCPUs},
		{"an unpinned load generator", func(c *referenceConditions) { c.LoadgenPinned = false }, envLoadgenCPUs},
		{"a CPU in common", func(c *referenceConditions) { c.PinsDisjoint = false }, "CPU in common"},
		{"nothing pinned", func(c *referenceConditions) {
			*c = referenceConditions{FullScale: true, PinsDisjoint: true, ValidRuns: true}
		}, envGatewayCPUs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s1Result(1, 1)
			// Two CPUs by affinity, as on a two-CPU host or in a two-CPU cgroup.
			r.Gateway.VCPUs = s1VCPUs
			tc.change(&r.Reference)
			r.Targets = evaluateTargets(r)
			for _, id := range []string{"s1-2vcpu-added-p95", "s1-2vcpu-added-p99"} {
				got := findTarget(t, r.Targets, id)
				if tc.reason == "" {
					if got.Status != statusMet {
						t.Errorf("%s: %+v", id, got)
					}
				} else if got.Status != statusNotChecked || got.MeetsTarget != nil || !strings.Contains(got.Reason, tc.reason) {
					t.Errorf("%s was judged though its processes were not pinned apart: %+v", id, got)
				}
			}
		})
	}
}

// TestHarnessJudgesWhetherTheStreamsWereHeld is the premise of S6, at the
// edges: the generator must have had 95% of the streams open at once, and the
// gateway 90%. Both are worked out as the scenario works them out.
func TestHarnessJudgesWhetherTheStreamsWereHeld(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inFlight int
		admitted float64
		// problem is what the problems say, "" for a run that held its streams.
		problem string
	}{
		{"the generator and the gateway held all of them", 10_000, 10_000, ""},
		{"the generator held exactly 95%", 9_500, 10_000, ""},
		{"the generator held a stream short of 95%", 9_499, 10_000, "the generator held 9499 of the 10000 streams"},
		{"the gateway held exactly 90%", 10_000, 9_000, ""},
		{"the gateway held a stream short of 90%", 10_000, 8_999, "the gateway held 8999 of the 10000 streams"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := report(1000, true, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{})
			run.Requests.PeakInFlight = tc.inFlight
			r := s1Result(1, 1)
			r.Streams = summarizeStreams(settings{}, plan{Concurrency: 10_000}, run, resourceUse{PeakInferenceAdmitted: tc.admitted})
			judgeValidity(r)
			problems := strings.Join(r.Validity.Problems, "\n")
			if tc.problem == "" && strings.Contains(problems, "streams") || !strings.Contains(problems, tc.problem) {
				t.Errorf("problems %q, want %q", problems, tc.problem)
			}
			if held := tc.problem == ""; r.Validity.Valid != held {
				t.Errorf("valid %v, want %v: %v", r.Validity.Valid, held, r.Validity.Problems)
			}
		})
	}
}

// TestHarnessAddsLatencyOnlyWhereBothRunsHaveRequests: a run, or a baseline,
// with no successful request of a kind has no latency of it to subtract, and
// the other's figures are not a gateway's overhead.
func TestHarnessAddsLatencyOnlyWhereBothRunsHaveRequests(t *testing.T) {
	some, none := summary1(20, 21, 22), loadgen.Summary{}
	if got := deltaOf(some, summary1(21, 23, 27)); got == nil || got.P50 != 1 || got.P95 != 2 || got.P99 != 5 {
		t.Errorf("both have requests: %+v", got)
	}
	for name, got := range map[string]*percentiles{
		"only the baseline": deltaOf(some, none),
		"only the run":      deltaOf(none, some),
		"neither":           deltaOf(none, none),
	} {
		if got != nil {
			t.Errorf("%s has requests, and the difference is %+v", name, got)
		}
	}

	// Through computeAdded: the run answered streams the baseline did not, and
	// the unary requests both did.
	r := &result{Scenario: "S3",
		Baseline: report(10, true, some, some, none, none),
		Run:      report(10, true, summary1(30, 31, 32), some, summary1(40, 41, 42), none)}
	computeAdded(r)
	if r.AddedLatency.Unary == nil || r.AddedLatency.Stream != nil || r.AddedLatency.All == nil || r.AddedLatency.All.P50 != 10 {
		t.Errorf("added latency %+v", r.AddedLatency)
	}
}

// TestHarnessJudgesWhetherS4MeasuredAFailover: the scenario measures what it
// costs to fail over only if the failing first target was tried and failed. A
// run in which it was not armed is a plain unary run, and it is neither
// valid nor to be compared as a failover.
func TestHarnessJudgesWhetherS4MeasuredAFailover(t *testing.T) {
	failover := func(first, failedOver, injected int64) *result {
		r := s1Result(1, 1)
		r.Scenario, r.Plan.Failover = "S4", true
		r.Failover = &failoverResult{FirstTargetAttempts: first, RequestsFailedOver: failedOver}
		r.MockStats.RunInjectedErrors = injected
		return judged(r)
	}
	if r := failover(5, 5, 5); !r.Validity.Valid {
		t.Errorf("a run that failed over was not valid: %v", r.Validity.Problems)
	}
	for _, tc := range []struct {
		name string
		r    *result
		want string
	}{
		{"a rule that was not armed, and a first target never tried", failover(0, 0, 0), "no request tried the failing first target"},
		{"a first target that was tried and did not fail", failover(240, 0, 0), "injected no error"},
		{"errors that no request failed over from", failover(240, 0, 5), "no request failed over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if problems := strings.Join(tc.r.Validity.Problems, "\n"); tc.r.Validity.Valid || !strings.Contains(problems, tc.want) {
				t.Errorf("valid %v, problems %q, want one with %q", tc.r.Validity.Valid, problems, tc.want)
			}
		})
	}
	// A run with no failover summary at all did not measure one either, and a
	// scenario that is not a failover is not held to it.
	missing := s1Result(1, 1)
	missing.Plan.Failover = true
	if judged(missing); missing.Validity.Valid {
		t.Error("a failover run with no failover summary was valid")
	}
	if plain := judged(s1Result(1, 1)); !plain.Validity.Valid {
		t.Errorf("a run that is not a failover was held to one: %v", plain.Validity.Problems)
	}
}

// TestHarnessJudgesWhetherS3ChargedItsKey: S3's budget is real, and a key that
// accrued no cost means the run did none of the reservation and settlement the
// scenario is for, whatever its success rate.
func TestHarnessJudgesWhetherS3ChargedItsKey(t *testing.T) {
	budgeted := func(b *budgetResult) *result {
		r := s1Result(1, 1)
		r.Scenario, r.Plan.Budget, r.Budget = "S3", true, b
		r.Run.Rates = loadgen.Rates{TargetRPS: 3000, OfferedRPS: 3000, Ratio: 1, SuccessRate: 1}
		return judged(r)
	}
	charged := budgeted(&budgetResult{DailyAccrued: "12.3450", MonthlyAccrued: "12.3450"})
	if !charged.Validity.Valid || findTarget(t, charged.Targets, "s3-success-rate").Status != statusMet {
		t.Errorf("a key that was charged: %+v %+v", charged.Validity, charged.Targets)
	}
	// Across midnight UTC the day's accrual may be nothing while the month's is not.
	if r := budgeted(&budgetResult{DailyAccrued: "0", MonthlyAccrued: "0.0001"}); !r.Validity.Valid {
		t.Errorf("a month that accrued: %v", r.Validity.Problems)
	}
	for _, tc := range []struct {
		name   string
		budget *budgetResult
		want   string
	}{
		{"a key that accrued nothing", &budgetResult{DailyAccrued: "0", MonthlyAccrued: "0.000000"}, "accrued no cost"},
		{"a budget that was not read", nil, "was not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := budgeted(tc.budget)
			if problems := strings.Join(r.Validity.Problems, "\n"); r.Validity.Valid || !strings.Contains(problems, tc.want) {
				t.Errorf("valid %v, problems %q", r.Validity.Valid, problems)
			}
			// Left unchecked rather than met, though every request succeeded.
			if got := findTarget(t, r.Targets, "s3-success-rate"); got.Status != statusNotChecked || got.MeetsTarget != nil || !strings.Contains(got.Reason, tc.want) {
				t.Errorf("the success target was judged on a run that did no budget work: %+v", got)
			}
		})
	}
	unpriced := budgeted(&budgetResult{DailyAccrued: "5", MonthlyAccrued: "5", UnpricedAttempts: 7})
	if !unpriced.Validity.Valid || !strings.Contains(strings.Join(unpriced.Validity.Warnings, "\n"), "7 of the key's attempts had no price") {
		t.Errorf("unpriced attempts are a warning: %+v", unpriced.Validity)
	}
	// A scenario without a budget is not asked for one.
	if plain := judged(s1Result(1, 1)); !plain.Validity.Valid {
		t.Errorf("a run with no budget was held to one: %v", plain.Validity.Problems)
	}
}

func TestHarnessReadsTheBudgetOfAKey(t *testing.T) {
	b, err := parseKeyBudget([]byte(`{"id":"k","budget":{"daily":{"accrued":"12.340000","limit":"1000000","window_ends_at":"2026-10-03T00:00:00Z"},` +
		`"monthly":{"accrued":"45.6","limit":"1000000","window_ends_at":"2026-11-01T00:00:00Z"},"enforcement_active":true,"unpriced_attempts":3}}`))
	if err != nil || b.DailyAccrued != "12.340000" || b.MonthlyAccrued != "45.6" || b.UnpricedAttempts != 3 || !b.accrued() {
		t.Errorf("%+v, %v", b, err)
	}
	if empty, err := parseKeyBudget([]byte(`{"budget":{"daily":{"accrued":"0"},"monthly":{"accrued":"0"},"unpriced_attempts":0}}`)); err != nil || empty.accrued() {
		t.Errorf("a budget that accrued nothing: %+v, %v", empty, err)
	}
	for name, raw := range map[string]string{
		"no budget":         `{"id":"k"}`,
		"a non-decimal":     `{"budget":{"daily":{"accrued":"lots"},"monthly":{"accrued":"0"}}}`,
		"a missing accrual": `{"budget":{"daily":{},"monthly":{"accrued":"0"}}}`,
		"not an object":     `[]`,
	} {
		if b, err := parseKeyBudget([]byte(raw)); err == nil {
			t.Errorf("%s was read as %+v", name, b)
		}
	}
	var none *budgetResult
	if none.accrued() {
		t.Error("no budget accrued")
	}
}

// TestHarnessCountsAllocationsPerRequest: the figure is what the gateway
// allocated while the run lasted, less what it allocates idle over the same
// time, over the requests it answered, as its CPU time per request is.
func TestHarnessCountsAllocationsPerRequest(t *testing.T) {
	t0 := time.Now()
	mark := func(seconds, objects, bytes float64) cpuMark {
		return cpuMark{at: t0.Add(time.Duration(seconds * float64(time.Second))), allocs: objects, allocBytes: bytes, allocsKnown: true}
	}
	// Idle for 10 s the gateway allocated 5,000 objects and 1 MB: 500 objects
	// and 100 kB a second. The run lasted 100 s and allocated 5,050,000 objects
	// and 1 GB, of which the idle share is 50,000 objects and 10 MB, and it
	// answered 10,000 requests.
	idleFrom, idleTo := mark(0, 1_000_000, 10e6), mark(10, 1_005_000, 11e6)
	start, end := mark(20, 1_100_000, 50e6), mark(120, 6_150_000, 1_050e6)
	got := allocationsPerRequest(start, end, idleFrom, idleTo, 10_000)
	if got.Value == nil || got.BytesPerRequest == nil || *got.Value != 500 || *got.BytesPerRequest != 99_000 || got.IdleObjectsPerSecond != 500 || got.Reason != "" {
		t.Fatalf("got %+v, want 500 objects and 99,000 bytes a request", got)
	}
	// Twice the requests over the same allocation is half the figure.
	if got := allocationsPerRequest(start, end, idleFrom, idleTo, 20_000); got.Value == nil || *got.Value != 250 {
		t.Errorf("20,000 requests: %+v", got)
	}

	// Without an honest figure it is null beside the reason, never a number.
	unknown := idleTo
	unknown.allocsKnown = false
	reversed := end
	reversed.allocs = 10
	for _, tc := range []struct {
		name string
		got  allocationsResult
		want string
	}{
		{"a mark that could not be read", allocationsPerRequest(start, end, idleFrom, unknown, 10_000), "could not be read"},
		{"no request answered", allocationsPerRequest(start, end, idleFrom, idleTo, 0), "no request"},
		{"a counter that went backwards", allocationsPerRequest(start, reversed, idleFrom, idleTo, 10_000), "went backwards"},
		{"a run that allocated no more than idle", allocationsPerRequest(start, mark(120, 1_100_000+50_000, 60e6), idleFrom, idleTo, 10_000), "nothing to attribute"},
		{"a window with no length", allocationsPerRequest(start, mark(20, 2_000_000, 100e6), idleFrom, idleTo, 10_000), "no length"},
	} {
		if tc.got.Value != nil || tc.got.BytesPerRequest != nil || !strings.Contains(tc.got.Reason, tc.want) {
			t.Errorf("%s: %+v, want a null figure and %q", tc.name, tc.got, tc.want)
		}
	}
}
