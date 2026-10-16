//go:build bench

package loadgen

import (
	"context"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/bench/mockupstream"
)

// These tests use real sockets and real time, so they assert counts and
// generous bounds, never exact timing: the exact schedule is covered on the
// fake clock.

// startMock serves a mock over TCP behind a handler that notes each arrival.
func startMock(t *testing.T, cfg mockupstream.Config) (origin string, server *mockupstream.Server, arrivals func() []time.Time) {
	t.Helper()
	server, err := mockupstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []time.Time
	noting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, time.Now())
		mu.Unlock()
		server.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mockupstream.Serve(ctx, ln, noting) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return "http://" + ln.Addr().String(), server, func() []time.Time { mu.Lock(); defer mu.Unlock(); return append([]time.Time(nil), seen...) }
}

// A busy machine may run the scheduler late; lateness is not what is tested.

// The mock saw every request, warmup included, and the bodies it was sent.

// Rate accuracy: the requests arrived on the schedule. A send is
// never early, so no arrival is before its due time, and the
// schedule's start is no later than any arrival less its offset;
// the earliest of those is the tightest estimate. An arrival's
// lateness against it is how far the machine delayed that send.
// A stall on a busy machine delays some sends and the rest catch
// up, which counting arrivals in slices of the run would take for
// a wrong rate. A generator that sent in a burst, or paced itself
// by its responses, is another matter: the burst's start is
// earlier than the schedule's and a response-paced one falls
// further behind it, so either shows a lateness of seconds.

// keptTheSchedule judges arrivals against a schedule whose request k is due at
// offset(k) after a start it does not know. A send is never early, so the start
// is no later than any arrival less its offset, and the earliest of those is
// the tightest estimate; an arrival's lateness against it is how far the machine
// delayed that send. A stall of a few hundred milliseconds delays the sends due
// during it and no others, so the worst lateness is bounded loosely and the
// median tightly. A burst has a median of 600 ms for 240 requests at 200 a
// second, and a generator paced by its responses falls further behind the
// longer it runs, so either fails both.
func keptTheSchedule(arrivals []time.Time, offset func(int64) time.Duration) (worst, median time.Duration, ok bool) {
	starts := make([]time.Duration, len(arrivals))
	for k, a := range arrivals {
		starts[k] = a.Sub(arrivals[0]) - offset(int64(k))
	}
	earliest := slices.Min(starts)
	lateness := make([]time.Duration, len(starts))
	for k, start := range starts {
		lateness[k] = start - earliest
	}
	slices.Sort(lateness)
	worst, median = lateness[len(lateness)-1], lateness[len(lateness)/2]
	return worst, median, worst <= 700*time.Millisecond && median <= 150*time.Millisecond
}

func TestAStallIsNotABurstOrADriftInTheSchedule(t *testing.T) {
	const n = 240
	offset := func(k int64) time.Duration { return time.Duration(k) * 5 * time.Millisecond }
	zero := time.Unix(1_700_000_000, 0)
	build := func(at func(k int) time.Duration) []time.Time {
		out := make([]time.Time, n)
		for k := range out {
			out[k] = zero.Add(at(k))
		}
		return out
	}
	exact := func(k int) time.Duration { return offset(int64(k)) }
	for _, tc := range []struct {
		name string
		at   func(k int) time.Duration
		ok   bool
	}{
		{"on time", exact, true},
		{"a 270 ms stall, then catching up", func(k int) time.Duration {
			if due := exact(k); due >= 400*time.Millisecond && due < 670*time.Millisecond {
				return 670 * time.Millisecond
			}
			return exact(k)
		}, true},
		{"the first send late and the rest on time", func(k int) time.Duration {
			if k == 0 {
				return 200 * time.Millisecond
			}
			return exact(k)
		}, true},
		{"every request at once", func(int) time.Duration { return 0 }, false},
		{"paced by a 12 ms response", func(k int) time.Duration { return time.Duration(k) * 12 * time.Millisecond }, false},
		{"a second's stall", func(k int) time.Duration {
			if due := exact(k); due >= 400*time.Millisecond && due < 1400*time.Millisecond {
				return 1400 * time.Millisecond
			}
			return exact(k)
		}, false},
	} {
		if worst, median, ok := keptTheSchedule(build(tc.at), offset); ok != tc.ok {
			t.Errorf("%s: kept = %v (worst %v, median %v), want %v", tc.name, ok, worst, median, tc.ok)
		}
	}
}

// Ten of each size: about 5.2 bytes per word, so 2.25 million words and 11 MiB.

// None could finish: 3 MB at 2 KB per second outlasts the timeout.
