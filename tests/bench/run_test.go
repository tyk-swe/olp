//go:build bench

package bench_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
)

// settle is the pause between the baseline and the gateway run, and before the
// first, so one run's connections and timers do not spill into the next.
const settle = 3 * time.Second

// runScenario is the whole sequence of a scenario, in one session:
//
//  1. Start the mock upstream, then a fresh installation (its own database)
//     and `olp all`, and provision a provider, route, key and budget through
//     the management API.
//  2. Wait until the gateway serves the new configuration.
//  3. Run the load straight at the mock: the baseline.
//  4. Run the same load through the gateway, watching its CPU and memory.
//  5. Wait for request metadata to reach PostgreSQL, count it, and shut the
//     gateway down cleanly.
//  6. Write the result and, if asked, fail on a missed target.
func runScenario(t *testing.T, sc scenario) {
	t.Helper()
	s := loadSettings(t)
	p := sc.plan(s)

	warnings := preflight(t, p)
	mock := startMock(t, s)
	// While the installation is built, the upstream answers briefly: the
	// gateway certifies a provider by calling its upstream, and caps what it
	// reads from a probe, so a long completion would fail certification.
	mock.configure(t, p.provisioningSpec())

	in := newInstallation(t, s)
	gw := startGateway(t, s, in, limitsFor(p.Concurrency))
	db := openDatabase(t, in)
	c := newConsole(t, gw.public)
	c.signIn(in.BootstrapToken)
	// A budget the gateway only stores is a policy, not an enforcement, and a
	// run against it would measure none of what S3 is for.
	limitsEnforced := c.get("/api/v1/auth/capabilities")["limits_enforced"] == true
	if p.Budget && !limitsEnforced {
		t.Fatalf("%s needs an enforced cost budget, but the gateway reports limits_enforced=false: is %s reachable?", sc.ID, envValkeyURL)
	}
	bench, probe, reconciled := provision(t, c, mock, gw, p)
	awaitServing(t, gw, p, probe.Secret)
	if p.Budget {
		awaitBudget(t, gw, reconciled)
	}
	// Only now does the upstream behave as the scenario says, and a failover
	// route's first target start to fail: it could not have been certified
	// while it did.
	mock.configure(t, p.spec(p.Failover))
	if sc.tune != nil {
		sc.tune(t, s, mock, &p)
	}
	time.Sleep(settle)
	if s.LoadgenCPUs != "" {
		// After the other processes started, which would inherit the mask.
		pinSelf(t, s.LoadgenCPUs)
	}

	mock.reset(t)
	baseline := runLoad(t, "baseline", p.loadConfig(s, mock.origin, p.Baseline, "bench-baseline"))
	baselineStats := mock.stats(t)
	// The gateway is idle while the baseline runs against the mock, so this
	// pause also measures what it costs to do nothing.
	idleFrom := markCPU(gw, mock)
	time.Sleep(settle)
	idleTo := markCPU(gw, mock)

	mock.reset(t)
	runStart := time.Now()
	before := markCPU(gw, mock)
	watch := startSampler(t, gw)
	run := runLoad(t, "gateway", p.loadConfig(s, gw.public+p.SurfacePath, routeSlug, bench.Secret))
	after := markCPU(gw, mock)
	use := watch.finish(t, before, after, idleCores(idleFrom, idleTo), allowedCPUs(mock.pid), gw.VCPUs, allowedCPUs(os.Getpid()))
	runStats := mock.stats(t)

	// Streams a held-open scenario cancels at its end are how it ends, not
	// failures to look into.
	failures := run.Requests.Failed
	if p.SlowReadBPS > 0 {
		failures -= run.Errors.ByKind["canceled"]
	}
	sample, diagnostic := "", int64(0)
	if failures > 0 {
		sample, diagnostic = sampleFailure(t, gw, p, bench.Secret), 1
		t.Logf("the gateway answered %s\nthe gateway's log, last lines:\n%s", sample, tail(withoutAccessLog(gw.Log()), 3000))
	}
	sent := run.Requests.WarmupSent + run.Requests.Sent + diagnostic
	drain, delivered := awaitDrain(t, gw, db, bench.ID, admittedRequests(sent, int64(use.Rejections)), s.Drain)
	// The gateway's pipeline and limiter metrics come from a snapshot it takes
	// every fifteen seconds, which may predate the end of the run: read them
	// once one taken since has appeared.
	final, fresh := metricsAfter(t, gw, time.Now(), metricsRefreshWait)
	byAPI, gap := usageRequestCount(c, bench.ID, runStart)
	// What the key's budget was charged, which the budget's own work leaves
	// behind and a run that did none of it does not.
	var budget *budgetResult
	if p.Budget {
		budget = keyBudget(c, bench.ID)
	}
	rows, err := db.attempts(t.Context(), bench.ID)
	if err != nil {
		t.Fatalf("read the recorded attempts: %v", err)
	}
	shares, err := db.byProvider(t.Context(), bench.ID)
	if err != nil {
		t.Fatalf("read the attempts by provider: %v", err)
	}
	dispatched, err := db.dispatched(t.Context(), bench.ID)
	if err != nil {
		t.Fatalf("count the dispatched requests: %v", err)
	}

	// A clean shutdown is part of the result: the gateway flushes the metadata
	// it still holds and closes its epoch, and a process that cannot is a
	// finding.
	if err := gw.Stop(3 * time.Minute); err != nil {
		t.Errorf("the gateway did not shut down cleanly: %v", err)
	}
	if s.KeepLog {
		keepLog(t, s, sc.ID, gw)
	}
	instance := gw.GatewayInstance
	if instance == "" {
		instance = gw.Instance
	}
	epoch, err := db.epoch(t.Context(), instance)
	if err != nil {
		t.Fatalf("read the gateway's epoch: %v", err)
	}
	deliveredAfter, err := db.delivered(t.Context(), bench.ID)
	if err != nil {
		t.Fatalf("count delivered request metadata: %v", err)
	}

	r := &result{
		Scenario: sc.ID, Title: sc.Title, Why: sc.Why, GeneratedAt: time.Now().UTC(), Scale: s.Scale, Plan: p,
		Environment: collectEnvironment(s),
		Gateway: gatewayInfo{Mode: "all", VCPUs: gw.VCPUs, VCPUSource: gw.Source, CPUsAllowed: gw.CPUs, Settings: gw.Settings, Limits: gw.Limits,
			LimitsEnforced: limitsEnforced},
		Baseline: baseline, Run: run, Resources: use,
		Allocations: allocationsPerRequest(before, after, idleFrom, idleTo, handledRequests(run, p.SlowReadBPS > 0)),
		MockStats: mockStats{BaselineRequests: baselineStats.Requests, RunRequests: runStats.Requests, RunInjectedErrors: runStats.InjectedErrors, RunMaxInFlight: runStats.MaxInFlight,
			RunDialects: runStats.Dialects},
	}
	r.Metadata = metadataResult{
		Sent: sent, DiagnosticRequests: diagnostic, RejectedBeforeAdmission: int64(use.Rejections),
		Delivered: delivered, DeliveredByAPI: byAPI, DeliveredAfterShutdown: deliveredAfter, ReportedGapEvents: gap,
		DroppedByGateway: final[seriesEventsDropped], AbandonedByGateway: final[seriesEventsAbandoned], FailOpen: final[seriesFailOpen],
		Epoch: epoch, Drain: drain, MetricsStale: !fresh,
		Healthy: final[seriesLimiterAvailable] == 1 && final[seriesStreamRetrying] == 0 && final[seriesFailOpen] == 0,
	}
	r.Metadata.account()
	r.Budget = budget
	r.ErrorSample = sample
	r.Attempts = summarizeAttempts(rows, dispatched)
	if gw.Limits.Providers > 1 {
		r.Attempts.ByProvider = shares
	}
	if p.Failover {
		r.Failover = summarizeFailover(p, rows, dispatched)
	}
	if p.SlowReadBPS > 0 {
		r.Streams = summarizeStreams(s, p, run, use)
	}

	r.Preflight = warnings
	judgeReference(r, s)
	computeAdded(r)
	computeThroughput(r, use)
	computeErrors(r)
	judgeValidity(r)
	r.Targets = evaluateTargets(r)
	path, err := writeResult(s.OutDir, r)
	if err != nil {
		t.Fatalf("write the result: %v", err)
	}
	t.Logf("%s: %s\n%s", sc.ID, path, summary(r))
	if s.Enforce {
		for _, failure := range enforcementFailures(r) {
			t.Error(failure)
		}
	}
}

