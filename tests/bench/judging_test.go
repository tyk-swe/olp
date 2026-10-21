//go:build bench

package bench_test

import (
	"strings"
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

// A snapshot that reports an age of 0 may be a second old, so within the
// second after a run ended it is not known to be newer than the run.

// A snapshot that is never newer comes back as such, with what was read, so
// that the target is left unchecked rather than judged on it.

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
