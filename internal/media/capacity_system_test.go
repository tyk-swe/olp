//go:build integration

package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type generatedMedia struct{}

func (generatedMedia) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Exercise full default-size uploads without constructing payload-sized test
// buffers. Hold all artifacts concurrently, read slowly, then mass-disconnect
// another upload wave and require every reservation and file to be released.
func TestMediaCapacityWithSlowReadersAndMassDisconnects(t *testing.T) {
	const size int64 = 64 << 20
	const concurrency = 8
	spool := testSpool(t, DefaultCapacityBytes)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	artifacts := make(chan *Artifact, concurrency)
	errorsCh := make(chan error, concurrency)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			a, err := spool.Put(t.Context(), Upload{Filename: "bounded.bin", MaximumLength: size, Body: io.NopCloser(io.LimitReader(generatedMedia{}, size))})
			if err != nil {
				errorsCh <- err
				return
			}
			artifacts <- a
		})
	}
	wg.Wait()
	close(artifacts)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	peakReserved := spool.UsedBytes()
	if peakReserved != concurrency*size {
		t.Fatalf("reserved %d, want %d", peakReserved, concurrency*size)
	}
	runtime.ReadMemStats(&after)
	status, _ := os.ReadFile("/proc/self/status")
	var rss string
	for line := range strings.SplitSeq(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			rss = strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:"))
		}
	}
	fds, _ := os.ReadDir("/proc/self/fd")
	for a := range artifacts {
		wg.Go(func() {
			r, err := spool.Open(a.Handle)
			if err != nil {
				t.Error(err)
				return
			}
			buffer := make([]byte, 64<<10)
			for {
				_, err := r.File.Read(buffer)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					break
				}
				time.Sleep(time.Microsecond * 100)
			}
			r.File.Close()
			if err := spool.Remove(a.Handle); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for range concurrency {
		wg.Go(func() {
			r, w := io.Pipe()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := spool.Put(ctx, Upload{Filename: "aborted.bin", MaximumLength: size, Body: r})
				done <- err
			}()
			_, _ = w.Write(make([]byte, 32<<10))
			cancel()
			_ = w.CloseWithError(errors.New("client disconnected"))
			if err := <-done; err == nil {
				t.Error("aborted upload succeeded")
			}
		})
	}
	wg.Wait()
	if got := spool.UsedBytes(); got != 0 {
		t.Fatalf("reservation leaked: %d", got)
	}
	measurement, _ := json.Marshal(map[string]any{"upload_bytes": size, "concurrency": concurrency, "capacity_bytes": spool.CapacityBytes(), "held_bytes": peakReserved, "final_reserved_bytes": spool.UsedBytes(), "heap_before": before.HeapAlloc, "heap_held": after.HeapAlloc, "resident_held": rss, "fds_held": len(fds), "elapsed_seconds": time.Since(start).Seconds()})
	t.Logf("media_capacity=%s", measurement)
}
