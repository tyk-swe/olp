//go:build bench

package bench_test

import (
	"fmt"
	"strings"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
)

// The roadmap's targets (docs/roadmap/m01-measured-advantage.md#targets) are
// judged against every run and reported in its result, but they fail the test
// only under OLP_BENCH_ENFORCE=1: a smoke run is not made on reference
// hardware and cannot meet or miss a target in any way that means something.
//
// A target that compares OLP with LiteLLM cannot be judged from one run of one
// gateway. It is reported with the figures it will be judged on and the
// status needs_comparison, which scripts/bench-compare.sh settles.

const (
	// The S1 target applies to a gateway pinned to exactly two vCPUs, with the
	// mock upstream and the load generator on CPUs of their own.
	s1VCPUs         = 2
	s1P95LimitMs    = 2.0
	s1P99LimitMs    = 5.0
	s3SuccessTarget = 1.0
)

func ptr[T any](v T) *T { return &v }

// evaluateTargets judges the run against the targets of its scenario.
func evaluateTargets(r *result) []targetResult {
	var targets []targetResult
	switch r.Scenario {
	case "S1", "S2", "S3", "S4", "S5":
		targets = append(targets, comparative(r)...)
	}
	switch r.Scenario {
	case "S1":
		targets = append(targets, s1Latency(r, "p95", s1P95LimitMs, func(p *percentiles) float64 { return p.P95 }, func(s loadgen.Summary) float64 { return s.P95Ms }),
			s1Latency(r, "p99", s1P99LimitMs, func(p *percentiles) float64 { return p.P99 }, func(s loadgen.Summary) float64 { return s.P99Ms }))
	case "S3":
		targets = append(targets, s3Success(r), s3Resources(r))
	}
	switch r.Scenario {
	case "S1", "S2", "S3":
		targets = append(targets, metadataLoss(r))
	}
	return targets
}

// comparative are the targets stated against LiteLLM on identical hardware.
func comparative(r *result) []targetResult {
	const reason = "needs the same scenario run against the pinned LiteLLM release; scripts/bench-compare.sh settles it"
	latency := targetResult{ID: "added-latency-below-litellm", Description: "OLP's added latency is lower than LiteLLM's at p50, p95 and p99",
		Target: "OLP < LiteLLM at p50, p95 and p99", Unit: "ms", Status: statusNeedsComparison, Reason: reason}
	if p := primaryAdded(r); p != nil {
		latency.Measured = ptr(p.P95)
		latency.Reason = fmt.Sprintf("OLP added p50 %.2f, p95 %.2f, p99 %.2f ms; %s", p.P50, p.P95, p.P99, reason)
	}
	rps := targetResult{ID: "rps-per-vcpu-above-litellm", Description: "OLP sustains more RPS per vCPU than LiteLLM",
		Target: "OLP > LiteLLM", Unit: "rps/vCPU", Status: statusNeedsComparison, Reason: reason}
	if r.Throughput.VCPUs > 0 {
		rps.Measured = ptr(r.Throughput.RPSPerVCPU)
	}
	return []targetResult{latency, rps}
}

// primaryAdded is the added latency that stands for the scenario: the mode it
// exercises, or all successful requests when it mixes them.
func primaryAdded(r *result) *percentiles {
	switch {
	case r.Plan.StreamShare == 0:
		return r.AddedLatency.Unary
	case r.Plan.StreamShare == 1:
		return r.AddedLatency.Stream
	}
	return r.AddedLatency.All
}

// primaryLatency is the latencies of the population that stands for the
// scenario, as primaryAdded picks the added latency of it.
func primaryLatency(l loadgen.Latencies, streamShare float64) loadgen.Summary {
	switch streamShare {
	case 0:
		return l.Unary
	case 1:
		return l.Stream
	}
	return l.All
}

// judge finishes a target whose conditions hold.
func judge(t targetResult, ok bool) targetResult {
	t.MeetsTarget = ptr(ok)
	t.Status = statusMissed
	if ok {
		t.Status = statusMet
	}
	return t
}

