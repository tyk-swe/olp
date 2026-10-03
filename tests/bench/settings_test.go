//go:build bench

package bench_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every knob of a scenario run is an environment variable, so make bench, the
// release qualification and a person at a shell all configure it the same way.
const (
	envDatabaseURL = "OLP_TEST_DATABASE_URL"
	envValkeyURL   = "OLP_TEST_VALKEY_URL"
	envBinary      = "OLP_TEST_BINARY"
	envMockBinary  = "OLP_BENCH_MOCK_BINARY"

	envScale       = "OLP_BENCH_SCALE"
	envDuration    = "OLP_BENCH_DURATION"
	envWarmup      = "OLP_BENCH_WARMUP"
	envDrain       = "OLP_BENCH_DRAIN"
	envLateAfter   = "OLP_BENCH_LATE_AFTER"
	envMockCPUs    = "OLP_BENCH_MOCK_CPUS"
	envGatewayCPUs = "OLP_BENCH_GATEWAY_CPUS"
	envLoadgenCPUs = "OLP_BENCH_LOADGEN_CPUS"
	envEnforce     = "OLP_BENCH_ENFORCE"
	envKeepLog     = "OLP_BENCH_KEEP_GATEWAY_LOG"
	envOut         = "OLP_BENCH_OUT"

	// S6 is shaped by how long a stream stays open, which these set.
	envS6Tokens = "OLP_BENCH_S6_OUTPUT_TOKENS"
	envS6BPS    = "OLP_BENCH_S6_READ_BPS"
)

// settings is one run's configuration: the services it runs against, the
// binaries it starts, how much of the roadmap's load it applies and where the
// processes are pinned.
type settings struct {
	DatabaseURL, ValkeyURL    string
	GatewayBinary, MockBinary string

	// Scale multiplies every scenario's rate and concurrency; 1 is the
	// roadmap's full rates.
	Scale float64
	// Duration is the measured period of a run, Warmup precedes it at the same
	// rate, and Drain is how long to wait for request metadata to land.
	Duration, Warmup, Drain time.Duration
	// LateAfter is when a send counts as late; see loadgen.Config.
	LateAfter time.Duration

	// The CPU lists pin each process with taskset when set.
	MockCPUs, GatewayCPUs, LoadgenCPUs string

	// Enforce turns every target the run could check into a failure when it is
	// missed or could not be checked.
	Enforce bool
	OutDir  string
	// KeepLog copies the gateway's log, which holds a line for every request,
	// next to the result.
	KeepLog bool

	// S6Tokens and S6BPS shape the slow-reader scenario.
	S6Tokens, S6BPS int
}

// Defaults of the measured period. They are the same at every scale, so a
// smoke run differs from a reference run in load, not in how it is measured.
const (
	defaultDuration  = 60 * time.Second
	defaultWarmup    = 10 * time.Second
	defaultDrain     = 90 * time.Second
	defaultLateAfter = 5 * time.Millisecond

	// A stream of 40,000 tokens is about 7.6 MB, more than the socket buffers
	// on a loopback path hold, read at 8 KiB a second: no stream finishes in
	// the minutes a run takes.
	defaultS6Tokens = 40000
	defaultS6BPS    = 8192
)

