//go:build bench

package bench_test

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
)

// result is what one scenario run writes to .local/bench/<scenario>.json: the
// two load generator reports it is built from and everything measured around
// them. Nothing in it is estimated; a figure with no honest source is null
// beside a reason.
type result struct {
	Scenario    string    `json:"scenario"`
	Title       string    `json:"title"`
	Why         string    `json:"why"`
	GeneratedAt time.Time `json:"generated_at"`
	Scale       float64   `json:"scale"`
	// Reference says whether the run was made under the conditions the
	// roadmap's targets are stated for. A smoke run is not, and its numbers
	// are not reference numbers.
	Reference   referenceConditions `json:"reference_conditions"`
	Environment environment         `json:"environment"`
	Plan        plan                `json:"workload"`
	Gateway     gatewayInfo         `json:"gateway"`

	Baseline *loadgen.Report `json:"baseline"`
	Run      *loadgen.Report `json:"gateway_run"`

	// AddedLatency is the gateway run's latency less the direct-to-mock
	// baseline's, in milliseconds, at each percentile.
	AddedLatency addedLatency `json:"added_latency_ms"`
	// TTFT is the added time to first token of streams.
	TTFT        *percentiles      `json:"ttft_overhead_ms"`
	TTFTReason  string            `json:"ttft_overhead_reason,omitempty"`
	Throughput  throughput        `json:"throughput"`
	Resources   resourceUse       `json:"resources"`
	Allocations allocationsResult `json:"allocations_per_request"`
	ErrorRate   errorRates        `json:"error_rate"`
	// ErrorSample is what the gateway answered to one request of the largest
	// prompt, sent after a run that had failures.
	ErrorSample string         `json:"error_sample,omitempty"`
	Metadata    metadataResult `json:"request_metadata_completeness"`
	Attempts    attemptStats   `json:"attempts"`
	// Preflight lists what may bend the run, found before it started.
	Preflight []string        `json:"preflight_warnings,omitempty"`
	Failover  *failoverResult `json:"failover,omitempty"`
	Streams   *streamsResult  `json:"slow_readers,omitempty"`
	// Budget is what the key's cost budget accrued, for a scenario that gives
	// its key one.
	Budget    *budgetResult `json:"budget,omitempty"`
	MockStats mockStats     `json:"mock_upstream"`

	Validity validity       `json:"validity"`
	Targets  []targetResult `json:"targets"`
}

type referenceConditions struct {
	FullScale     bool `json:"full_scale"`
	GatewayPinned bool `json:"gateway_pinned"`
	MockPinned    bool `json:"mock_pinned"`
	LoadgenPinned bool `json:"loadgen_pinned"`
	// PinsDisjoint is false when two of the processes were given a CPU in
	// common; a process left unpinned shares no CPU it was told of.
	PinsDisjoint bool `json:"pins_disjoint"`
	ValidRuns    bool `json:"valid_runs"`
	// All is true only when every condition holds.
	All bool `json:"all"`
}

type environment struct {
	GoVersion   string  `json:"go_version"`
	OS          string  `json:"os"`
	Arch        string  `json:"arch"`
	Kernel      string  `json:"kernel"`
	CPUModel    string  `json:"cpu_model"`
	HostCPUs    int     `json:"host_cpus"`
	MemoryGiB   float64 `json:"memory_gib"`
	LoadAverage float64 `json:"load_average_at_start"`
	OLPVersion  string  `json:"olp_version"`
	OLPSHA256   string  `json:"olp_binary_sha256"`
	MockCPUs    string  `json:"mock_cpus,omitempty"`
	GatewayCPUs string  `json:"gateway_cpus,omitempty"`
	LoadgenCPUs string  `json:"loadgen_cpus,omitempty"`
}

