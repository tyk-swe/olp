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

func TestLoadAgainstTheMockOverTheNetwork(t *testing.T) {
	for _, tc := range []struct {
		name string
		h2c  bool
	}{{"http1", false}, {"h2c", true}} {
		for _, d := range dialects {
			t.Run(tc.name+"/"+string(d), func(t *testing.T) {
				origin, server, arrivals := startMock(t, mockupstream.Config{Default: mockupstream.Behavior{TTFT: 5 * time.Millisecond, Interval: time.Millisecond, OutputTokens: 8}})
				cfg := Config{
					URL: origin, Dialect: d, Model: "net-model", APIKey: "k", Rate: 200, Duration: time.Second, Warmup: 200 * time.Millisecond,
					StreamShare: 0.5, PromptTokens: []int{0, 2000}, H2C: tc.h2c,
					// A busy machine may run the scheduler late; lateness is not what is tested.
					LateAfter: time.Second,
				}
				report, err := Run(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				c := report.Requests
				if c.Scheduled != 200 || c.Sent != 200 || c.Succeeded != 200 || c.Failed != 0 || c.Dropped != 0 || c.WarmupSent != 40 || c.WarmupFailed != 0 {
					t.Fatalf("%+v\n%s", c, report.Text())
				}
				if c.Streams != 100 || c.Unary != 100 || report.Errors.StatusCodes["200"] != 200 || report.Latency.TTFT.Count != 100 {
					t.Fatalf("%+v %+v", c, report.Errors)
				}
				// The mock saw every request, warmup included, and the bodies it was sent.
				st := server.Stats()
				if st.Requests != 240 || st.InjectedErrors != 0 || st.RequestBytes < c.RequestBytes {
					t.Fatalf("mock %+v, loadgen %+v", st, c)
				}
				if report.Latency.TTFT.P50Ms < 5 || report.Latency.Stream.P50Ms < 12 || report.Latency.Unary.P50Ms < 12 {
					t.Fatalf("a real server cannot answer faster than it was told to: %+v", report.Latency)
				}

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
				at := arrivals()
				if len(at) != 240 {
					t.Fatalf("%d arrivals", len(at))
				}
				if worst, median, ok := keptTheSchedule(at, cfg.offset); !ok {
					t.Errorf("the schedule was kept to within %v at worst and %v at the median", worst, median)
				}
				if r := report.Rates; r.OfferedRPS < 160 || r.OfferedRPS > 220 {
					t.Errorf("offered %.1f requests per second against 200", r.OfferedRPS)
				}
			})
		}
	}
}

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

func TestLargePromptsReachTheUpstreamWhole(t *testing.T) {
	origin, server, _ := startMock(t, mockupstream.Config{Default: mockupstream.DefaultBehavior()})
	cfg := Config{
		URL: origin, Model: "m", Rate: 30, Duration: time.Second, PromptTokens: []int{50_000, 75_000, 100_000},
		MaxTokens: 16, LateAfter: time.Second, Timeout: 20 * time.Second,
	}
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Requests.Succeeded != 30 || report.Requests.Failed != 0 {
		t.Fatalf("%+v", report.Requests)
	}
	// Ten of each size: about 5.2 bytes per word, so 2.25 million words and 11 MiB.
	if got, want := server.Stats().RequestBytes, report.Requests.RequestBytes; got != want {
		t.Fatalf("the upstream received %d bytes of the %d sent", got, want)
	}
	if mb := report.Requests.RequestBytes >> 20; mb < 10 || mb > 20 {
		t.Fatalf("%d MiB sent for ten prompts of each size", mb)
	}
}

func TestAnUnreachableTargetFailsEveryRequestVisibly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	report, err := Run(context.Background(), Config{URL: "http://" + addr, Model: "m", Rate: 20, Duration: 500 * time.Millisecond, LateAfter: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if report.Requests.Failed != 10 || report.Errors.ByKind["transport"] != 10 || report.Requests.Succeeded != 0 || report.Rates.SuccessRate != 0 {
		t.Fatalf("%+v %+v", report.Requests, report.Errors)
	}
}

// TestSlowReadersBackUpTheServer proves the point of the slow reader on real
// sockets, for both protocols: while the clients crawl, the mock's handlers
// stay blocked in their writes, so the server sees streams held open by the
// reader's pace rather than finished into a buffer. Each response is 3 MB,
// larger than any socket buffer, and the streams arrive and then must still be
// in flight after the time a fast reader would need to take them whole.
func TestSlowReadersBackUpTheServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		h2c  bool
	}{{"http1", false}, {"h2c", true}} {
		t.Run(tc.name, func(t *testing.T) {
			origin, server, _ := startMock(t, mockupstream.Config{Default: mockupstream.Behavior{OutputTokens: 20_000}})
			done := make(chan *Report, 1)
			go func() {
				report, err := Run(context.Background(), Config{
					URL: origin, Model: "m", Rate: 10, Duration: 500 * time.Millisecond, StreamShare: 1, H2C: tc.h2c,
					SlowRead: SlowRead{BytesPerSecond: 2000}, Timeout: 2 * time.Second, LateAfter: time.Minute,
				})
				if err != nil {
					t.Error(err)
				}
				done <- report
			}()
			deadline := time.Now().Add(10 * time.Second)
			for server.Stats().Requests < 5 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(400 * time.Millisecond)
			if st := server.Stats(); st.Requests != 5 || st.InFlight != 5 || st.ClientGone != 0 {
				t.Errorf("the streams finished into a buffer instead of waiting on their readers: %+v", st)
			}
			report := <-done
			// None could finish: 3 MB at 2 KB per second outlasts the timeout.
			if report == nil || report.Errors.ByKind["timeout"] != 5 {
				t.Errorf("%+v", report)
			}
		})
	}
}