// notChecked finishes a target that could not be judged, and says why.
func notChecked(t targetResult, format string, args ...any) targetResult {
	t.Status = statusNotChecked
	t.Reason = fmt.Sprintf(format, args...)
	return t
}

// referenceShortfall lists what keeps a run from being judged against a target
// stated for reference conditions, or "" when nothing does.
func referenceShortfall(r *result) string {
	var short []string
	if !r.Reference.FullScale {
		short = append(short, fmt.Sprintf("the run was at scale %g, not 1", r.Scale))
	}
	if !r.Reference.ValidRuns {
		short = append(short, "a load run was invalid")
	}
	return strings.Join(short, "; ")
}

// pinShortfalls lists the processes of a run that were not given CPUs of their
// own: one that was not pinned, and CPUs two of them share. A gateway that
// competes for its two CPUs with the mock upstream or the load generator is not
// the two-vCPU gateway a target is stated for, whatever its affinity says.
func pinShortfalls(r *result) []string {
	var short []string
	for _, pin := range []struct {
		name   string
		pinned bool
	}{{envGatewayCPUs, r.Reference.GatewayPinned}, {envMockCPUs, r.Reference.MockPinned}, {envLoadgenCPUs, r.Reference.LoadgenPinned}} {
		if !pin.pinned {
			short = append(short, fmt.Sprintf("%s is not set: a reference run gives each process its own CPUs", pin.name))
		}
	}
	if !r.Reference.PinsDisjoint {
		short = append(short, "two of the gateway, the mock upstream and the load generator were given a CPU in common: a reference run gives each process its own CPUs")
	}
	return short
}

// s1Shortfall is what keeps a run from being judged against the S1 target: the
// conditions of a reference run, which for a target stated for a gateway on two
// CPUs include the CPUs themselves.
func s1Shortfall(r *result) string {
	short := pinShortfalls(r)
	if s := referenceShortfall(r); s != "" {
		short = append(short, s)
	}
	return strings.Join(short, "; ")
}

// s1Latency judges the added latency at one percentile; baseline reads the
// same percentile of the baseline run.
func s1Latency(r *result, name string, limit float64, pick func(*percentiles) float64, baseline func(loadgen.Summary) float64) targetResult {
	t := targetResult{ID: "s1-2vcpu-added-" + name, Description: fmt.Sprintf("S1 on a 2-vCPU gateway: added latency at most %g ms at %s", limit, name),
		Target: fmt.Sprintf("<= %g ms", limit), Unit: "ms"}
	switch {
	case r.Gateway.VCPUs != s1VCPUs:
		return notChecked(t, "the gateway may use %d CPUs and the target is stated for %d; pin it with %s", r.Gateway.VCPUs, s1VCPUs, envGatewayCPUs)
	case s1Shortfall(r) != "":
		return notChecked(t, "%s", s1Shortfall(r))
	case len(r.Validity.Problems) > 0:
		// The latency is that of the requests that succeeded, and what made the
		// run doubtful may have cost the ones that did not.
		return notChecked(t, "the run was not valid: %s", strings.Join(r.Validity.Problems, "; "))
	case r.AddedLatency.Unary == nil:
		return notChecked(t, "the run recorded no successful unary request")
	}
	t.Measured = ptr(pick(r.AddedLatency.Unary))
	if base := baseline(r.Baseline.Latency.Unary); *t.Measured < -negativeAllowance(base) {
		// A gateway cannot answer faster than the upstream it calls, so this is
		// the baseline's noise, and a limit is not met by it.
		return notChecked(t, "the added latency is %.2f ms, below zero by more than a baseline's jitter: the baseline was slower than the run it is subtracted from", *t.Measured)
	}
	// The target is "at most", so a gateway that adds exactly the limit meets it.
	return judge(t, *t.Measured <= limit)
}

