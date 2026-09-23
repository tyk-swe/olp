package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	goruntime "runtime"
	runtimemetrics "runtime/metrics"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This is an additive measurement adapter for the frozen fidelity fixture. The
// fixture, request oracle and provider effect checks remain in the v1 file.
// A single test process accepts fixed-size subruns so B and C can be measured
// in interleaved, long-lived processes without compilation in the timed path.
const fidelityPairedPrefix = "OLP_SOURCE_PAIRED_V2 "

type fidelityPairedCommand struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type fidelityPairedRawSample struct {
	LatencyUS float64 `json:"latency_us"`
	FirstUS   float64 `json:"first_event_us"`
	MaxGapUS  float64 `json:"max_inter_event_gap_us"`
	Events    int     `json:"events"`
	TextBytes int     `json:"text_bytes"`
}

type fidelityPairedReply struct {
	ID         string                    `json:"id,omitempty"`
	Name       string                    `json:"name,omitempty"`
	Iterations int                       `json:"iterations,omitempty"`
	Metrics    map[string]float64        `json:"metrics,omitempty"`
	Samples    []fidelityPairedRawSample `json:"samples,omitempty"`
	Runtime    map[string]uint64         `json:"runtime,omitempty"`
	Scheduler  fidelityPairedScheduler   `json:"scheduler"`
	Error      string                    `json:"error,omitempty"`
	Ready      bool                      `json:"ready,omitempty"`
}

type fidelityPairedScheduler struct {
	LatencyBucketSeconds []string `json:"latency_bucket_seconds"`
	LatencyCountsDelta   []uint64 `json:"latency_counts_delta"`
}

func fidelityPairedSchedulerSnapshot() ([]string, []uint64) {
	samples := []runtimemetrics.Sample{{Name: "/sched/latencies:seconds"}}
	runtimemetrics.Read(samples)
	histogram := samples[0].Value.Float64Histogram()
	buckets := make([]string, len(histogram.Buckets))
	for i, boundary := range histogram.Buckets {
		buckets[i] = fmt.Sprintf("%g", boundary)
	}
	return buckets, slices.Clone(histogram.Counts)
}

func fidelityPairedWrite(t *testing.T, reply fidelityPairedReply) {
	t.Helper()
	encoded, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(fidelityPairedPrefix + string(encoded))
}

func fidelityPairedLookup(name string) (fidelityWorkload, int, bool, bool) {
	for _, w := range fidelityWorkloads() {
		for _, concurrency := range []int{1, 8} {
			for _, relay := range []bool{true, false} {
				if relay && w.rejected {
					continue
				}
				mode := "gateway"
				if relay {
					mode = "relay"
				}
				if name == fmt.Sprintf("%s/c%d/%s", w.name, concurrency, mode) {
					return w, concurrency, relay, true
				}
			}
		}
	}
	return fidelityWorkload{}, 0, false, false
}