type gatewayInfo struct {
	Mode string `json:"mode"`
	// VCPUs is how many CPUs the process may use, from its CPU affinity in
	// /proc, and Source says whether taskset set it or it is the machine's.
	VCPUs       int               `json:"vcpus"`
	VCPUSource  string            `json:"vcpu_source"`
	CPUsAllowed string            `json:"cpus_allowed"`
	Settings    map[string]string `json:"settings"`
	Limits      gatewayLimits     `json:"derived_limits"`
	// LimitsEnforced is the installation's own report that budgets and rate
	// limits are enforced, which needs Valkey.
	LimitsEnforced bool `json:"limits_enforced"`
}

type percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

// addedLatency is the gateway's overhead for all successful requests and for
// each mode, null for a mode the scenario does not exercise.
type addedLatency struct {
	All    *percentiles `json:"all"`
	Unary  *percentiles `json:"unary"`
	Stream *percentiles `json:"stream"`
}

type throughput struct {
	TargetRPS  float64 `json:"target_rps"`
	OfferedRPS float64 `json:"offered_rps"`
	// SustainedRPS is the successful requests that finished per second of the
	// measured period; RPSPerVCPU divides it by the gateway's vCPUs.
	SustainedRPS float64 `json:"sustained_rps"`
	VCPUs        int     `json:"vcpus"`
	RPSPerVCPU   float64 `json:"rps_per_vcpu"`
	// CPUMsPerRequest is the CPU time the gateway spent per request it
	// answered successfully, warmup included, less what it spends idle; the
	// gross figure keeps the idle share. RequestsPerCPUSecond is its inverse:
	// what one fully used core could carry, whatever number of CPUs the process
	// was allowed.
	CPUMsPerRequest      float64 `json:"gateway_cpu_ms_per_request"`
	CPUMsPerRequestGross float64 `json:"gateway_cpu_ms_per_request_gross"`
	RequestsPerCPUSecond float64 `json:"requests_per_cpu_second"`
}

// allocationsResult is what the whole gateway process allocated on the heap per
// request it answered. Without an honest source the figures are null beside a
// reason.
type allocationsResult struct {
	// Value is heap objects per request and BytesPerRequest bytes per request,
	// net of what the gateway allocates idle.
	Value           *float64 `json:"value"`
	BytesPerRequest *float64 `json:"bytes_per_request"`
	// IdleObjectsPerSecond is the rate that was taken off.
	IdleObjectsPerSecond float64 `json:"idle_objects_per_second,omitempty"`
	Reason               string  `json:"reason,omitempty"`
}

type errorRates struct {
	Baseline float64 `json:"baseline"`
	Gateway  float64 `json:"gateway"`
	// SuccessRate is successes over scheduled requests, so a dropped request
	// counts against it.
	GatewaySuccessRate float64 `json:"gateway_success_rate"`
	// CanceledAtEnd is the requests of a held-open scenario that the generator
	// ended itself, which are not counted as errors.
	CanceledAtEnd int64            `json:"gateway_canceled_at_end,omitempty"`
	Note          string           `json:"note,omitempty"`
	ByKind        map[string]int64 `json:"gateway_by_kind"`
	StatusCodes   map[string]int64 `json:"gateway_status_codes"`
}

type attemptStats struct {
	// PerRequest is recorded attempts over the requests that made one, 1 when
	// nothing was retried.
	PerRequest float64      `json:"per_request"`
	Rows       []attemptRow `json:"rows"`
	// ByProvider shows how a load spread over several providers divided.
	ByProvider []providerShare `json:"by_provider,omitempty"`
}

// failoverResult is S4's account of what the gateway did with the failing
// first target. The gateway's circuit is kept per provider and opens after
// five consecutive failures, so once it is open the first target is skipped
// except for one probe every thirty seconds; the steady state then measures
// the skip, and only the requests before it opened and the probes pay for a
// failed attempt.
type failoverResult struct {
	FirstTarget, SecondTarget string `json:"-"`
	FirstTargetAttempts       int64  `json:"first_target_attempts"`
	SecondTargetAttempts      int64  `json:"second_target_attempts"`
	// RequestsFailedOver is the requests that needed a second attempt.
	RequestsFailedOver int64 `json:"requests_failed_over"`
	// FailedOverShare is that count over the requests that made an attempt.
	FailedOverShare float64 `json:"failed_over_share"`
	Note            string  `json:"note"`
}