// loadSettings reads the environment. A scenario that is run without the
// services it needs fails, naming everything that is missing; skipping would
// let a misconfigured reference run pass without measuring anything.
func loadSettings(t *testing.T) settings {
	t.Helper()
	s := settings{
		DatabaseURL:   os.Getenv(envDatabaseURL),
		ValkeyURL:     os.Getenv(envValkeyURL),
		GatewayBinary: os.Getenv(envBinary),
		MockBinary:    os.Getenv(envMockBinary),
		MockCPUs:      os.Getenv(envMockCPUs),
		GatewayCPUs:   os.Getenv(envGatewayCPUs),
		LoadgenCPUs:   os.Getenv(envLoadgenCPUs),
	}
	var missing []string
	for name, value := range map[string]string{
		envDatabaseURL: s.DatabaseURL, envValkeyURL: s.ValkeyURL, envBinary: s.GatewayBinary, envMockBinary: s.MockBinary,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		t.Fatalf("%s must be set; run make bench, which provisions the services and builds the binaries", strings.Join(missing, ", "))
	}
	for _, binary := range []string{s.GatewayBinary, s.MockBinary} {
		if info, err := os.Stat(binary); err != nil || info.IsDir() {
			t.Fatalf("%s is not an executable file: %v", binary, err)
		}
	}
	var err error
	if s.Scale, err = parseScale(os.Getenv(envScale)); err != nil {
		t.Fatalf("%s: %v", envScale, err)
	}
	for _, d := range []struct {
		name     string
		dst      *time.Duration
		fallback time.Duration
	}{
		{envDuration, &s.Duration, defaultDuration}, {envWarmup, &s.Warmup, defaultWarmup},
		{envDrain, &s.Drain, defaultDrain}, {envLateAfter, &s.LateAfter, defaultLateAfter},
	} {
		if *d.dst, err = parseDuration(os.Getenv(d.name), d.fallback); err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
	}
	if s.Duration < time.Second {
		t.Fatalf("%s must be at least one second", envDuration)
	}
	for name, list := range map[string]string{envMockCPUs: s.MockCPUs, envGatewayCPUs: s.GatewayCPUs, envLoadgenCPUs: s.LoadgenCPUs} {
		if list == "" {
			continue
		}
		if _, err := parseCPUList(list); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if s.Enforce, err = parseFlag(os.Getenv(envEnforce)); err != nil {
		t.Fatalf("%s: %v", envEnforce, err)
	}
	if s.KeepLog, err = parseFlag(os.Getenv(envKeepLog)); err != nil {
		t.Fatalf("%s: %v", envKeepLog, err)
	}
	if s.Enforce && s.Scale != 1 {
		t.Fatalf("%s=1 judges the roadmap's targets, which are stated at full rates: unset %s, or unset %s for a smoke run", envEnforce, envScale, envEnforce)
	}
	if shared := sharedCPUs(s); s.Enforce && len(shared) > 0 {
		t.Fatalf("%s=1 judges the roadmap's targets, which are stated for processes with CPUs of their own, and %s", envEnforce, strings.Join(shared, ", and "))
	}
	if s.S6Tokens, err = parsePositive(os.Getenv(envS6Tokens), defaultS6Tokens); err != nil {
		t.Fatalf("%s: %v", envS6Tokens, err)
	}
	if s.S6BPS, err = parsePositive(os.Getenv(envS6BPS), defaultS6BPS); err != nil {
		t.Fatalf("%s: %v", envS6BPS, err)
	}
	// The tests run in their package's directory, so the result directory is
	// resolved against the repository root, which is where make bench is run
	// from and where a relative OLP_BENCH_OUT is meant to be relative to.
	root, rootErr := repositoryRoot()
	switch s.OutDir = os.Getenv(envOut); {
	case s.OutDir == "" && rootErr != nil:
		t.Fatal(rootErr)
	case s.OutDir == "":
		s.OutDir = filepath.Join(root, ".local", "bench")
	case !filepath.IsAbs(s.OutDir) && rootErr != nil:
		t.Fatal(rootErr)
	case !filepath.IsAbs(s.OutDir):
		s.OutDir = filepath.Join(root, s.OutDir)
	}
	return s
}

// repositoryRoot finds the module root from the test's working directory,
// which is the package directory.
func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory; set %s", envOut)
		}
		dir = parent
	}
}

// parseScale reads OLP_BENCH_SCALE: a positive number, 1 when unset.
func parseScale(s string) (float64, error) {
	if s == "" {
		return 1, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 100 {
		return 0, fmt.Errorf("%q is not a number above 0 and at most 100", s)
	}
	return v, nil
}

func parseDuration(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration such as 30s", s)
	}
	return d, nil
}

func parsePositive(s string, fallback int) (int, error) {
	if s == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a positive integer", s)
	}
	return n, nil
}

// parseFlag reads a switch that is on only when it is "1" or "true".
func parseFlag(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	}
	return false, fmt.Errorf("%q is not 0 or 1", s)
}

// parseCPUList counts the CPUs of a taskset list such as "0-3,6", rejecting
// anything taskset would.
func parseCPUList(list string) (int, error) {
	set, err := cpuSet(list)
	return len(set), err
}

// cpuSet is the CPUs of a taskset list such as "0-3,6".
func cpuSet(list string) (map[int]bool, error) {
	seen := map[int]bool{}
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		first, err := strconv.Atoi(lo)
		if err != nil || first < 0 {
			return nil, fmt.Errorf("%q is not a CPU list such as 0-3,6", list)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil || last < first {
				return nil, fmt.Errorf("%q is not a CPU list such as 0-3,6", list)
			}
		}
		if last-first > 4095 {
			return nil, fmt.Errorf("%q names more CPUs than a machine has", list)
		}
		for cpu := first; cpu <= last; cpu++ {
			seen[cpu] = true
		}
	}
	return seen, nil
}

// sharedCPUs lists the pairs of pinned processes that were given a CPU in
// common, as sentences naming the CPUs. A reference run gives the gateway, the
// mock upstream and the load generator CPUs of their own, since a mock or a
// generator that competes with the gateway for a core bends exactly what is
// measured. A process that is not pinned shares nothing it can be told of.
func sharedCPUs(s settings) []string {
	pins := []struct{ name, list string }{{"the gateway", s.GatewayCPUs}, {"the mock upstream", s.MockCPUs}, {"the load generator", s.LoadgenCPUs}}
	var shared []string
	for i, a := range pins {
		for _, b := range pins[i+1:] {
			if a.list == "" || b.list == "" {
				continue
			}
			first, errA := cpuSet(a.list)
			second, errB := cpuSet(b.list)
			if errA != nil || errB != nil {
				continue
			}
			var both []int
			for cpu := range first {
				if second[cpu] {
					both = append(both, cpu)
				}
			}
			if len(both) > 0 {
				slices.Sort(both)
				names := make([]string, len(both))
				for j, cpu := range both {
					names[j] = strconv.Itoa(cpu)
				}
				shared = append(shared, fmt.Sprintf("%s and %s both run on CPU %s", a.name, b.name, strings.Join(names, ",")))
			}
		}
	}
	return shared
}

// scaled applies the scale to a count that cannot drop below one.
func scaled(n int, scale float64) int {
	return max(1, int(math.Round(float64(n)*scale)))
}