// TestFidelityPairedServer is inactive in ordinary test runs. The protocol is
// deliberately narrow: a prebuilt binary reads one known workload name per
// line; every request uses the v1 independent fixture and exactly 64 measured
// requests. A failed oracle, count or process is never a timing observation.
func TestFidelityPairedServer(t *testing.T) {
	if os.Getenv("OLP_SOURCE_PAIRED_SERVER") != "1" {
		t.Skip("paired benchmark server is explicitly selected by the v2 runner")
	}
	fidelityPairedWrite(t, fidelityPairedReply{Ready: true})
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var command fidelityPairedCommand
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil || command.ID == "" {
			fidelityPairedWrite(t, fidelityPairedReply{Error: "malformed command"})
			t.Fatal("malformed paired benchmark command")
		}
		w, concurrency, relay, ok := fidelityPairedLookup(command.Name)
		if !ok {
			fidelityPairedWrite(t, fidelityPairedReply{ID: command.ID, Error: "unknown workload"})
			t.Fatalf("unknown paired benchmark workload %q", command.Name)
		}
		reply := fidelityPairedMeasure(w, concurrency, relay)
		reply.ID, reply.Name = command.ID, command.Name
		fidelityPairedWrite(t, reply)
		if reply.Error != "" {
			t.Fatal(reply.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func fidelityPairedMeasure(w fidelityWorkload, concurrency int, relay bool) fidelityPairedReply {
	var reply fidelityPairedReply
	var failed atomic.Bool
	result := testing.Benchmark(func(b *testing.B) {
		f := newFidelityFixture(b, w, relay)
		if _, err := f.request(w, relay); err != nil {
			failed.Store(true)
			b.Fatal(err)
		}
		f.dispatches.Store(0)
		f.completed.Store(0)
		samples := make([]fidelitySample, b.N)
		goruntime.GC()
		var before, after goruntime.MemStats
		goruntime.ReadMemStats(&before)
		schedulerBuckets, schedulerBefore := fidelityPairedSchedulerSnapshot()
		initialHeap := before.HeapAlloc
		var peak atomic.Uint64
		peak.Store(initialHeap)
		stop, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					var m goruntime.MemStats
					goruntime.ReadMemStats(&m)
					peak.Store(max(peak.Load(), m.HeapAlloc))
				}
			}
		}()
		var next atomic.Int64
		var workers sync.WaitGroup
		b.ReportAllocs()
		b.SetBytes(int64(len(f.body)))
		b.ResetTimer()
		cpuStart := fidelityCPU()
		for range concurrency {
			workers.Go(func() {
				for {
					i := int(next.Add(1) - 1)
					if i >= b.N || failed.Load() {
						return
					}
					var err error
					samples[i], err = f.request(w, relay)
					if err != nil {
						failed.Store(true)
						b.Error(err)
						return
					}
				}
			})
		}
		workers.Wait()
		cpu := fidelityCPU() - cpuStart
		b.StopTimer()
		close(stop)
		<-stopped
		goruntime.ReadMemStats(&after)
		_, schedulerAfter := fidelityPairedSchedulerSnapshot()
		peak.Store(max(peak.Load(), after.HeapAlloc))
		wantDispatches := int64(b.N)
		if w.rejected {
			wantDispatches = 0
		}
		if f.dispatches.Load() != wantDispatches || f.completed.Load() != wantDispatches {
			failed.Store(true)
			b.Errorf("provider effects: dispatched=%d completed=%d want=%d", f.dispatches.Load(), f.completed.Load(), wantDispatches)
		}
		if b.N != 64 || failed.Load() {
			return // testing.Benchmark first executes a discarded one-request calibration.
		}
		metrics := map[string]float64{
			"dispatches": float64(wantDispatches), "succeeded": float64(wantDispatches), "rejected": float64(b.N) - float64(wantDispatches),
			"request-bytes": float64(len(f.body)), "content-events/op": float64(w.events),
			"sampled-heap-growth-B": float64(peak.Load() - initialHeap), "process-cpu-ns/op": float64(cpu.Nanoseconds()) / float64(b.N),
		}
		report := func(name string, pick func(fidelitySample) time.Duration) {
			values := make([]time.Duration, len(samples))
			for i, sample := range samples {
				values[i] = pick(sample)
			}
			slices.Sort(values)
			for _, p := range []int{50, 95, 99} {
				index := max(0, int(math.Ceil(float64(p)*float64(len(values))/100))-1)
				metrics[fmt.Sprintf("%s-p%d-us", name, p)] = float64(values[index].Nanoseconds()) / 1000
			}
		}
		report("latency", func(s fidelitySample) time.Duration { return s.elapsed })
		if w.stream {
			report("first-event", func(s fidelitySample) time.Duration { return s.first })
			report("max-inter-event-gap", func(s fidelitySample) time.Duration { return s.maximumGap })
		}
		reply.Metrics = metrics
		reply.Iterations = b.N
		reply.Samples = make([]fidelityPairedRawSample, len(samples))
		for i, sample := range samples {
			reply.Samples[i] = fidelityPairedRawSample{
				LatencyUS: float64(sample.elapsed.Nanoseconds()) / 1000,
				FirstUS:   float64(sample.first.Nanoseconds()) / 1000,
				MaxGapUS:  float64(sample.maximumGap.Nanoseconds()) / 1000,
				Events:    sample.events, TextBytes: sample.textBytes,
			}
		}
		reply.Runtime = map[string]uint64{
			"num_gc_delta":            uint64(after.NumGC - before.NumGC),
			"pause_total_ns_delta":    after.PauseTotalNs - before.PauseTotalNs,
			"total_alloc_bytes_delta": after.TotalAlloc - before.TotalAlloc,
			"heap_alloc_after_bytes":  after.HeapAlloc,
			"goroutines_after":        uint64(goruntime.NumGoroutine()),
		}
		reply.Scheduler.LatencyBucketSeconds = schedulerBuckets
		reply.Scheduler.LatencyCountsDelta = make([]uint64, len(schedulerAfter))
		for i := range schedulerAfter {
			reply.Scheduler.LatencyCountsDelta[i] = schedulerAfter[i] - schedulerBefore[i]
		}
	})
	if failed.Load() || reply.Iterations != 64 || len(reply.Samples) != 64 || result.N != 64 {
		reply.Error = "oracle, effect count or fixed sample count failed"
		return reply
	}
	reply.Metrics["ns/op"] = float64(result.NsPerOp())
	reply.Metrics["B/op"] = float64(result.AllocedBytesPerOp())
	reply.Metrics["allocs/op"] = float64(result.AllocsPerOp())
	return reply
}

func TestFidelityPairedLookupPreservesFrozenInventory(t *testing.T) {
	seen := make(map[string]bool)
	for _, w := range fidelityWorkloads() {
		for _, concurrency := range []int{1, 8} {
			for _, mode := range []string{"relay", "gateway"} {
				name := fmt.Sprintf("%s/c%d/%s", w.name, concurrency, mode)
				_, _, _, ok := fidelityPairedLookup(name)
				if ok != (mode != "relay" || !w.rejected) {
					t.Fatalf("inventory changed at %s", name)
				}
				if ok {
					seen[name] = true
				}
			}
		}
	}
	if len(seen) != 22 {
		t.Fatalf("expected 22 exact v1 workloads, found %d", len(seen))
	}
	if _, _, _, ok := fidelityPairedLookup(strings.Replace("native_unary/c1/relay", "c1", "c4", 1)); ok {
		t.Fatal("unregistered concurrency accepted")
	}
}
