//go:build bench

package bench_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
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

func TestHarnessParsesMetrics(t *testing.T) {
	m := parseMetrics([]byte(`# HELP olp_x help
# TYPE olp_x gauge
olp_x 3
olp_http_admission_rejections_total{surface="inference"} 12
olp_http_admission_rejections_total{surface="management"} 1
olp_ready 1
broken line
olp_big 1.5e3
`))
	if m["olp_x"] != 3 || m[seriesInferenceRejections] != 12 || m[`olp_http_admission_rejections_total{surface="management"}`] != 1 || m["olp_big"] != 1500 || len(m) != 5 {
		t.Fatalf("%v", m)
	}
}

// TestHarnessLauncherKeepsThePidAndPins proves what the scenarios rely on: the
// script's process is the program's, so its pid is the one signalled and read
// in /proc, and taskset pins it.
func TestHarnessLauncherKeepsThePidAndPins(t *testing.T) {
	if _, err := exec.LookPath("taskset"); err != nil {
		t.Fatalf("taskset is required to pin processes: %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(launcher(t, "probe", sh, "0"), "-c", `echo $$; grep Cpus_allowed_list /proc/self/status`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(out))
	if len(lines) < 3 || lines[0] != strconv.Itoa(cmd.ProcessState.Pid()) {
		t.Errorf("the program ran as pid %q, not the launcher's %d", lines[0], cmd.ProcessState.Pid())
	}
	if lines[len(lines)-1] != "0" {
		t.Errorf("the pinned program may run on CPUs %q, want 0", lines[len(lines)-1])
	}
	// Unpinned, the program is exec'd directly and keeps the machine's CPUs.
	cmd = exec.Command(launcher(t, "unpinned", sh, ""), "-c", `echo $$`)
	out, err = cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != strconv.Itoa(cmd.ProcessState.Pid()) {
		t.Errorf("unpinned: pid %q, %v", out, err)
	}
	// The descriptor limit is raised to the hard limit.
	cmd = exec.Command(launcher(t, "limits", sh, ""), "-c", `[ "$(ulimit -n)" = "$(ulimit -Hn)" ] && echo raised`)
	if out, _ := cmd.Output(); strings.TrimSpace(string(out)) != "raised" {
		t.Errorf("the descriptor limit was not raised to the hard limit")
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// TestHarnessLauncherEndsWithItsParent proves a process the launcher started
// does not outlive a test that was killed, as a test that times out is.
func TestHarnessLauncherEndsWithItsParent(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command("sh", "-c", shellQuote(launcher(t, "watched", sleep, ""))+" 120 & echo $!; wait")
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Fscan(stdout, &pid); err != nil || pid <= 1 {
		parent.Process.Kill()
		t.Fatalf("the launched process's pid: %d, %v", pid, err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(1500 * time.Millisecond)
	if processGone(pid) {
		t.Fatal("the launched process ended while its parent was alive")
	}
	parent.Process.Kill()
	parent.Wait()
	for deadline := time.Now().Add(15 * time.Second); !processGone(pid); {
		if time.Now().After(deadline) {
			t.Fatal("the launched process outlived its parent")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A process that ends on its own leaves no watcher to kill a stranger
	// that is given its pid.
	quick := exec.Command(launcher(t, "quick", sleep, ""), "0")
	if err := quick.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessPinsAndRestoresTheLoadGenerator(t *testing.T) {
	if _, err := exec.LookPath("taskset"); err != nil {
		t.Fatalf("taskset is required to pin processes: %v", err)
	}
	before, err := readProcStatus(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(strings.Split(before.CPUsAllowed, ",")[0], "-")
	t.Run("pinned", func(t *testing.T) {
		pinSelf(t, first)
		if got := allowedCPUs(os.Getpid()); got != 1 {
			t.Errorf("pinned to CPU %s, the process may run on %d", first, got)
		}
		// A thread the runtime starts later inherits the mask.
		done := make(chan int)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			data, _ := os.ReadFile("/proc/thread-self/status")
			st, _ := parseProcStatus(string(data))
			n, _ := parseCPUList(st.CPUsAllowed)
			done <- n
		}()
		if got := <-done; got != 1 {
			t.Errorf("a goroutine's thread may run on %d CPUs", got)
		}
	})
	after, err := readProcStatus(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if after.CPUsAllowed != before.CPUsAllowed {
		t.Errorf("the mask was %s before and %s after the scenario: the next one would inherit it", before.CPUsAllowed, after.CPUsAllowed)
	}
}

// TestHarnessS3IsTheScenarioOfTheExactTokenizer holds S3 to the model the gateway
// counts a prompt of 50K to 100K tokens for with its exact encoder. The other
// scenarios' models are of no family the gateway can name and are charged four
// characters to a token, so a scenario that kept one of those would leave the
// estimator out of the measurement it exists to include.
func TestHarnessS3IsTheScenarioOfTheExactTokenizer(t *testing.T) {
	p := s3.plan(settings{Scale: 1, Duration: time.Minute, Warmup: time.Second})
	if len(p.Models) != 1 || p.Baseline != p.Models[0] {
		t.Fatalf("S3 has one upstream model, which its baseline calls too: %+v", p)
	}
	if family := estimate.FamilyOf(p.Models[0]); family != estimate.FamilyOpenAIO200k {
		t.Errorf("S3's upstream model %q is of the %q family: the gateway would not tokenize its prompts", p.Models[0], family)
	}
}

// deployments is the model each deployment of a LiteLLM configuration calls, by
// the name a client asks for, in the order the file lists them.
func deployments(t *testing.T, file string) map[string][]string {
	t.Helper()
	config, err := os.ReadFile(filepath.Join("..", "..", "deploy", "litellm", file))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?m)^  - model_name: (\S+)\n    litellm_params:\n      model: openai/(\S+)$`).FindAllStringSubmatch(string(config), -1) {
		out[m[1]] = append(out[m[1]], m[2])
	}
	return out
}

// TestHarnessLiteLLMCallsTheUpstreamModelsOfTheScenarios holds the comparison to
// a like-for-like upstream: the mock is configured with the models a scenario
// names, and LiteLLM's deployments have to call those same ones, or its requests
// are answered by the mock's default and the two sides measure different work.
func TestHarnessLiteLLMCallsTheUpstreamModelsOfTheScenarios(t *testing.T) {
	s := settings{Scale: 1, Duration: time.Minute, Warmup: time.Second, S6Tokens: defaultS6Tokens, S6BPS: defaultS6BPS}
	for _, file := range []string{"production.yaml", "high-throughput.yaml"} {
		got := deployments(t, file)
		for group, scenarios := range map[string][]scenario{
			"bench-route":    {s1, s2, s5},
			"bench-priced":   {s3},
			"bench-failover": {s4},
		} {
			for _, sc := range scenarios {
				if want := sc.plan(s).Models; !slices.Equal(got[group], want) {
					t.Errorf("%s: deployments %s call %v, and %s names %v", file, group, got[group], sc.ID, want)
				}
			}
		}
	}
}

// TestHarnessWaitsForMetadataForAsLongAsItArrives holds the wait for request
// metadata to the pace of the pipeline: one consumer persists an event at a time,
// so events that arrive steadily for ten minutes after the load are late, not
// lost, and a wait that ended at a fixed time would score them missing. It ends
// when nothing has arrived for the stall.
func TestHarnessWaitsForMetadataForAsLongAsItArrives(t *testing.T) {
	const stall = 90 * time.Second
	start := time.Unix(1_800_000_000, 0)
	p := newDrainProgress(start, 100, stall)
	// A hundred and fifty events a second, for ten minutes: each observation sees
	// more than the one before.
	now, delivered := start, int64(100)
	for range 600 {
		now, delivered = now.Add(time.Second), delivered+150
		p.observe(now, delivered)
		if p.stalled(now) {
			t.Fatalf("the wait gave up after %s of steady delivery", now.Sub(start))
		}
	}
	if p.delivered != delivered {
		t.Errorf("progress records %d delivered, want %d", p.delivered, delivered)
	}
	// An observation of the same count is not progress, so the stall runs from the
	// last time the count moved.
	last := now
	for _, silence := range []time.Duration{time.Second, stall - time.Second, stall} {
		p.observe(last.Add(silence), delivered)
		if p.stalled(last.Add(silence)) {
			t.Errorf("the wait gave up after %s of silence, before the stall of %s", silence, stall)
		}
	}
	if !p.stalled(last.Add(stall + time.Second)) {
		t.Errorf("the wait did not give up after %s of silence", stall+time.Second)
	}
	// A delivery ends the silence.
	p.observe(last.Add(stall+2*time.Second), delivered+1)
	if p.stalled(last.Add(stall + 3*time.Second)) {
		t.Error("the wait gave up just after an event arrived")
	}
}

func TestHarnessPlansMatchTheRoadmap(t *testing.T) {
	s := settings{Scale: 1, Duration: 60 * time.Second, Warmup: 10 * time.Second, S6Tokens: defaultS6Tokens, S6BPS: defaultS6BPS}
	p1, p2, p3, p4, p5, p6 := s1.plan(s), s2.plan(s), s3.plan(s), s4.plan(s), s5.plan(s), s6.plan(s)

	if p1.Rate != 1000 || p1.StreamShare != 0 || p1.PromptTokens[0] != 0 || p1.Dialect != loadgen.OpenAI {
		t.Errorf("S1 is unary, short and 1,000 RPS: %+v", p1)
	}
	if p2.Rate != 1000 || p2.StreamShare != 1 || *p2.Mock.OutputTokens != 64 || *p2.Mock.IntervalMs != 20 {
		t.Errorf("S2 streams 64 tokens at 20 ms intervals at 1,000 RPS: %+v", p2)
	}
	// 64 tokens at 20 ms is a stream of about 1.3 seconds, so a thousand a
	// second keeps about 1,300 open.
	if p2.Concurrency < 1280 || p2.Concurrency > 1300 {
		t.Errorf("S2 expects %d streams open at once", p2.Concurrency)
	}
	if p3.Rate != 3000 || p3.StreamShare != 0.5 || p3.MaxTokens != 16 || !p3.Budget ||
		len(p3.PromptTokens) != 3 || p3.PromptTokens[0] != 50_000 || p3.PromptTokens[1] != 75_000 || p3.PromptTokens[2] != 100_000 {
		t.Errorf("S3 is 50K/75K/100K prompts, half streaming, max_tokens 16, with a budget, at 3,000 RPS: %+v", p3)
	}
	if !p4.Failover || len(p4.Models) != 2 || p4.Models[0] == p4.Baseline || p4.Models[1] != p4.Baseline || p4.failingModel() != p4.Models[0] {
		t.Errorf("S4 fails its first target and the baseline is the second: %+v", p4)
	}
	if p5.Dialect != loadgen.Anthropic || p5.SurfacePath != "/anthropic" || p5.StreamShare != 1 || len(p5.Surfaces) != 2 {
		t.Errorf("S5 streams Anthropic Messages to an OpenAI upstream: %+v", p5)
	}
	if p6.Concurrency != 10_000 || p6.StreamShare != 1 || p6.SlowReadBPS != defaultS6BPS {
		t.Errorf("S6 holds 10,000 slow streams: %+v", p6)
	}
	// The arrival rate reaches the full count exactly when the schedule ends.
	if got := p6.Rate * (p6.WarmupSeconds + p6.DurationSeconds); got < 9999 || got > 10001 {
		t.Errorf("S6 opens %.0f streams over its schedule", got)
	}
	// The gateway ends a stream thirty seconds after its writes block, so S6
	// opens them over less than that whatever the other scenarios run for, and
	// keeps a shorter schedule it is given.
	if p6.WarmupSeconds != 3 || p6.DurationSeconds != 15 || p6.DrainSeconds != 15 {
		t.Errorf("S6 opens its streams over %.0f s of warmup and %.0f s measured, and holds them for %.0f s", p6.WarmupSeconds, p6.DurationSeconds, p6.DrainSeconds)
	}
	short := s
	short.Warmup, short.Duration = time.Second, 5*time.Second
	if p := s6.plan(short); p.WarmupSeconds != 1 || p.DurationSeconds != 5 {
		t.Errorf("S6 lengthens a shorter schedule to %.0f s and %.0f s", p.WarmupSeconds, p.DurationSeconds)
	}

	s.Scale = 0.05
	if p := s1.plan(s); p.Rate != 50 {
		t.Errorf("at scale 0.05 S1 runs %v RPS", p.Rate)
	}
	if p := s6.plan(s); p.Concurrency != 500 {
		t.Errorf("at scale 0.05 S6 holds %d streams", p.Concurrency)
	}
	if p := s3.plan(s); p.PromptTokens[2] != 100_000 {
		t.Error("scale changes the prompt size: it multiplies rates and concurrency only")
	}
}

func TestHarnessMockSpecs(t *testing.T) {
	p := s4.plan(settings{Scale: 1, Duration: time.Minute, Warmup: time.Second})
	healthy, failing := p.spec(false), p.spec(true)
	if healthy.Models[p.Models[0]].Status != nil {
		t.Error("the first target fails before it is armed")
	}
	if st := failing.Models[p.failingModel()].Status; st == nil || *st != 503 {
		t.Errorf("the first target is not armed to return 503: %+v", failing.Models)
	}
	if failing.Models[p.Baseline].Status != nil {
		t.Error("the second target fails too")
	}
	// A spec lists every model, because replacing the configuration drops the
	// rest and a model the mock does not list cannot be discovered.
	if len(failing.Models) != 2 || len(p.provisioningSpec().Models) != 2 {
		t.Errorf("specs omit models: %+v", failing)
	}
	if out := p.provisioningSpec().Default; *out.OutputTokens != 16 || *out.TTFTMs != 0 {
		t.Errorf("provisioning is not brief: %+v", out)
	}
}

func TestHarnessSizesTheGatewayForTheScenario(t *testing.T) {
	small := limitsFor(20)
	if small.Providers != 1 || small.Pool.MaxConnsPerHost < 64 || small.MaxInFlight < 256 || small.MaxConnections < 1024 {
		t.Errorf("a small scenario is sized below OLP's defaults: %+v", small)
	}
	// S2 at full scale holds about 1,300 streams, which the default pool of 64
	// connections and 256 in-flight requests could not carry.
	s2 := limitsFor(1280)
	if s2.Providers != 1 || s2.Pool.MaxConnsPerHost < 1280 || s2.MaxInFlight < 1280 || s2.MaxConnections < 1280 {
		t.Errorf("S2 would be shed: %+v", s2)
	}
	big := limitsFor(10_000)
	if big.Providers < 3 || big.Pool.MaxConnsPerHost > 4096 || big.Pool.MaxIdleConnsPerHost > big.Pool.MaxConnsPerHost ||
		big.Pool.MaxIdleConns < big.Pool.MaxIdleConnsPerHost || big.MaxInFlight < 10_000 || big.MaxInFlight > 100_000 || big.MaxConnections > 100_000 {
		t.Errorf("S6 cannot be carried: %+v", big)
	}
	// Each provider's share of the streams, with a third to spare, fits its pool.
	if share := 10_000 / big.Providers * 13 / 10; share > big.Pool.MaxConnsPerHost {
		t.Errorf("a provider's share of %d streams exceeds its %d connections", share, big.Pool.MaxConnsPerHost)
	}
}

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

func TestHarnessJudgesS1AgainstItsTargets(t *testing.T) {
	r := s1Result(1.5, 4)
	r.Targets = evaluateTargets(r)
	for _, id := range []string{"s1-2vcpu-added-p95", "s1-2vcpu-added-p99", "request-metadata-lost"} {
		if got := findTarget(t, r.Targets, id); got.Status != statusMet || got.MeetsTarget == nil || !*got.MeetsTarget {
			t.Errorf("%s: %+v", id, got)
		}
	}
	if got := findTarget(t, r.Targets, "s1-2vcpu-added-p95"); got.Measured == nil || *got.Measured != 1.5 {
		t.Errorf("p95 measured %v, want the added 1.5 ms", got.Measured)
	}
	for _, id := range []string{"added-latency-below-litellm", "rps-per-vcpu-above-litellm"} {
		if got := findTarget(t, r.Targets, id); got.Status != statusNeedsComparison || got.MeetsTarget != nil {
			t.Errorf("%s is a comparison with LiteLLM and must not be judged here: %+v", id, got)
		}
	}
	if failures := enforcementFailures(r); len(failures) != 0 {
		t.Errorf("a clean reference run fails enforcement: %v", failures)
	}

	missed := s1Result(2.5, 6)
	missed.Targets = evaluateTargets(missed)
	for _, id := range []string{"s1-2vcpu-added-p95", "s1-2vcpu-added-p99"} {
		if got := findTarget(t, missed.Targets, id); got.Status != statusMissed || got.MeetsTarget == nil || *got.MeetsTarget {
			t.Errorf("%s should be missed: %+v", id, got)
		}
	}
	if failures := enforcementFailures(missed); len(failures) != 2 {
		t.Errorf("enforcement reports %d failures for two misses: %v", len(failures), failures)
	}

	// The limits are "at most": adding exactly 2 ms at p95 and 5 ms at p99 meets
	// them, and a hair more at either misses.
	for _, tc := range []struct {
		name             string
		added95, added99 float64
		meets95, meets99 bool
	}{
		{"exactly at both limits", 2, 5, true, true},
		{"over the p95 limit", 2.01, 5, false, true},
		{"over the p99 limit", 2, 5.01, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s1Result(tc.added95, tc.added99)
			r.Targets = evaluateTargets(r)
			for id, meets := range map[string]bool{"s1-2vcpu-added-p95": tc.meets95, "s1-2vcpu-added-p99": tc.meets99} {
				got := findTarget(t, r.Targets, id)
				if got.MeetsTarget == nil || *got.MeetsTarget != meets || (got.Status == statusMet) != meets {
					t.Errorf("%s: %+v, want meets %v", id, got, meets)
				}
			}
		})
	}
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

func TestHarnessJudgesS3(t *testing.T) {
	r := s1Result(1, 1)
	r.Scenario = "S3"
	r.Run.Rates = loadgen.Rates{TargetRPS: 3000, OfferedRPS: 2999, Ratio: 0.9997, SuccessRate: 1}
	r.Targets = evaluateTargets(r)
	if got := findTarget(t, r.Targets, "s3-success-rate"); got.Status != statusMet {
		t.Errorf("a perfect full-rate run: %+v", got)
	}
	if got := findTarget(t, r.Targets, "s3-fewer-vcpu-than-litellm"); got.Status != statusNeedsComparison {
		t.Errorf("the vCPU comparison is LiteLLM's to settle: %+v", got)
	}
	r.Run.Rates.SuccessRate = 0.9999
	r.Targets = evaluateTargets(r)
	if got := findTarget(t, r.Targets, "s3-success-rate"); got.Status != statusMissed {
		t.Errorf("one failure in ten thousand is not 100%%: %+v", got)
	}
	// A generator that fell behind cannot pass by sending less.
	r.Run.Rates = loadgen.Rates{TargetRPS: 3000, OfferedRPS: 2000, Ratio: 0.67, SuccessRate: 1}
	r.Targets = evaluateTargets(r)
	if got := findTarget(t, r.Targets, "s3-success-rate"); got.Status != statusMissed {
		t.Errorf("a run that offered two thirds of the rate passed: %+v", got)
	}

	// The edges of what counts as 3,000 RPS at a full success rate: 99% of the
	// rate offered is enough, a hair under is not, and the target rate itself
	// may not be lowered.
	for _, tc := range []struct {
		name  string
		rates loadgen.Rates
		met   bool
	}{
		{"offering exactly 99% of the rate", loadgen.Rates{TargetRPS: 3000, OfferedRPS: 2970, Ratio: 0.99, SuccessRate: 1}, true},
		{"offering a hair under 99%", loadgen.Rates{TargetRPS: 3000, OfferedRPS: 2969.9, Ratio: 0.9899, SuccessRate: 1}, false},
		{"a target rate of 3,000", loadgen.Rates{TargetRPS: 3000, OfferedRPS: 3000, Ratio: 1, SuccessRate: 1}, true},
		{"a target rate below 3,000", loadgen.Rates{TargetRPS: 2999, OfferedRPS: 2999, Ratio: 1, SuccessRate: 1}, false},
		{"one request failing", loadgen.Rates{TargetRPS: 3000, OfferedRPS: 3000, Ratio: 1, SuccessRate: 0.99999}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s1Result(1, 1)
			r.Scenario, r.Run.Rates = "S3", tc.rates
			r.Targets = evaluateTargets(r)
			if got := findTarget(t, r.Targets, "s3-success-rate"); (got.Status == statusMet) != tc.met || got.MeetsTarget == nil || *got.MeetsTarget != tc.met {
				t.Errorf("%+v, want met %v", got, tc.met)
			}
		})
	}
}

func TestHarnessComputesTheGatewaysOverhead(t *testing.T) {
	base := report(1000, true, summary1(100, 120, 130), summary1(50, 60, 70), summary1(150, 170, 180), summary1(20, 25, 30))
	run := report(1000, true, summary1(104, 130, 150), summary1(52, 63, 75), summary1(156, 180, 190), summary1(26, 33, 41))
	r := &result{Scenario: "S3", Baseline: base, Run: run, Gateway: gatewayInfo{VCPUs: 4}}
	run.Rates.ThroughputRPS = 800
	computeAdded(r)
	if a := r.AddedLatency.All; a == nil || a.P50 != 4 || a.P95 != 10 || a.P99 != 20 {
		t.Errorf("added latency %+v", a)
	}
	if a := r.AddedLatency.Stream; a == nil || a.P50 != 6 || a.P95 != 10 || a.P99 != 10 {
		t.Errorf("added stream latency %+v", a)
	}
	if a := r.TTFT; a == nil || a.P50 != 6 || a.P95 != 8 || a.P99 != 11 {
		t.Errorf("ttft overhead %+v", a)
	}
	// 70 seconds of the gateway's CPU, 10 of them idle, for 1,000 requests.
	computeThroughput(r, resourceUse{Gateway: cpuUse{Seconds: 70}, WallSeconds: 100, GatewayIdleCores: 0.1})
	tp := r.Throughput
	if tp.RPSPerVCPU != 200 || tp.VCPUs != 4 || tp.CPUMsPerRequestGross != 70 || tp.CPUMsPerRequest != 60 || tp.RequestsPerCPUSecond < 16.6 || tp.RequestsPerCPUSecond > 16.7 {
		t.Errorf("throughput %+v", tp)
	}

	// With nothing to compare, the overhead is absent rather than zero.
	unary := &result{Baseline: report(10, true, summary1(1, 2, 3), summary1(1, 2, 3), loadgen.Summary{}, loadgen.Summary{}),
		Run: report(10, true, summary1(1, 2, 3), summary1(1, 2, 3), loadgen.Summary{}, loadgen.Summary{})}
	computeAdded(unary)
	if unary.TTFT != nil || unary.TTFTReason == "" || unary.AddedLatency.Stream != nil {
		t.Errorf("a unary scenario reports a first token: %+v %+v", unary.TTFT, unary.AddedLatency)
	}
}

func TestHarnessJudgesValidity(t *testing.T) {
	r := s1Result(1, 1)
	r.Resources = resourceUse{Rejections: 3, Mock: cpuUse{Cores: 1.9, Allowed: 2}, Gateway: cpuUse{Cores: 1.99, Allowed: 2}}
	r.Metadata.Drain = drainResult{Settled: false, Reason: "still 4 short"}
	judgeValidity(r)
	if r.Validity.Valid || len(r.Validity.Problems) != 2 {
		t.Errorf("shedding and an unsettled drain are problems: %+v", r.Validity)
	}
	joined := strings.Join(r.Validity.Warnings, "\n")
	if !strings.Contains(joined, "mock upstream") || !strings.Contains(joined, "at its limit") {
		t.Errorf("a saturated mock and gateway are warnings: %+v", r.Validity)
	}

	// S6's premise is that the gateway holds the streams.
	held := s1Result(1, 1)
	held.Streams = &streamsResult{TargetConcurrency: 10000, PeakInFlight: 10000, Reached: 1, PeakAdmitted: 40, GatewayReached: 0.004}
	judgeValidity(held)
	problems := strings.Join(held.Validity.Problems, "\n")
	if held.Validity.Valid || !strings.Contains(problems, envS6Tokens) || !strings.Contains(problems, envDuration) {
		t.Errorf("a gateway that held 40 of 10,000 streams was valid, or did not say what to change: %+v", held.Validity)
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

func TestHarnessWritesResults(t *testing.T) {
	dir := t.TempDir()
	r := s1Result(1, 1)
	r.Scenario = "S1"
	r.Targets = evaluateTargets(r)
	r.Allocations = allocationsResult{Reason: "no mechanism"}
	path, err := writeResult(dir, r)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "s1.json") {
		t.Errorf("result written to %s", path)
	}
	for _, name := range []string{"raw/s1-baseline.json", "raw/s1-gateway.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the raw report %s is missing: %v", name, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	// Every figure the roadmap asks of a run is present.
	for _, key := range []string{"added_latency_ms", "ttft_overhead_ms", "throughput", "resources", "allocations_per_request", "error_rate",
		"request_metadata_completeness", "targets", "baseline", "gateway_run", "reference_conditions", "environment", "validity"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("the result has no %q", key)
		}
	}
	// A figure without a source is null beside a reason, never a number.
	allocs := decoded["allocations_per_request"].(map[string]any)
	if allocs["value"] != nil || allocs["reason"] == "" {
		t.Errorf("allocations %v", allocs)
	}
	// A comparison target is neither met nor missed.
	for _, target := range decoded["targets"].([]any) {
		m := target.(map[string]any)
		if m["status"] == statusNeedsComparison && m["meets_target"] != nil {
			t.Errorf("target %v is judged without a comparison", m["id"])
		}
	}
}
