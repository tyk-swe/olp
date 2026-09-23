//go:build integration

package integration_test

// Measurement-only loop for the production translated continuation arm. The
// unchanged candidateWorkflow checks the public projected client contract and
// the same provider's frozen native first/next requests as the reference arm.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pairedBarrierCandidateRun(t *testing.T, command pairedBarrierCommand, documents []barrierDocuments, provider *barrierProvider, h *accessHarness, client *http.Client, key, slug string) pairedBarrierRun {
	t.Helper()
	parts := strings.Split(command.Name, "/")
	if len(parts) != 3 || (parts[0] != "small" && parts[0] != "large") || (parts[1] != "c1" && parts[1] != "c8") || parts[2] != "translated" || command.Block < 0 || command.Block >= 32 || command.Position < 0 || command.Position >= 8 {
		t.Fatalf("invalid translated command: %+v", command)
	}
	d, concurrency := documents[0], 1
	if parts[0] == "large" {
		d = documents[1]
	}
	if parts[1] == "c8" {
		concurrency = 8
	}
	// The same checked warmup rule as the reference arm; it is not timed.
	warmup, err := candidateWorkflow(t.Context(), d, client, h.HTTP.URL, key, slug)
	if err != nil {
		t.Fatal(err)
	}
	var stateBytes int
	if err := h.Pool.QueryRow(t.Context(), `SELECT octet_length(s.ciphertext) FROM olp_go.provider_resources r JOIN olp_go.secrets s ON s.id=r.id WHERE r.submission_id=$1 AND r.state='ready'`, warmup.submission).Scan(&stateBytes); err != nil || stateBytes <= d.historyBytes {
		t.Fatalf("missing complete encrypted candidate state: bytes=%d err=%v", stateBytes, err)
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
	results := make([]candidateSample, samples)
	errs := make(chan error, samples)
	var wg sync.WaitGroup
	for worker := range concurrency {
		wg.Go(func() {
			for i := worker; i < samples; i += concurrency {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				result, err := candidateWorkflow(ctx, d, client, h.HTTP.URL, key, slug)
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
		t.Fatalf("translated provider dispatch counts changed: %d/%d", first, next)
	}
	run := pairedBarrierRun{
		Name: command.Name, Block: command.Block, Position: command.Position, Samples: samples,
		Dispatches: first + next, FirstRequests: first, NextRequests: next,
		HistoryBytes: d.historyBytes, StateBytes: stateBytes,
		Metrics:      map[string]float64{"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / samples, "process-cpu-ns/op": float64(cpu) / samples, "B/op": float64(after.TotalAlloc-before.TotalAlloc) / samples, "allocs/op": float64(after.Mallocs-before.Mallocs) / samples, "sampled-heap-growth-B": float64(peak.Load() - before.HeapAlloc)},
		Observations: make([]pairedBarrierObservation, 0, samples),
		GCCount:      after.NumGC - before.NumGC, GCPauseNS: after.PauseTotalNs - before.PauseTotalNs,
		GoroutinesBefore: goroutines, GoroutinesAfter: runtime.NumGoroutine(),
		SchedulerWaits: schedulerWaits, SchedulerP99US: schedulerP99US,
	}
	values := map[string][]time.Duration{"workflow": {}, "first-event": {}, "tool-visible": {}, "action-ready": {}}
	for _, result := range results {
		run.NativeEvents += result.nativeEvents
		run.FirstTurnObservations += result.observations
		run.FinalObservations += result.finalObservations
		run.Actions += result.actions
		run.ReadyChecks += result.readyChecks
		values["workflow"] = append(values["workflow"], result.workflow)
		values["first-event"] = append(values["first-event"], result.firstEvent)
		values["tool-visible"] = append(values["tool-visible"], result.toolVisible)
		values["action-ready"] = append(values["action-ready"], result.actionReady)
		run.Observations = append(run.Observations, pairedBarrierObservation{WorkflowUS: pairedBarrierUS(result.workflow), FirstEventUS: pairedBarrierUS(result.firstEvent), ToolVisibleUS: pairedBarrierUS(result.toolVisible), ActionReadyUS: pairedBarrierUS(result.actionReady), Events: result.nativeEvents, Actions: result.actions})
	}
	if run.NativeEvents != samples*19 || run.FirstTurnObservations != samples*13 || run.FinalObservations != samples*2 || run.Actions != samples*2 || run.ReadyChecks != samples {
		t.Fatal("translated workflow coverage changed")
	}
	for phase, observations := range values {
		for _, p := range []int{50, 95, 99} {
			run.Metrics[fmt.Sprintf("%s-p%d-us", phase, p)] = lifecyclePercentile(observations, p)
		}
	}
	return run
}

func TestPairedBarrierCandidateV2(t *testing.T) {
	h := newAccessHarness(t)
	documents := []barrierDocuments{barrierCorpus(t, 0), barrierCorpus(t, 256<<10)}
	provider := newBarrierProvider(t, documents)
	fixture := &strictProviderFixture{Server: provider.Server, profile: "anthropic-messages"}
	options := map[string]any{"bindings": map[string]any{vendorModel: map[string]any{"model": "fixture-model"}}, "operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}}}
	owner := h.owner()
	slug, _ := publishStrictProvider(t, h, owner, fixture, options, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	startPairedBarrierAuthority(t, h, key)
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
	// Ordinary integration checks setup and exits even if stdin remains open.
	// The paired runner explicitly enables the long-lived command protocol.
	if os.Getenv("OLP_PAIRED_BARRIER_COMMAND_MODE") != "1" {
		return
	}
	fmt.Println("PAIRED_BARRIER_READY translated")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command pairedBarrierCommand
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
			t.Fatal(err)
		}
		run := pairedBarrierCandidateRun(t, command, documents, provider, h, client, key, slug)
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

func TestPairedBarrierCandidateDefaultOpenStdinV2Attempt2(t *testing.T) {
	pairedBarrierDefaultOpenStdin(t, "TestPairedBarrierCandidateV2")
}