// keepLog saves the gateway's log under the result directory.
func keepLog(t *testing.T, s settings, id string, gw *gatewayProcess) {
	t.Helper()
	dir := filepath.Join(s.OutDir, "raw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strings.ToLower(id)+"-gateway.log"), []byte(gw.Log()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runLoad applies a load and logs its report.
func runLoad(t *testing.T, name string, cfg loadgen.Config) *loadgen.Report {
	t.Helper()
	report, err := loadgen.Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("%s load: %v", name, err)
	}
	report.Name = name
	t.Logf("%s load\n%s", name, report.Text())
	return report
}

// provision builds the installation the gateway serves: an OpenAI provider for
// each upstream model (several, for a load of more streams than one provider
// takes), a route over them (transformed, so that a client of another dialect
// is translated), and the bench key. The probe key is minted last and used to
// learn when the gateway has loaded the lot, so that polling never touches the
// bench key's metadata. It returns how many cost reconciliation passes the
// gateway had logged once the bench key existed.
func provision(t *testing.T, c *console, mock *mockProcess, gw *gatewayProcess, p plan) (bench, probe keyRecord, reconciled int) {
	t.Helper()
	var targets []routeTarget
	var providers []providerRecord
	// A target per provider; the targets of one model share its priority, so a
	// load spread over several providers is still one tier of the route.
	for priority, model := range p.Models {
		for i := 0; i < gw.Limits.Providers; i++ {
			name := fmt.Sprintf("Bench upstream %s %d", model, i+1)
			provider := c.addProvider(name, mock.origin+"/v1", model, gw.Limits.Pool, p.Surfaces)
			providers = append(providers, provider)
			targets = append(targets, routeTarget{Provider: provider, Priority: priority, Timeout: p.timeout()})
		}
	}
	// Time for every attempt, and a little over.
	c.addRoute(routeSlug, targets, len(p.Models), p.timeout()*time.Duration(len(p.Models))+10*time.Second)
	budget := ""
	if p.Budget {
		budget = benchBudget
		// A priced model is what gives a cost budget something to accrue.
		for _, provider := range providers {
			c.addPrice(provider, "2", "4")
		}
	}
	bench = c.addKey("bench", routeSlug, budget)
	reconciled = strings.Count(gw.Log(), budgetsReconciled)
	probe = c.addKey("probe", routeSlug, "")
	return bench, probe, reconciled
}

// budgetsReconciled is what the worker plane logs when a reconciliation pass
// installs cost budgets in Valkey.
const budgetsReconciled = `"msg":"reconciled cost budgets"`

// awaitBudget waits for the pass that makes a new cost budget servable. The
// gateway refuses a key whose spend Valkey does not know yet, and the worker
// plane installs it once a minute, so a budget key is not usable for up to a
// minute after it is created. Waiting on the log rather than on requests keeps
// the wait from leaving metadata of its own.
func awaitBudget(t *testing.T, gw *gatewayProcess, passes int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for strings.Count(gw.Log(), budgetsReconciled) <= passes {
		if time.Now().After(deadline) {
			t.Fatalf("no cost reconciliation pass installed the key's budget within three minutes\n%s", tail(withoutAccessLog(gw.Log()), 3000))
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// tail is the last n bytes of s.
func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// withoutAccessLog drops the gateway's per-request lines from its log, which at
// a benchmark's rates bury everything else it says.
func withoutAccessLog(log string) string {
	var kept []string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, `"msg":"inference request"`) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// probeRequest builds a request to the gateway for the plan's dialect, with a
// prompt of about tokens tokens: the vocabulary of one short word per token
// the load generator uses, repeated.
func probeRequest(ctx context.Context, gw *gatewayProcess, p plan, secret string, tokens int) *http.Request {
	prompt := strings.Repeat("the ", tokens) + "ping"
	var path, body string
	header := http.Header{"Content-Type": {"application/json"}}
	switch p.Dialect {
	case loadgen.Anthropic:
		path = "/anthropic/v1/messages"
		body = `{"model":"` + routeSlug + `","max_tokens":8,"messages":[{"role":"user","content":"` + prompt + `"}]}`
		header.Set("X-Api-Key", secret)
		header.Set("Anthropic-Version", "2023-06-01")
	default:
		path = "/v1/chat/completions"
		body = `{"model":"` + routeSlug + `","max_tokens":8,"messages":[{"role":"user","content":"` + prompt + `"}]}`
		header.Set("Authorization", "Bearer "+secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.public+path, strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header = header
	return req
}

// sampleFailure sends one request with the plan's largest prompt, as the bench
// key, after a run that had failures, and returns what the gateway answered,
// since the generator records only a status. It is one request more than the
// run sent, which the metadata count allows for.
func sampleFailure(t *testing.T, gw *gatewayProcess, p plan, secret string) string {
	t.Helper()
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(probeRequest(t.Context(), gw, p, secret, slices.Max(p.PromptTokens)))
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 800))
	return fmt.Sprintf("status %d: %s", resp.StatusCode, raw)
}

// awaitServing polls the gateway with the probe key until it answers: a
// process loads keys and routes on a poll, not the moment they are created.
func awaitServing(t *testing.T, gw *gatewayProcess, p plan, secret string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for {
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(probeRequest(t.Context(), gw, p, secret, 0))
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Sprintf("status %d: %s", resp.StatusCode, raw)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gateway did not serve the route within a minute; last answer %s\n%s", last, gw.Log())
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// measureStream asks the mock for one streamed completion and returns its size
// in bytes.
func measureStream(t *testing.T, m *mockProcess, model string) int64 {
	t.Helper()
	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"measure"}]}`
	resp, err := http.Post(m.origin+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("measure a stream: %v", err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("measure a stream: status %d after %d bytes: %v", resp.StatusCode, n, err)
	}
	return n
}

// preflight fails a scenario whose concurrency the machine cannot carry, with
// the setting to change, before it spends minutes finding out, and returns
// what may bend the run without stopping it.
func preflight(t *testing.T, p plan) (warnings []string) {
	t.Helper()
	if p.Concurrency < 1000 && p.SlowReadBPS == 0 {
		return nil
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err == nil && p.Concurrency >= 1000 {
		// The generator, the gateway and the mock each hold a socket per stream
		// and the gateway holds a second one upstream.
		if need := uint64(3*p.Concurrency + 4096); limit.Max < need {
			t.Fatalf("%d concurrent requests need about %d file descriptors, and the hard limit is %d: raise it with ulimit -Hn or limits.conf",
				p.Concurrency, need, limit.Max)
		}
	}
	if data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil && p.Concurrency >= 1000 {
		var low, high int
		if _, err := fmt.Sscan(string(data), &low, &high); err == nil {
			if have, need := high-low+1, p.Concurrency*3/2+1000; have < need {
				t.Fatalf("%d concurrent connections need about %d ephemeral ports, and net.ipv4.ip_local_port_range has %d: widen it with sysctl",
					p.Concurrency, need, have)
			}
		}
	}
	if p.SlowReadBPS > 0 {
		if w := tcpMemoryWarning(p); w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// tcpStreamBytes is what a held stream keeps in the kernel on loopback, as
// measured: the gateway's and the mock's send buffers, which are millions of
// bytes each because the loopback MTU is 64 KiB, and a receive buffer.
const tcpStreamBytes = 7 << 20

// tcpMemoryWarning says when the kernel's TCP memory limit is below what the
// held streams' buffers can take, which makes the kernel, not the gateway, what
// bounds the run.
func tcpMemoryWarning(p plan) string {
	data, err := os.ReadFile("/proc/sys/net/ipv4/tcp_mem")
	if err != nil {
		return ""
	}
	var low, pressure, high int64
	if _, err := fmt.Sscan(string(data), &low, &pressure, &high); err != nil {
		return ""
	}
	limit := pressure * int64(os.Getpagesize())
	if need := int64(p.Concurrency) * tcpStreamBytes; need > limit {
		return fmt.Sprintf("the held streams' socket buffers need about %.1f GiB on loopback and the kernel starts limiting TCP memory at %.1f GiB (net.ipv4.tcp_mem), "+
			"so the kernel will bound this run; raise tcp_mem, or run the generator and the mock on other hosts", float64(need)/(1<<30), float64(limit)/(1<<30))
	}
	return ""
}

// allowedCPUs is how many CPUs a process may run on, 0 when unknown.
func allowedCPUs(pid int) int {
	st, err := readProcStatus(pid)
	if err != nil {
		return 0
	}
	n, _ := parseCPUList(st.CPUsAllowed)
	return n
}

func summarizeAttempts(rows []attemptRow, requests int64) attemptStats {
	var attempts int64
	for _, row := range rows {
		attempts += row.Attempts
	}
	stats := attemptStats{Rows: rows}
	if requests > 0 {
		stats.PerRequest = float64(attempts) / float64(requests)
	}
	return stats
}

func summarizeFailover(p plan, rows []attemptRow, requests int64) *failoverResult {
	f := &failoverResult{FirstTarget: p.failingModel(), SecondTarget: p.Baseline}
	for _, row := range rows {
		switch row.UpstreamModel {
		case f.FirstTarget:
			f.FirstTargetAttempts += row.Attempts
		case f.SecondTarget:
			f.SecondTargetAttempts += row.Attempts
		}
		if row.Ordinal > 1 {
			f.RequestsFailedOver += row.Attempts
		}
	}
	if requests > 0 {
		f.FailedOverShare = float64(f.RequestsFailedOver) / float64(requests)
	}
	f.Note = "The gateway's circuit is kept per provider and opens after five counted failures within thirty seconds, " +
		"so once it is open the first target is skipped except for one probe every thirty seconds. " +
		"The steady state therefore measures the skip, and only the requests before the circuit opened and the probes pay for a failed attempt; " +
		"requests_failed_over says how many that was."
	return f
}

func summarizeStreams(s settings, p plan, run *loadgen.Report, use resourceUse) *streamsResult {
	r := &streamsResult{
		TargetConcurrency: p.Concurrency, PeakInFlight: run.Requests.PeakInFlight, PeakAdmitted: use.PeakInferenceAdmitted,
		OutputTokens: s.S6Tokens, ReadBytesPerSec: p.SlowReadBPS, ResponseBytes: p.StreamBytes,
		HeldOpenAtEnd: run.Errors.ByKind["canceled"], TimedOut: run.Errors.ByKind["timeout"], Truncated: run.Errors.ByKind["truncated"],
	}
	r.Failed = run.Requests.Failed - r.HeldOpenAtEnd
	if p.Concurrency > 0 {
		r.Reached = float64(r.PeakInFlight) / float64(p.Concurrency)
		r.GatewayReached = r.PeakAdmitted / float64(p.Concurrency)
	}
	switch growth := (use.RSSPeakMiB - use.RSSBeforeMiB) * 1024; {
	case r.PeakAdmitted <= 0:
		r.RSSPerStreamReason = "the gateway held no streams"
	case growth <= 0:
		r.RSSPerStreamReason = "resident memory did not grow: it was larger before the run, from start-up and certification, than the streams made it, so the growth per stream is below what can be measured this way"
	default:
		per := growth / r.PeakAdmitted
		r.RSSPerStreamKiB = &per
	}
	return r
}

func collectEnvironment(s settings) environment {
	e := environment{
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, HostCPUs: runtime.NumCPU(), LoadAverage: loadAverage(),
		OLPVersion: binaryVersion(s.GatewayBinary), OLPSHA256: binaryDigest(s.GatewayBinary),
		MockCPUs: s.MockCPUs, GatewayCPUs: s.GatewayCPUs, LoadgenCPUs: s.LoadgenCPUs,
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		e.Kernel = strings.TrimSpace(string(data))
	}
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if name, value, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(name) == "model name" {
				e.CPUModel = strings.TrimSpace(value)
				break
			}
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
				e.MemoryGiB = float64(kibibytes(strings.TrimSpace(rest))) / (1 << 20)
				break
			}
		}
	}
	return e
}

// summary is the result in a few lines, for the test log.
func summary(r *result) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, "  "+format+"\n", args...) }
	if p := primaryAdded(r); p != nil {
		line("added latency  p50 %.2f  p95 %.2f  p99 %.2f ms", p.P50, p.P95, p.P99)
	}
	if r.TTFT != nil {
		line("ttft overhead  p50 %.2f  p95 %.2f  p99 %.2f ms", r.TTFT.P50, r.TTFT.P95, r.TTFT.P99)
	}
	line("throughput     %.1f rps sustained, %.1f per vCPU of %d; %.2f gateway CPU ms per request", r.Throughput.SustainedRPS, r.Throughput.RPSPerVCPU, r.Throughput.VCPUs, r.Throughput.CPUMsPerRequest)
	line("memory         RSS %.0f MiB before, peak %.0f, after %.0f; %d threads at peak", r.Resources.RSSBeforeMiB, r.Resources.RSSPeakMiB, r.Resources.RSSAfterMiB, r.Resources.ThreadsPeak)
	if a := r.Allocations; a.Value != nil {
		line("allocations    %.0f heap objects and %.1f KiB per request", *a.Value, *a.BytesPerRequest/1024)
	} else {
		line("allocations    not measured: %s", a.Reason)
	}
	line("errors         %.4f%% of requests; success %.4f%%", 100*r.ErrorRate.Gateway, 100*r.ErrorRate.GatewaySuccessRate)
	line("metadata       %d of %d admitted requests delivered (%.4f%%), %d missing; drain settled=%v", r.Metadata.Delivered, r.Metadata.Admitted, 100*r.Metadata.Completeness, r.Metadata.Missing, r.Metadata.Drain.Settled)
	line("attempts       %.4f per request", r.Attempts.PerRequest)
	for _, t := range r.Targets {
		line("target %-30s %-16s %s", t.ID, t.Status, measured(t))
	}
	for _, p := range r.Validity.Problems {
		line("PROBLEM %s", p)
	}
	for _, w := range r.Validity.Warnings {
		line("warning %s", w)
	}
	return strings.TrimRight(b.String(), "\n")
}
