//go:build bench

package bench_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
)

// These tests cover the harness itself. They need no services, so they run
// wherever the scenarios cannot, and TestHarness* is how to select them.

func TestHarnessScenariosFailWithoutServices(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a subprocess")
	}
	for _, name := range []string{"TestScenarioS1", "TestScenarioS6"} {
		cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v")
		cmd.Env = withoutOLP(os.Environ())
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("%s passed without its services:\n%s", name, out)
		}
		for _, want := range []string{"--- FAIL: " + name, envDatabaseURL, envValkeyURL, envBinary, envMockBinary} {
			if !strings.Contains(string(out), want) {
				t.Errorf("%s: the failure does not mention %q:\n%s", name, want, out)
			}
		}
		if strings.Contains(string(out), "--- SKIP") {
			t.Errorf("%s skipped:\n%s", name, out)
		}
	}
}

func withoutOLP(env []string) []string {
	var kept []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "OLP_") {
			kept = append(kept, kv)
		}
	}
	return kept
}

func TestHarnessSettingsParsing(t *testing.T) {
	for in, want := range map[string]float64{"": 1, "1": 1, "0.05": 0.05, "2.5": 2.5} {
		if got, err := parseScale(in); err != nil || got != want {
			t.Errorf("parseScale(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "-1", "abc", "NaN", "Inf", "1000"} {
		if _, err := parseScale(in); err == nil {
			t.Errorf("parseScale(%q) accepted", in)
		}
	}
	for in, want := range map[string]int{"0": 1, "0-3": 4, "0-3,6": 5, "2,2,3": 2, " 1 , 4-5": 3} {
		if got, err := parseCPUList(in); err != nil || got != want {
			t.Errorf("parseCPUList(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a", "3-1", "1-", "-1", "0-99999", "1,,2"} {
		if _, err := parseCPUList(in); err == nil {
			t.Errorf("parseCPUList(%q) accepted", in)
		}
	}
	for in, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true, "TRUE": true} {
		if got, err := parseFlag(in); err != nil || got != want {
			t.Errorf("parseFlag(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	if _, err := parseFlag("yes"); err == nil {
		t.Error("parseFlag accepted yes")
	}
	if d, err := parseDuration("", 3*time.Second); err != nil || d != 3*time.Second {
		t.Errorf("parseDuration default = %v, %v", d, err)
	}
	if d, err := parseDuration("90s", 0); err != nil || d != 90*time.Second {
		t.Errorf("parseDuration = %v, %v", d, err)
	}
	if _, err := parseDuration("-1s", 0); err == nil {
		t.Error("parseDuration accepted a negative duration")
	}
	if got := scaled(1000, 0.05); got != 50 {
		t.Errorf("scaled(1000, 0.05) = %d", got)
	}
	if got := scaled(3, 0.001); got != 1 {
		t.Errorf("a scaled count dropped to %d, below one", got)
	}
}

func TestHarnessReadsProcStatus(t *testing.T) {
	st, err := parseProcStatus("Name:\tolp\nVmHWM:\t  204800 kB\nVmRSS:\t  102400 kB\nThreads:\t18\nCpus_allowed:\tf\nCpus_allowed_list:\t0-3\n")
	if err != nil || st.RSSKB != 102400 || st.PeakRSSKB != 204800 || st.Threads != 18 || st.CPUsAllowed != "0-3" {
		t.Fatalf("%+v, %v", st, err)
	}
	if _, err := parseProcStatus("Name:\tolp\nThreads:\t3\n"); err == nil {
		t.Error("an incomplete status was accepted")
	}
	// The command name may hold spaces and parentheses; the fields that count
	// start after the last one.
	stat := "1234 (olp (all) x) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 150 0 0 20 0 18 0 5000 1000000 25000 18446744073709551615 0 0 0"
	if got, err := parseCPUSeconds(stat); err != nil || got != 4.0 {
		t.Fatalf("parseCPUSeconds = %v, %v, want 4 (250 + 150 ticks)", got, err)
	}
	if _, err := parseCPUSeconds("1 (x) S 1"); err == nil {
		t.Error("a short stat line was accepted")
	}
	// A real process reads back.
	if _, err := readProcStatus(os.Getpid()); err != nil {
		t.Fatalf("read this process's status: %v", err)
	}
	if _, err := readCPUSeconds(os.Getpid()); err != nil {
		t.Fatalf("read this process's CPU time: %v", err)
	}
}

// A process that ends on its own leaves no watcher to kill a stranger
// that is given its pid.

// report builds a load report for the tests of what is computed from two.
func report(sent int64, valid bool, all, unary, stream, ttft loadgen.Summary) *loadgen.Report {
	r := &loadgen.Report{Valid: valid, Latency: loadgen.Latencies{All: all, Unary: unary, Stream: stream, TTFT: ttft}}
	r.Requests.Sent, r.Requests.Succeeded, r.Requests.Scheduled = sent, sent, sent
	r.Rates = loadgen.Rates{TargetRPS: 1000, OfferedRPS: 1000, Ratio: 1, ThroughputRPS: 1000, SuccessRate: 1}
	r.Errors = loadgen.Errors{ByKind: map[string]int64{}, StatusCodes: map[string]int64{"200": sent}}
	return r
}

func summary1(p50, p95, p99 float64) loadgen.Summary {
	return loadgen.Summary{Count: 100, P50Ms: p50, P95Ms: p95, P99Ms: p99}
}

// s1Result is a full-scale S1 result on a pinned two-vCPU gateway.
func s1Result(added95, added99 float64) *result {
	base := summary1(20, 21, 22)
	run := summary1(21, 21+added95, 22+added99)
	r := &result{Scenario: "S1", Scale: 1,
		Baseline:  report(60000, true, base, base, loadgen.Summary{}, loadgen.Summary{}),
		Run:       report(60000, true, run, run, loadgen.Summary{}, loadgen.Summary{}),
		Gateway:   gatewayInfo{VCPUs: 2},
		Reference: referenceConditions{FullScale: true, GatewayPinned: true, MockPinned: true, LoadgenPinned: true, PinsDisjoint: true, ValidRuns: true, All: true},
		Metadata:  metadataResult{Healthy: true, Drain: drainResult{Settled: true}},
	}
	r.Plan.StreamShare = 0
	computeAdded(r)
	computeThroughput(r, resourceUse{Gateway: cpuUse{Seconds: 100}, WallSeconds: 70})
	return r
}

func findTarget(t *testing.T, targets []targetResult, id string) targetResult {
	t.Helper()
	for _, target := range targets {
		if target.ID == id {
			return target
		}
	}
	t.Fatalf("no target %q among %+v", id, targets)
	return targetResult{}
}

func TestHarnessDoesNotJudgeTargetsItCannotCheck(t *testing.T) {
	// A gateway on four CPUs is not the S1 target's two.
	wide := s1Result(1, 1)
	wide.Gateway.VCPUs = 4
	wide.Targets = evaluateTargets(wide)
	got := findTarget(t, wide.Targets, "s1-2vcpu-added-p95")
	if got.Status != statusNotChecked || got.MeetsTarget != nil || !strings.Contains(got.Reason, envGatewayCPUs) {
		t.Errorf("a four-vCPU gateway was judged against the two-vCPU target: %+v", got)
	}
	if failures := enforcementFailures(wide); len(failures) == 0 {
		t.Error("enforcement passed a run whose target was not checked")
	}

	// A smoke run is not a reference run, whatever its numbers.
	smoke := s1Result(0.1, 0.1)
	smoke.Scale, smoke.Reference = 0.05, referenceConditions{ValidRuns: true}
	smoke.Targets = evaluateTargets(smoke)
	if got := findTarget(t, smoke.Targets, "s1-2vcpu-added-p95"); got.Status != statusNotChecked {
		t.Errorf("a smoke run was judged: %+v", got)
	}
	failures := strings.Join(enforcementFailures(smoke), "\n")
	for _, want := range []string{"scale 0.05", envGatewayCPUs, envMockCPUs, envLoadgenCPUs} {
		if !strings.Contains(failures, want) {
			t.Errorf("enforcement does not mention %q:\n%s", want, failures)
		}
	}

	// An invalid run cannot vouch for a target either.
	invalid := s1Result(0.1, 0.1)
	invalid.Validity = validity{Problems: []string{"the gateway run load did not deliver its schedule"}}
	if failures := enforcementFailures(invalid); len(failures) != 1 {
		t.Errorf("an invalid run's problem is not enforced: %v", failures)
	}
}

func TestHarnessJudgesRequestMetadataLoss(t *testing.T) {
	lost := s1Result(1, 1)
	lost.Metadata = metadataResult{Healthy: true, Missing: 3, Drain: drainResult{Settled: true}}
	lost.Targets = evaluateTargets(lost)
	if got := findTarget(t, lost.Targets, "request-metadata-lost"); got.Status != statusMissed {
		t.Errorf("lost events were not a miss: %+v", got)
	}
	dropped := s1Result(1, 1)
	dropped.Metadata = metadataResult{Healthy: true, DroppedByGateway: 1, Drain: drainResult{Settled: true}}
	dropped.Targets = evaluateTargets(dropped)
	if got := findTarget(t, dropped.Targets, "request-metadata-lost"); got.Status != statusMissed {
		t.Errorf("an event the gateway admits to dropping was not a miss: %+v", got)
	}
	// What the gateway abandoned, or the usage report admits to missing, is lost
	// as much as what it dropped.
	abandoned := s1Result(1, 1)
	abandoned.Metadata = metadataResult{Healthy: true, AbandonedByGateway: 2, Drain: drainResult{Settled: true}}
	abandoned.Targets = evaluateTargets(abandoned)
	if got := findTarget(t, abandoned.Targets, "request-metadata-lost"); got.Status != statusMissed {
		t.Errorf("events the gateway abandoned were not a miss: %+v", got)
	}
	gap := s1Result(1, 1)
	gap.Metadata = metadataResult{Healthy: true, ReportedGapEvents: 5, Drain: drainResult{Settled: true}}
	gap.Targets = evaluateTargets(gap)
	if got := findTarget(t, gap.Targets, "request-metadata-lost"); got.Status != statusMissed {
		t.Errorf("a gap the usage report admits to was not a miss: %+v", got)
	}
	// The totals a gateway wrote for its epoch at shutdown are exact, where its
	// metrics may be older than the run's end.
	for name, epoch := range map[string]*epochCounters{"dropped": {Dropped: 1}, "abandoned": {Abandoned: 1}} {
		late := s1Result(1, 1)
		late.Metadata = metadataResult{Healthy: true, Epoch: epoch, Drain: drainResult{Settled: true}}
		late.Targets = evaluateTargets(late)
		if got := findTarget(t, late.Targets, "request-metadata-lost"); got.Status != statusMissed {
			t.Errorf("an event the epoch records as %s was not a miss: %+v", name, got)
		}
	}
	clean := s1Result(1, 1)
	clean.Metadata = metadataResult{Healthy: true, Epoch: &epochCounters{Accepted: 10, Persisted: 10, ClosedGracefully: true}, Drain: drainResult{Settled: true}}
	clean.Targets = evaluateTargets(clean)
	if got := findTarget(t, clean.Targets, "request-metadata-lost"); got.Status != statusMet {
		t.Errorf("a clean epoch was not a pass: %+v", got)
	}
	// Metrics older than the end of the run cannot say Valkey stayed healthy.
	stale := s1Result(1, 1)
	stale.Metadata = metadataResult{Healthy: true, MetricsStale: true, Drain: drainResult{Settled: true}}
	stale.Targets = evaluateTargets(stale)
	if got := findTarget(t, stale.Targets, "request-metadata-lost"); got.Status != statusNotChecked || got.MeetsTarget != nil {
		t.Errorf("a run whose metrics did not refresh was judged: %+v", got)
	}
	unsettled := s1Result(1, 1)
	unsettled.Metadata = metadataResult{Healthy: true, Drain: drainResult{Settled: false}}
	unsettled.Targets = evaluateTargets(unsettled)
	if got := findTarget(t, unsettled.Targets, "request-metadata-lost"); got.Status != statusMissed {
		t.Errorf("metadata still arriving was not a miss: %+v", got)
	}
	// The target is stated for a healthy Valkey.
	sick := s1Result(1, 1)
	sick.Metadata = metadataResult{Healthy: false, FailOpen: 2, Drain: drainResult{Settled: true}}
	sick.Targets = evaluateTargets(sick)
	if got := findTarget(t, sick.Targets, "request-metadata-lost"); got.Status != statusNotChecked {
		t.Errorf("a run with an unhealthy Valkey was judged: %+v", got)
	}
	// S4 to S6 carry no such target.
	for _, id := range []string{"S4", "S5", "S6"} {
		r := s1Result(1, 1)
		r.Scenario = id
		for _, target := range evaluateTargets(r) {
			if target.ID == "request-metadata-lost" {
				t.Errorf("%s is judged on metadata loss", id)
			}
		}
	}
}

func TestHarnessErrorRateOfHeldStreams(t *testing.T) {
	run := report(1000, true, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{})
	run.Requests.Succeeded, run.Requests.Failed = 0, 1000
	run.Errors.ByKind = map[string]int64{"canceled": 990, "truncated": 10}
	r := &result{Baseline: report(1000, true, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{}, loadgen.Summary{}), Run: run}
	r.Plan.SlowReadBPS = 8192
	computeErrors(r)
	if r.ErrorRate.Gateway != 0.01 || r.ErrorRate.CanceledAtEnd != 990 || r.ErrorRate.Note == "" {
		t.Errorf("held streams canceled at the end are not errors, a truncated one is: %+v", r.ErrorRate)
	}
	r.Plan.SlowReadBPS = 0
	run.Rates.ErrorRate = 1
	computeErrors(r)
	if r.ErrorRate.Gateway != 1 || r.ErrorRate.CanceledAtEnd != 0 {
		t.Errorf("a canceled request is an error outside S6: %+v", r.ErrorRate)
	}
}