// budgetResult is what the bench key's cost budget accrued over the run, as
// the management API reports the key after the request metadata has arrived.
// A budget that accrued nothing did none of the work S3 is for: the price did
// not apply, or the requests were not charged to the key.
type budgetResult struct {
	// DailyAccrued and MonthlyAccrued are the cost of the key's current UTC
	// day and month, as the exact decimals the API serves.
	DailyAccrued   string `json:"daily_accrued"`
	MonthlyAccrued string `json:"monthly_accrued"`
	// UnpricedAttempts counts the attempts the key made that no price covered.
	UnpricedAttempts int64 `json:"unpriced_attempts"`
}

// accrued says whether the budget was charged anything at all.
func (b *budgetResult) accrued() bool {
	if b == nil {
		return false
	}
	for _, amount := range []string{b.DailyAccrued, b.MonthlyAccrued} {
		if v, ok := new(big.Rat).SetString(amount); ok && v.Sign() > 0 {
			return true
		}
	}
	return false
}

// streamsResult is S6's account of holding the streams open. No stream is
// meant to finish: the readers are slower than the run is long, and the
// generator cancels what is still open when it ends, so the figures that
// matter are how many were held at once and what that cost.
type streamsResult struct {
	TargetConcurrency int `json:"target_concurrent_streams"`
	// PeakInFlight is the most requests the generator had open at once, and
	// PeakAdmitted the most the gateway held, from its own gauge. A gateway
	// that holds fewer than the generator opened has absorbed streams into
	// socket buffers instead of serving them.
	PeakInFlight int     `json:"peak_in_flight"`
	PeakAdmitted float64 `json:"peak_gateway_admitted"`
	// Reached and GatewayReached are those peaks over the target.
	Reached         float64 `json:"target_reached"`
	GatewayReached  float64 `json:"gateway_target_reached"`
	OutputTokens    int     `json:"output_tokens"`
	ReadBytesPerSec int     `json:"read_bytes_per_second"`
	// ResponseBytes is the size of one stream, measured against the mock.
	ResponseBytes int64 `json:"response_bytes"`
	// RSSPerStreamKiB is the gateway's growth in resident memory, from before
	// the run to its peak, over the streams it held at its peak. Resident
	// memory follows the garbage collector as much as the streams, so it is
	// null, with the reason, when it did not grow, and otherwise indicative.
	RSSPerStreamKiB    *float64 `json:"gateway_rss_growth_per_stream_kib"`
	RSSPerStreamReason string   `json:"gateway_rss_growth_reason,omitempty"`
	// HeldOpenAtEnd is the streams the generator canceled when its drain
	// period ended, which is how a run of held streams ends and is not an
	// error. Truncated, TimedOut and Failed are streams that ended some other
	// way, which are findings: a write deadline that cut a stalled reader
	// appears as truncated.
	HeldOpenAtEnd int64 `json:"held_open_at_end"`
	Truncated     int64 `json:"truncated"`
	TimedOut      int64 `json:"timed_out"`
	Failed        int64 `json:"failed"`
}

type mockStats struct {
	// BaselineRequests and RunRequests are what the mock saw during each run.
	BaselineRequests  int64 `json:"baseline_requests"`
	RunRequests       int64 `json:"run_requests"`
	RunInjectedErrors int64 `json:"run_injected_errors"`
	RunMaxInFlight    int64 `json:"run_max_in_flight"`
	// RunDialects counts the run's requests by the endpoint the gateway called,
	// which shows what a translated route asked its upstream for.
	RunDialects map[string]int64 `json:"run_dialects"`
}