func s3Success(r *result) targetResult {
	t := targetResult{ID: "s3-success-rate", Description: "S3: 3,000 RPS at a 100% success rate", Target: "100% of scheduled requests succeed at 3,000 RPS", Unit: "ratio"}
	if short := referenceShortfall(r); short != "" {
		return notChecked(t, "%s", short)
	}
	// A run that charged its key nothing was not S3, however many requests
	// succeeded: the reservation and settlement the target includes did not run.
	if r.Plan.Budget {
		if short := budgetShortfall(r); short != "" {
			return notChecked(t, "%s", short)
		}
	}
	t.Measured = ptr(r.Run.Rates.SuccessRate)
	// The rate itself is part of the target: a generator that fell behind
	// would otherwise pass by sending less.
	return judge(t, r.Run.Rates.SuccessRate >= s3SuccessTarget && r.Run.Rates.TargetRPS >= 3000 && r.Run.Rates.Ratio >= 0.99)
}

// s3Resources is the second half of the S3 target, which is a comparison.
func s3Resources(r *result) targetResult {
	t := targetResult{ID: "s3-fewer-vcpu-than-litellm", Description: "S3: fewer total vCPU than LiteLLM's high-throughput profile at equal or better p95",
		Target: "OLP vCPU < LiteLLM profile's vCPU, with p95 no worse", Unit: "vCPU", Status: statusNeedsComparison,
		Reason: "needs the LiteLLM high-throughput profile run on the same workload; scripts/bench-compare.sh settles it"}
	t.Measured = ptr(float64(r.Gateway.VCPUs))
	return t
}

func metadataLoss(r *result) targetResult {
	t := targetResult{ID: "request-metadata-lost", Description: fmt.Sprintf("%s with healthy Valkey: zero lost request-metadata events", r.Scenario),
		Target: "0 events lost", Unit: "events"}
	m := r.Metadata
	switch {
	case m.MetricsStale:
		return notChecked(t, "the gateway's metrics did not refresh after the run, so whether Valkey stayed healthy to its end is not known, and the target is stated for a healthy one")
	case !m.Healthy:
		return notChecked(t, "Valkey was not healthy throughout the run (limiter fail-open %.0f), and the target is stated for a healthy one", m.FailOpen)
	case !m.Drain.Settled:
		t.Measured = ptr(float64(m.Missing))
		return judge(t, false)
	}
	t.Measured = ptr(float64(m.Missing))
	return judge(t, m.Missing == 0 && m.droppedEvents() == 0 && m.abandonedEvents() == 0 && m.ReportedGapEvents == 0)
}

// droppedEvents and abandonedEvents are what the gateway admits to having lost:
// its own counters as its metrics reported them, and the totals it wrote for
// its epoch when it shut down, which are exact where the metrics come from a
// snapshot that may be older than the run's end. Whichever is larger stands.
func (m metadataResult) droppedEvents() float64 {
	if m.Epoch != nil {
		return max(m.DroppedByGateway, float64(m.Epoch.Dropped))
	}
	return m.DroppedByGateway
}

func (m metadataResult) abandonedEvents() float64 {
	if m.Epoch != nil {
		return max(m.AbandonedByGateway, float64(m.Epoch.Abandoned))
	}
	return m.AbandonedByGateway
}

// enforcementFailures is what OLP_BENCH_ENFORCE=1 turns into test failures:
// a run that is not a reference run cannot vouch for any target, a target that
// was missed or could not be checked fails, and one that is a comparison is
// left to scripts/bench-compare.sh.
func enforcementFailures(r *result) []string {
	var failures []string
	if !r.Reference.FullScale {
		failures = append(failures, fmt.Sprintf("the run was at scale %g, and the targets are stated at full rates", r.Scale))
	}
	failures = append(failures, pinShortfalls(r)...)
	failures = append(failures, r.Validity.Problems...)
	for _, t := range r.Targets {
		switch t.Status {
		case statusMissed:
			failures = append(failures, fmt.Sprintf("target %s missed: measured %s, want %s", t.ID, measured(t), t.Target))
		case statusNotChecked:
			failures = append(failures, fmt.Sprintf("target %s was not checked: %s", t.ID, t.Reason))
		}
	}
	return failures
}

func measured(t targetResult) string {
	if t.Measured == nil {
		return "nothing"
	}
	return fmt.Sprintf("%.4g %s", *t.Measured, t.Unit)
}
