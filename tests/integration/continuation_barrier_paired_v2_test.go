//go:build integration

package integration_test

// Measurement-only command loop for the historical native encrypted reference.
// This file is overlaid without edits to product or the frozen v1 oracle when
// compiling the 29e18268 reference checkout. The same file compiles at C.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	olpruntime "github.com/tyk-swe/olp/internal/runtime"
)

type pairedBarrierCommand struct {
	Name     string `json:"name"`
	Block    int    `json:"block"`
	Position int    `json:"position"`
}

type pairedBarrierObservation struct {
	WorkflowUS    float64 `json:"workflow_us"`
	FirstEventUS  float64 `json:"first_event_us"`
	WireToolUS    float64 `json:"wire_tool_us,omitempty"`
	ToolVisibleUS float64 `json:"tool_visible_us,omitempty"`
	ActionReadyUS float64 `json:"action_ready_us"`
	ClaimUS       float64 `json:"claim_us,omitempty"`
	JournalUS     float64 `json:"journal_us,omitempty"`
	ReadyUS       float64 `json:"ready_us,omitempty"`
	Events        int     `json:"events"`
	Actions       int     `json:"actions"`
}

type pairedBarrierRun struct {
	Name                  string                     `json:"name"`
	Block                 int                        `json:"block"`
	Position              int                        `json:"position"`
	Samples               int                        `json:"samples"`
	Dispatches            int64                      `json:"dispatches"`
	FirstRequests         int64                      `json:"first_requests"`
	NextRequests          int64                      `json:"next_requests"`
	NativeEvents          int                        `json:"native_events"`
	FirstTurnObservations int                        `json:"first_turn_observations"`
	FinalObservations     int                        `json:"final_observations"`
	Actions               int                        `json:"actions"`
	ReadyChecks           int                        `json:"ready_checks"`
	Rejected              int                        `json:"rejected"`
	HistoryBytes          int                        `json:"history_bytes"`
	StateBytes            int                        `json:"state_bytes"`
	Metrics               map[string]float64         `json:"metrics"`
	Observations          []pairedBarrierObservation `json:"observations"`
	GCCount               uint32                     `json:"gc_count"`
	GCPauseNS             uint64                     `json:"gc_pause_ns"`
	GoroutinesBefore      int                        `json:"goroutines_before"`
	GoroutinesAfter       int                        `json:"goroutines_after"`
	SchedulerWaits        uint64                     `json:"scheduler_waits"`
	SchedulerP99US        float64                    `json:"scheduler_p99_us"`
}

func pairedBarrierUS(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000 }

// A multi-minute paired experiment must use the same five-second authority
// refresh lifecycle as a production gateway. A one-shot h.refresh() becomes
// stale after 60 seconds even when storage and the fixture are healthy.
func startPairedBarrierAuthority(t *testing.T, h *accessHarness, key string) {
	t.Helper()
	h.Runtime.Start(t.Context())
	t.Cleanup(h.Runtime.Stop)
	if _, err := h.Runtime.Authenticate(key); err != nil {
		t.Fatalf("initial paired key authority: %v", err)
	}
}

type pairedSchedulerHistogram struct {
	buckets []float64
	counts  []uint64
}

func pairedSchedulerSnapshot() pairedSchedulerHistogram {
	sample := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindFloat64Histogram {
		panic("runtime scheduler histogram unavailable")
	}
	h := sample[0].Value.Float64Histogram()
	return pairedSchedulerHistogram{append([]float64(nil), h.Buckets...), append([]uint64(nil), h.Counts...)}
}

func pairedSchedulerDelta(before, after pairedSchedulerHistogram) (uint64, float64) {
	if len(before.counts) != len(after.counts) || len(before.buckets) != len(after.buckets) {
		panic("runtime scheduler histogram shape changed")
	}
	var total uint64
	for i := range before.counts {
		if before.buckets[i] != after.buckets[i] || after.counts[i] < before.counts[i] {
			panic("runtime scheduler histogram regressed")
		}
		total += after.counts[i] - before.counts[i]
	}
	if total == 0 {
		return 0, 0
	}
	target, cumulative := (total*99+99)/100, uint64(0)
	for i := range before.counts {
		cumulative += after.counts[i] - before.counts[i]
		if cumulative >= target {
			upper := after.buckets[i+1]
			if math.IsInf(upper, 1) {
				upper = after.buckets[i]
			}
			return total, upper * 1e6
		}
	}
	panic("runtime scheduler histogram missing observations")
}