type validity struct {
	Valid bool `json:"valid"`
	// Problems make a run unfit for judging a target; Warnings do not.
	Problems []string `json:"problems,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Target statuses. A target is checked only under the conditions it is stated
// for, and one that was not checked says why.
const (
	statusMet             = "met"
	statusMissed          = "missed"
	statusNeedsComparison = "needs_comparison"
	statusNotChecked      = "not_checked"
)

// targetResult is one roadmap target judged against a run. MeetsTarget is
// null unless the target could be checked.
type targetResult struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Target      string   `json:"target"`
	Measured    *float64 `json:"measured"`
	Unit        string   `json:"unit,omitempty"`
	Status      string   `json:"status"`
	MeetsTarget *bool    `json:"meets_target"`
	Reason      string   `json:"reason,omitempty"`
}

func deltaOf(base, run loadgen.Summary) *percentiles {
	if base.Count == 0 || run.Count == 0 {
		return nil
	}
	return &percentiles{P50: run.P50Ms - base.P50Ms, P95: run.P95Ms - base.P95Ms, P99: run.P99Ms - base.P99Ms}
}

// computeAdded fills what the gateway added, which Compare does not cover for
// the mixed population of a scenario that both streams and does not.
func computeAdded(r *result) {
	b, g := r.Baseline.Latency, r.Run.Latency
	r.AddedLatency = addedLatency{All: deltaOf(b.All, g.All), Unary: deltaOf(b.Unary, g.Unary), Stream: deltaOf(b.Stream, g.Stream)}
	if overhead := loadgen.Compare(r.Baseline, r.Run); overhead.TTFT.Present {
		r.TTFT = &percentiles{P50: overhead.TTFT.P50Ms, P95: overhead.TTFT.P95Ms, P99: overhead.TTFT.P99Ms}
	} else {
		r.TTFTReason = "the scenario has no streaming requests, so there is no first token to time"
	}
}

func computeThroughput(r *result, use resourceUse) {
	run := r.Run
	t := throughput{TargetRPS: run.Rates.TargetRPS, OfferedRPS: run.Rates.OfferedRPS, SustainedRPS: run.Rates.ThroughputRPS, VCPUs: r.Gateway.VCPUs}
	if t.VCPUs > 0 {
		t.RPSPerVCPU = t.SustainedRPS / float64(t.VCPUs)
	}
	if handled := handledRequests(run, r.Plan.SlowReadBPS > 0); handled > 0 && use.Gateway.Seconds > 0 {
		t.CPUMsPerRequestGross = use.Gateway.Seconds * 1000 / float64(handled)
		if net := use.Gateway.Seconds - use.GatewayIdleCores*use.WallSeconds; net > 0 {
			t.CPUMsPerRequest = net * 1000 / float64(handled)
			t.RequestsPerCPUSecond = float64(handled) / net
		}
	}
	r.Throughput = t
}

// handledRequests is the requests the gateway answered over the whole run,
// warmup included, which is the span its CPU time covers. Only the ones it
// answered successfully count: a request it failed or shed cost it little and
// says nothing of what a CPU carries, so a gateway that fails fast would look
// the more efficient for it. A run that holds its streams open ends them
// itself, so every stream the gateway took is counted.
func handledRequests(run *loadgen.Report, held bool) int64 {
	if held {
		return run.Requests.Sent + run.Requests.WarmupSent
	}
	return run.Requests.Succeeded + run.Requests.WarmupSent - run.Requests.WarmupFailed
}

func computeErrors(r *result) {
	e := errorRates{Baseline: r.Baseline.Rates.ErrorRate, Gateway: r.Run.Rates.ErrorRate, GatewaySuccessRate: r.Run.Rates.SuccessRate,
		ByKind: r.Run.Errors.ByKind, StatusCodes: r.Run.Errors.StatusCodes}
	if r.Plan.SlowReadBPS > 0 {
		// Streams that never finish are canceled when the run ends, which is
		// how it ends rather than a failure.
		e.CanceledAtEnd = r.Run.Errors.ByKind["canceled"]
		if sent := r.Run.Requests.Sent; sent > 0 {
			e.Gateway = float64(r.Run.Requests.Failed-e.CanceledAtEnd) / float64(sent)
			e.Baseline = float64(r.Baseline.Requests.Failed-r.Baseline.Errors.ByKind["canceled"]) / float64(max(r.Baseline.Requests.Sent, 1))
		}
		e.Note = "the streams are held open past the end of the run and canceled by the generator, so the success rate does not apply and the error rate leaves the canceled streams out"
	}
	r.ErrorRate = e
}

// maxFailureRate is the share of a run's requests that may fail before its
// latencies stop being fit for judging. They are the latencies of the requests
// that succeeded, so a gateway that fails requests fast would look quicker than
// one that answers them: a few in a thousand do not move a percentile that is
// reported, and more make the figures the survivors' alone.
const maxFailureRate = 0.001

// negativeAllowance is how far below zero an added latency may fall and still
// be taken for jitter: a millisecond or a tenth of the baseline's figure,
// whichever is larger, as bench-compare allows two baselines to differ.
func negativeAllowance(baseline float64) float64 { return max(1, 0.1*baseline) }

// budgetShortfall says why a scenario with a cost budget did no budget work, or
// "" when it did.
func budgetShortfall(r *result) string {
	switch {
	case r.Budget == nil:
		return "the key's budget was not read after the run, so whether it accrued any cost is not known"
	case !r.Budget.accrued():
		return "the key's budget accrued no cost, so the run did no budget work and is not comparable with a run that charges every request: the model's price did not apply, or the requests were not charged to the key"
	}
	return ""
}

// judgeValidity decides whether the run can be trusted and records what makes
// it doubtful.
func judgeValidity(r *result) {
	v := validity{Valid: true}
	problem := func(format string, args ...any) {
		v.Valid = false
		v.Problems = append(v.Problems, fmt.Sprintf(format, args...))
	}
	warn := func(format string, args ...any) { v.Warnings = append(v.Warnings, fmt.Sprintf(format, args...)) }
	for _, run := range []struct {
		name   string
		report *loadgen.Report
	}{{"baseline", r.Baseline}, {"gateway run", r.Run}} {
		if !run.report.Valid {
			problem("the %s load did not deliver its schedule: %s", run.name, strings.Join(run.report.Problems, "; "))
		}
	}
	if st := r.Streams; st != nil {
		if st.Reached < 0.95 {
			problem("the generator held %d of the %d streams open at once", st.PeakInFlight, st.TargetConcurrency)
		}
		if st.GatewayReached < 0.9 {
			// Two causes leave the gateway holding fewer streams than were open:
			// a response that fits in the socket buffers is written whole, and
			// the gateway ends a stream whose writes have been blocked for thirty
			// seconds, so the first streams are gone before the last opens when
			// the warmup and the measured period, over which S6 opens them, last
			// longer than that.
			problem("the gateway held %.0f of the %d streams at once, because their responses fit in socket buffers instead of meeting the slow readers (raise %s), "+
				"or because it ended the first streams, thirty seconds after their writes blocked, before the last opened (open them over less than that: shorten %s and %s)",
				st.PeakAdmitted, st.TargetConcurrency, envS6Tokens, envWarmup, envDuration)
		}
	}
	if f := r.Failover; r.Plan.Failover {
		// S4 measures what it costs to fail over, which it does only if the
		// first target was tried and failed. A rule that was not armed, or a
		// route that stopped trying the target first, leaves a plain unary run
		// that is judged and compared as a failover.
		switch {
		case f == nil || f.FirstTargetAttempts == 0:
			problem("no request tried the failing first target, so the run measured a healthy route, not a failover")
		case r.MockStats.RunInjectedErrors == 0:
			problem("the mock upstream injected no error, so the first target never failed and the run measured no failover")
		case f.RequestsFailedOver == 0:
			problem("no request failed over after the first target failed, so the run measured no failover")
		}
	}
	if r.Plan.Budget {
		// S3's budget is real on both sides: a key that was charged nothing did
		// none of the reservation and settlement the scenario is for.
		if reason := budgetShortfall(r); reason != "" {
			problem("%s", reason)
		}
		if b := r.Budget; b != nil && b.UnpricedAttempts > 0 {
			warn("%d of the key's attempts had no price, so the budget was not charged for them", b.UnpricedAttempts)
		}
	}
	if r.Plan.SlowReadBPS == 0 {
		// S6's streams end by being canceled, and the ones that end any other
		// way are findings of that scenario, not a reason to doubt it.
		for _, run := range []struct {
			name string
			rate float64
		}{{"baseline", r.ErrorRate.Baseline}, {"gateway run", r.ErrorRate.Gateway}} {
			if run.rate > maxFailureRate {
				problem("%.2f%% of the %s's requests failed, above the %.1f%% a run may lose: its latencies are the survivors' alone and say nothing of the gateway", 100*run.rate, run.name, 100*maxFailureRate)
			}
		}
	}
	if added, base := primaryAdded(r), primaryLatency(r.Baseline.Latency, r.Plan.StreamShare); added != nil {
		// A gateway cannot answer faster than the upstream it calls, so a
		// negative difference is a baseline that was slower than the run, not
		// a speed-up, and a target judged on it would be judged on noise.
		for _, q := range []struct {
			name      string
			add, base float64
			gates     bool
		}{{"p50", added.P50, base.P50Ms, true}, {"p95", added.P95, base.P95Ms, true}, {"p99", added.P99, base.P99Ms, false}} {
			if q.add >= -negativeAllowance(q.base) {
				continue
			}
			if q.gates {
				problem("the gateway's added latency is %.2f ms at %s, below zero by more than a baseline's jitter: the baseline was slower than the run it is subtracted from, and no target can be judged on that", q.add, q.name)
			} else {
				warn("the gateway's added latency is %.2f ms at %s, below zero by more than a baseline's jitter, as a tail varies between sessions", q.add, q.name)
			}
		}
	}
	if r.Resources.Rejections > 0 {
		problem("the gateway's admission pool shed %.0f requests, so its configured limits were too low for the load", r.Resources.Rejections)
	}
	if !r.Metadata.Drain.Settled {
		problem("request metadata did not finish arriving: %s", r.Metadata.Drain.Reason)
	}
	// A busy mock or generator bends the numbers it is not meant to touch.
	for _, u := range []struct {
		name string
		use  cpuUse
	}{{"the mock upstream", r.Resources.Mock}, {"the load generator", r.Resources.Loadgen}} {
		if u.use.Allowed > 0 && u.use.Cores > 0.8*float64(u.use.Allowed) {
			warn("%s kept %.2f of its %d CPUs busy, so it may have limited the run", u.name, u.use.Cores, u.use.Allowed)
		}
	}
	if r.Resources.Gateway.Allowed > 0 && r.Resources.Gateway.Cores > 0.95*float64(r.Resources.Gateway.Allowed) {
		warn("the gateway kept %.2f of its %d CPUs busy: it was at its limit", r.Resources.Gateway.Cores, r.Resources.Gateway.Allowed)
	}
	if !r.Resources.PeakResetOK {
		warn("the kernel refused to reset the gateway's peak resident memory, so rss_peak_mib is the largest of the samples taken every %v and may miss a briefer peak", procInterval)
	}
	for _, w := range r.Preflight {
		warn("%s", w)
	}
	if r.Metadata.MetricsStale {
		warn("the gateway's metrics did not refresh after the run, so Valkey's health and the gateway's own loss counters may predate its last seconds")
	}
	if !r.Reference.PinsDisjoint {
		warn("two of the gateway, the mock upstream and the load generator were given a CPU in common, so they competed for it")
	}
	if !r.Reference.All {
		warn("this is not a reference run: full scale, pinned processes and valid runs are all needed")
	}
	r.Validity = v
}

// judgeReference says whether the run was made under the conditions the
// targets are stated for: full scale, each process pinned to CPUs of its own,
// and load runs that delivered their schedule.
func judgeReference(r *result, s settings) {
	c := referenceConditions{
		FullScale: r.Scale == 1, GatewayPinned: s.GatewayCPUs != "", MockPinned: s.MockCPUs != "", LoadgenPinned: s.LoadgenCPUs != "",
		PinsDisjoint: len(sharedCPUs(s)) == 0,
	}
	c.ValidRuns = r.Baseline.Valid && r.Run.Valid
	c.All = c.FullScale && c.GatewayPinned && c.MockPinned && c.LoadgenPinned && c.PinsDisjoint && c.ValidRuns
	r.Reference = c
}

// writeResult stores the scenario's result and its two raw reports under dir.
func writeResult(dir string, r *result) (string, error) {
	raw := filepath.Join(dir, "raw")
	if err := os.MkdirAll(raw, 0o755); err != nil {
		return "", err
	}
	id := strings.ToLower(r.Scenario)
	if err := r.Baseline.WriteJSON(filepath.Join(raw, id+"-baseline.json")); err != nil {
		return "", err
	}
	if err := r.Run.WriteJSON(filepath.Join(raw, id+"-gateway.json")); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".json")
	return path, os.WriteFile(path, append(data, '\n'), 0o644)
}

// allocationsPerRequest is what the gateway allocated per request it answered,
// from the Go runtime's counters on its metrics listener: what it allocated
// between the marks that bound the run, less what it allocates with no traffic,
// which the marks around the pause between the runs give, over the requests it
// answered, warmup included, as its CPU time is. A request it failed or shed
// costs it little and says nothing of what a request costs, so a gateway that
// fails fast would look the more frugal for it.
func allocationsPerRequest(start, end, idleFrom, idleTo cpuMark, handled int64) allocationsResult {
	none := func(reason string) allocationsResult { return allocationsResult{Reason: reason} }
	for _, mark := range []cpuMark{start, end, idleFrom, idleTo} {
		if !mark.allocsKnown {
			return none("the allocation counters (" + seriesAllocObjects + " and " + seriesAllocBytes + ") could not be read from the gateway's metrics at every point they are needed: a scrape failed, or its binary does not serve them")
		}
	}
	wall, idleWall := end.at.Sub(start.at).Seconds(), idleTo.at.Sub(idleFrom.at).Seconds()
	switch {
	case handled <= 0:
		return none("the gateway answered no request successfully")
	case wall <= 0 || idleWall <= 0:
		return none("a window the counters were read over has no length")
	case end.allocs < start.allocs || end.allocBytes < start.allocBytes || idleTo.allocs < idleFrom.allocs || idleTo.allocBytes < idleFrom.allocBytes:
		return none("a counter went backwards, which means the gateway restarted while it was read")
	}
	idleObjects, idleBytes := (idleTo.allocs-idleFrom.allocs)/idleWall, (idleTo.allocBytes-idleFrom.allocBytes)/idleWall
	objects, bytes := end.allocs-start.allocs-idleObjects*wall, end.allocBytes-start.allocBytes-idleBytes*wall
	if objects <= 0 || bytes <= 0 {
		return none("the gateway allocated no more during the run than it does idle, so there is nothing to attribute to the requests")
	}
	perRequest, bytesPerRequest := objects/float64(handled), bytes/float64(handled)
	return allocationsResult{Value: &perRequest, BytesPerRequest: &bytesPerRequest, IdleObjectsPerSecond: idleObjects}
}