func pairedBarrierReferenceRun(t *testing.T, command pairedBarrierCommand, documents []barrierDocuments, provider *barrierProvider, binding barrierBinding, client *http.Client, relayURL, gatewayURL, key, slug string) pairedBarrierRun {
	t.Helper()
	parts := strings.Split(command.Name, "/")
	if len(parts) != 3 || (parts[0] != "small" && parts[0] != "large") || (parts[1] != "c1" && parts[1] != "c8") || (parts[2] != "relay" && parts[2] != "gateway" && parts[2] != "reference") || command.Block < 0 || command.Block >= 32 || command.Position < 0 || command.Position >= 8 {
		t.Fatalf("invalid reference command: %+v", command)
	}
	d, concurrency, path := documents[0], 1, parts[2]
	if parts[0] == "large" {
		d = documents[1]
	}
	if parts[1] == "c8" {
		concurrency = 8
	}
	endpoint, secret, model := gatewayURL, key, slug
	if path == "relay" {
		endpoint, secret, model = relayURL, "barrier-reference-key", vendorModel
	}
	// The original oracle's warmup is repeated before every 24-workflow subrun.
	// It is checked, excluded and never substituted for a failed measured case.
	if _, err := barrierWorkflow(t.Context(), binding, provider, client, d, endpoint, secret, model, path); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	schedulerBefore := pairedSchedulerSnapshot()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				peak.Store(max(peak.Load(), m.HeapAlloc))
			}
		}
	}()
	const samples = 24
	first, next, cpu, goroutines, started := provider.first.Load(), provider.next.Load(), lifecycleCPU(), runtime.NumGoroutine(), time.Now()
	results := make([]barrierSample, samples)
	errs := make(chan error, samples)
	var wg sync.WaitGroup
	for worker := range concurrency {
		wg.Go(func() {
			for i := worker; i < samples; i += concurrency {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				result, err := barrierWorkflow(ctx, binding, provider, client, d, endpoint, secret, model, path)
				cancel()
				results[i] = result
				if err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	elapsed, cpu := time.Since(started), lifecycleCPU()-cpu
	first, next = provider.first.Load()-first, provider.next.Load()-next
	close(stop)
	<-stopped
	runtime.ReadMemStats(&after)
	peak.Store(max(peak.Load(), after.HeapAlloc))
	schedulerWaits, schedulerP99US := pairedSchedulerDelta(schedulerBefore, pairedSchedulerSnapshot())
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if first != samples || next != samples {
		t.Fatalf("reference dispatch counts changed: %d/%d", first, next)
	}
	run := pairedBarrierRun{
		Name: command.Name, Block: command.Block, Position: command.Position, Samples: samples,
		Dispatches: first + next, FirstRequests: first, NextRequests: next,
		HistoryBytes: d.historyBytes, StateBytes: len(barrierPayload(d, true)),
		Metrics:      map[string]float64{"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / samples, "process-cpu-ns/op": float64(cpu) / samples, "B/op": float64(after.TotalAlloc-before.TotalAlloc) / samples, "allocs/op": float64(after.Mallocs-before.Mallocs) / samples, "sampled-heap-growth-B": float64(peak.Load() - before.HeapAlloc)},
		Observations: make([]pairedBarrierObservation, 0, samples),
		GCCount:      after.NumGC - before.NumGC, GCPauseNS: after.PauseTotalNs - before.PauseTotalNs,
		GoroutinesBefore: goroutines, GoroutinesAfter: runtime.NumGoroutine(),
		SchedulerWaits: schedulerWaits, SchedulerP99US: schedulerP99US,
	}
	values := map[string][]time.Duration{"workflow": {}, "first-event": {}, "wire-tool": {}, "action-ready": {}}
	if path == "reference" {
		values["claim-commit"], values["dispatch-journal"], values["ready-commit"] = nil, nil, nil
	}
	for _, result := range results {
		run.NativeEvents += result.events
		run.Actions += result.actions
		values["workflow"] = append(values["workflow"], result.workflow)
		values["first-event"] = append(values["first-event"], result.firstEvent)
		values["wire-tool"] = append(values["wire-tool"], result.wireTool)
		values["action-ready"] = append(values["action-ready"], result.actionReady)
		observation := pairedBarrierObservation{WorkflowUS: pairedBarrierUS(result.workflow), FirstEventUS: pairedBarrierUS(result.firstEvent), WireToolUS: pairedBarrierUS(result.wireTool), ActionReadyUS: pairedBarrierUS(result.actionReady), Events: result.events, Actions: result.actions}
		if path == "reference" {
			run.ReadyChecks++ // barrierWorkflow returned only after its independent committed-state read.
			values["claim-commit"] = append(values["claim-commit"], result.claim)
			values["dispatch-journal"] = append(values["dispatch-journal"], result.journal)
			values["ready-commit"] = append(values["ready-commit"], result.ready)
			observation.ClaimUS, observation.JournalUS, observation.ReadyUS = pairedBarrierUS(result.claim), pairedBarrierUS(result.journal), pairedBarrierUS(result.ready)
		}
		run.Observations = append(run.Observations, observation)
	}
	if run.NativeEvents != samples*19 || run.Actions != samples*2 || run.ReadyChecks != map[bool]int{true: samples, false: 0}[path == "reference"] {
		t.Fatal("reference workflow coverage changed")
	}
	for phase, observations := range values {
		for _, p := range []int{50, 95, 99} {
			run.Metrics[fmt.Sprintf("%s-p%d-us", phase, p)] = lifecyclePercentile(observations, p)
		}
	}
	return run
}

func TestPairedBarrierReferenceV2(t *testing.T) {
	h := newAccessHarness(t)
	documents := []barrierDocuments{barrierCorpus(t, 0), barrierCorpus(t, 256<<10)}
	provider := newBarrierProvider(t, documents)
	fixture := &strictProviderFixture{Server: provider.Server, profile: "anthropic-messages"}
	owner := h.owner()
	slug, key := publishStrictProvider(t, h, owner, fixture, nil, nil, "strict")
	key = stateKey(t, h, owner, slug, true)
	startPairedBarrierAuthority(t, h, key)
	binding := newBarrierBinding(t, h, fixture.providerID, slug, key)
	relay := newBarrierRelay(t, provider)
	provider.measured.Store(true)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	var version string
	var tls bool
	var databaseName string
	if err := h.Pool.QueryRow(t.Context(), "SHOW server_version_num").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()),false)").Scan(&tls); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatal(err)
	}
	setup, _ := json.Marshal(map[string]any{"postgresql_server_version": version, "postgresql_tls": tls, "database_name": databaseName})
	fmt.Printf("PAIRED_BARRIER_SETUP %s\n", setup)
	// Ordinary integration runs complete checked setup without waiting for an
	// inherited stdin pipe. Only the write-once benchmark runner opts into the
	// long-lived command loop after required service configuration is checked.
	if os.Getenv("OLP_PAIRED_BARRIER_COMMAND_MODE") != "1" {
		return
	}
	fmt.Println("PAIRED_BARRIER_READY reference")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command pairedBarrierCommand
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
			t.Fatal(err)
		}
		run := pairedBarrierReferenceRun(t, command, documents, provider, binding, client, relay.URL, h.HTTP.URL+"/anthropic", key, slug)
		encoded, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("PAIRED_BARRIER_RUN %s\n", encoded)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestPairedBarrierAuthorityFreshnessV2Attempt2(t *testing.T) {
	h := newAccessHarness(t)
	d := barrierCorpus(t, 0)
	provider := newBarrierProvider(t, []barrierDocuments{d})
	fixture := &strictProviderFixture{Server: provider.Server, profile: "anthropic-messages"}
	owner := h.owner()
	slug, _ := publishStrictProvider(t, h, owner, fixture, nil, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	startPairedBarrierAuthority(t, h, key)
	initial := h.Runtime.Authority().ReadAt
	provider.measured.Store(true)
	time.Sleep(olpruntime.AuthorityStaleAfter + time.Second)
	current := h.Runtime.Authority()
	if current.Stale || !current.ReadAt.After(initial) {
		t.Fatal("long-lived paired gateway lost production authority polling")
	}
	if _, err := h.Runtime.Authenticate(key); err != nil {
		t.Fatalf("paired key authority stale after 60 seconds: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	if _, err := barrierWorkflow(t.Context(), barrierBinding{}, provider, client, d, h.HTTP.URL+"/anthropic", key, slug, "gateway"); err != nil {
		t.Fatalf("public native workflow failed after authority refresh: %v", err)
	}
}

func pairedBarrierDefaultOpenStdin(t *testing.T, selectedTest string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run", "^"+selectedTest+"$", "-test.v", "-test.timeout=15s")
	command.Env = make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OLP_PAIRED_BARRIER_COMMAND_MODE=") {
			command.Env = append(command.Env, entry)
		}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close() // Deliberately keep stdin open until the child exits.
	command.Stdin = reader
	output, err := command.CombinedOutput()
	if ctx.Err() != nil || err != nil || !bytes.Contains(output, []byte("PAIRED_BARRIER_SETUP ")) || bytes.Contains(output, []byte("PAIRED_BARRIER_READY ")) {
		t.Fatalf("ordinary integration command loop did not exit with open stdin: timeout=%v err=%v setup=%v ready=%v", ctx.Err(), err, bytes.Contains(output, []byte("PAIRED_BARRIER_SETUP ")), bytes.Contains(output, []byte("PAIRED_BARRIER_READY ")))
	}
}

func TestPairedBarrierReferenceDefaultOpenStdinV2Attempt2(t *testing.T) {
	pairedBarrierDefaultOpenStdin(t, "TestPairedBarrierReferenceV2")
}
