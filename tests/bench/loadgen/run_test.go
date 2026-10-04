//go:build bench

package loadgen

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// These tests run on the fake clock of a synctest bubble: a request that the
// server holds for three seconds takes no real time, and every instant is
// exact, so a test can assert the schedule to the nanosecond.

func TestScheduleIsExactAndWarmupIsExcluded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := record(serve(0))
		cfg := base(rec)
		cfg.Warmup = 500 * time.Millisecond
		report := run(t, cfg)

		arrivals := rec.sorted()
		if len(arrivals) != 150 {
			t.Fatalf("%d requests reached the server, want 50 of warmup and 100 measured", len(arrivals))
		}
		for k, at := range arrivals {
			if want := cfg.offset(int64(k)); at != want {
				t.Fatalf("request %d arrived at %v, want exactly %v", k, at, want)
			}
		}
		c := report.Requests
		if c.WarmupSent != 50 || c.Scheduled != 100 || c.Sent != 100 || c.Succeeded != 100 || c.Failed != 0 || c.Dropped != 0 {
			t.Fatalf("counts %+v", c)
		}
		if report.Latency.All.Count != 100 || report.Latency.SendLag.Count != 100 {
			t.Fatalf("the warmup leaked into the statistics: %+v", report.Latency)
		}
		if r := report.Rates; r.OfferedRPS != 100 || r.Ratio != 1 || r.ThroughputRPS != 100 || r.SuccessRate != 1 || r.ErrorRate != 0 {
			t.Fatalf("rates %+v", r)
		}
		if !report.Valid {
			t.Fatalf("problems %v", report.Problems)
		}
	})
}

func TestScheduleDoesNotDriftAtAwkwardRates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := record(serve(0))
		cfg := base(rec)
		cfg.Rate, cfg.Duration = 7, 30*time.Second
		report := run(t, cfg)
		arrivals := rec.sorted()
		if len(arrivals) != 210 || report.Requests.Sent != 210 {
			t.Fatalf("%d arrivals, %d sent, want 210 of 7 per second for 30 seconds", len(arrivals), report.Requests.Sent)
		}
		for k, at := range arrivals {
			// Computed from the index, so the error never exceeds a nanosecond.
			ideal := time.Duration(float64(k) * 1e9 / 7)
			if diff := at - ideal; diff < -time.Nanosecond || diff > time.Nanosecond {
				t.Fatalf("request %d at %v, ideal %v", k, at, ideal)
			}
		}
		if last := arrivals[len(arrivals)-1]; last < 29*time.Second || last > 30*time.Second {
			t.Fatalf("the last request left at %v", last)
		}
	})
}

func TestLatencyIsMeasuredFromTheScheduledTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		report := run(t, base(serve(30*time.Millisecond)))
		l := report.Latency
		near(t, "p50", l.All.P50Ms, 30)
		near(t, "p99", l.All.P99Ms, 30)
		near(t, "max", l.All.MaxMs, 30)
		near(t, "unary p95", l.Unary.P95Ms, 30)
		if l.SendLag.MaxMs > 0.01 || report.Requests.Late != 0 {
			t.Fatalf("send lag %+v, late %d", l.SendLag, report.Requests.Late)
		}
		if l.Stream.Count != 0 || l.TTFT.Count != 0 {
			t.Fatalf("unary requests recorded stream statistics: %+v", l)
		}
	})
}

// TestALateSchedulerShowsInTheLatencyAndTheSendLag is the generator's own share
// of coordinated omission. Its loop is held up for 30 ms once request 3 is due,
// so that request and the two after it that fell due meanwhile are dispatched
// late, by 30, 20 and 10 ms, and a request that is dispatched late keeps its
// scheduled time: the lateness is in its latency, in the send lag and in the
// count of late sends, though the server answered at once.
func TestALateSchedulerShowsInTheLatencyAndTheSendLag(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, err := newRunner(base(serve(0)))
		if err != nil {
			t.Fatal(err)
		}
		r.beforeDispatch = func(k int64) {
			if k == 3 {
				time.Sleep(30 * time.Millisecond)
			}
		}
		report := r.run(context.Background())
		l := report.Latency
		if report.Requests.Sent != 100 || report.Requests.Succeeded != 100 {
			t.Fatalf("%+v", report.Requests)
		}
		near(t, "send lag max", l.SendLag.MaxMs, 30)
		near(t, "latency max", l.All.MaxMs, 30)
		// Nothing but the generator was slow: from its send each request took
		// no time.
		if l.Service.MaxMs > 0.01 || l.All.P50Ms > 0.01 {
			t.Fatalf("service %+v, all %+v", l.Service, l.All)
		}
		if report.Requests.Late != 3 {
			t.Fatalf("%d sends counted late, want the three held up", report.Requests.Late)
		}
		if report.Valid || !strings.Contains(strings.Join(report.Problems, "\n"), "3 of 100 requests were sent more than 5ms late") {
			t.Fatalf("a generator that fell behind was valid: %v", report.Problems)
		}
	})
}

// TestAStalledServerShowsInThePercentiles is the coordinated omission test.
// The server answers nothing for 300 ms. The requests scheduled during that
// time were due, and a client that held them back until the server recovered
// would see none of the wait; an open-loop client measures it, so the slowest
// request waited the whole stall and the 99th percentile is nearly as bad.
func TestAStalledServerShowsInThePercentiles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		zero := time.Now()
		stalled := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			time.Sleep(time.Until(zero.Add(300 * time.Millisecond)))
			io.WriteString(w, `{}`)
		})
		report := run(t, base(stalled))
		l := report.Latency
		if l.All.Count != 100 || report.Requests.Failed != 0 {
			t.Fatalf("%+v", report.Requests)
		}
		// Request 0 was due at 0 and answered at 300 ms; request 1 at 10 ms.
		near(t, "max", l.All.MaxMs, 300)
		near(t, "p99", l.All.P99Ms, 290)
		// Thirty of the hundred waited, so the 75th percentile is clear of it.
		if l.All.P90Ms < 200 || l.All.P50Ms > 1 {
			t.Fatalf("p50 %.2f p90 %.2f", l.All.P50Ms, l.All.P90Ms)
		}
		// Nothing delayed the sends: the wait is the server's, and it is all there.
		if l.SendLag.MaxMs > 0.01 || !report.Valid {
			t.Fatalf("send lag %+v, problems %v", l.SendLag, report.Problems)
		}
	})
}

// TestBoundedConcurrencyDoesNotHideQueueing is the other half: when the
// generator itself cannot send because every slot is busy, a request waits
// and its latency still starts at its scheduled time. One slot and a server
// that takes 100 ms can serve 10 requests a second; at 20 a second the 20
// requests of the run queue up, and the last answer arrives a second late. A
// closed-loop measurement of the same run reports 100 ms for every request.
func TestBoundedConcurrencyDoesNotHideQueueing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(serve(100 * time.Millisecond))
		cfg.Rate, cfg.MaxInFlight, cfg.MaxBacklog = 20, 1, 100
		report := run(t, cfg)
		l := report.Latency
		if l.All.Count != 20 || report.Requests.Dropped != 0 {
			t.Fatalf("%+v", report.Requests)
		}
		// Request k is due at 50k ms, sent at 100k ms and done at 100k+100 ms.
		near(t, "service p99", l.Service.P99Ms, 100)
		near(t, "service max", l.Service.MaxMs, 100)
		near(t, "p50", l.All.P50Ms, 550)
		near(t, "max", l.All.MaxMs, 1050)
		if l.All.P99Ms < 5*l.Service.P99Ms {
			t.Fatalf("p99 %.1f ms against %.1f ms from the send: the queueing is hidden", l.All.P99Ms, l.Service.P99Ms)
		}
		near(t, "max send lag", l.SendLag.MaxMs, 950)
		if report.Requests.Late != 19 || report.Requests.PeakInFlight != 1 || report.Requests.PeakBacklog < 5 {
			t.Fatalf("%+v", report.Requests)
		}
		// The run delivered its answers but not its load on time, and says so.
		if report.Valid || !strings.Contains(strings.Join(report.Problems, ";"), "late") {
			t.Fatalf("valid %v, problems %v", report.Valid, report.Problems)
		}
	})
}

// TestThroughputIsWhatTheServerFinishedInTheWindow is the other way a slow
// server could hide: its answers all arrive, only late, and counting them
// would report the offered rate as sustained. One slot and a server that takes
// 90 ms finish 11 requests in the second the schedule lasts, though all 20
// are answered in the end.
func TestThroughputIsWhatTheServerFinishedInTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(serve(90 * time.Millisecond))
		cfg.Rate, cfg.MaxInFlight, cfg.MaxBacklog = 20, 1, 100
		report := run(t, cfg)
		c, r := report.Requests, report.Rates
		if c.Succeeded != 20 || c.SucceededInWindow != 11 {
			t.Fatalf("%+v", c)
		}
		if r.ThroughputRPS != 11 || r.SuccessRate != 1 {
			t.Fatalf("throughput %.1f/s against %.1f/s offered: %+v", r.ThroughputRPS, r.OfferedRPS, r)
		}
	})
}

// TestThroughputCountsTheWarmupsTailInTheWindow: a request sent late in the
// warmup that finishes in the period is part of what the period sustained, as
// the period's own last requests finish after it. At a steady rate the
// two balance and the throughput is the rate.
func TestThroughputCountsTheWarmupsTailInTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(mock(t))
		cfg.Rate, cfg.StreamShare, cfg.Warmup = 20, 1, 500*time.Millisecond
		report := run(t, cfg)
		// A stream takes 90 ms, so the last warmup request, due at 450 ms,
		// finishes at 540 ms and the last of the period, due at 1,450 ms,
		// finishes after it.
		if c, r := report.Requests, report.Rates; c.Succeeded != 20 || c.SucceededInWindow != 20 || r.ThroughputRPS != 20 {
			t.Fatalf("%+v %+v", c, r)
		}
	})
}

func TestRequestsBeyondTheBoundsAreDroppedAndCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(serve(95 * time.Millisecond))
		cfg.MaxInFlight, cfg.MaxBacklog = 2, -1
		report := run(t, cfg)
		c := report.Requests
		// In every 100 ms two requests find a free slot and eight do not.
		if c.Scheduled != 100 || c.Sent != 20 || c.Dropped != 80 || c.Succeeded != 20 {
			t.Fatalf("%+v", c)
		}
		if c.Sent+c.Dropped != c.Scheduled || c.PeakInFlight != 2 || c.PeakBacklog != 0 {
			t.Fatalf("%+v", c)
		}
		// Dropped requests are a failure of the run, not an invisible one.
		if report.Valid || !strings.Contains(strings.Join(report.Problems, ";"), "80 requests were dropped") {
			t.Fatalf("valid %v, problems %v", report.Valid, report.Problems)
		}
		if r := report.Rates; r.SuccessRate != 0.2 || r.Ratio > 0.25 {
			t.Fatalf("rates %+v", r)
		}
		// The ones sent were on time, and their latency is the server's.
		near(t, "p99", report.Latency.All.P99Ms, 95)
	})
}

func TestTheBacklogIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(serve(95 * time.Millisecond))
		cfg.MaxInFlight, cfg.MaxBacklog = 1, 3
		report := run(t, cfg)
		c := report.Requests
		if c.Sent+c.Dropped+c.Abandoned != c.Scheduled || c.Dropped == 0 || c.Abandoned != 0 {
			t.Fatalf("%+v", c)
		}
		if c.PeakBacklog != 3 || c.PeakInFlight != 1 {
			t.Fatalf("peaks %+v", c)
		}
		// A request that waited behind three others took about four service times.
		if max := report.Latency.All.MaxMs; max < 250 || max > 450 {
			t.Fatalf("max latency %.1f ms, want the wait for a bounded backlog", max)
		}
		if report.Valid {
			t.Fatal("a run that dropped requests is not valid")
		}
	})
}

func TestWarmupRequestsDoNotCountEvenWhenSlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		zero := time.Now()
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			if time.Since(zero) < time.Second {
				time.Sleep(500 * time.Millisecond)
			}
			io.WriteString(w, `{}`)
		})
		cfg := base(h)
		cfg.Rate, cfg.Warmup = 50, time.Second
		report := run(t, cfg)
		if report.Requests.WarmupSent != 50 || report.Latency.All.Count != 50 || report.Latency.All.MaxMs > 1 {
			t.Fatalf("%+v %+v", report.Requests, report.Latency.All)
		}
	})
}

func TestConcurrencyNeverExceedsTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := record(serve(time.Second))
		cfg := base(rec)
		cfg.Rate, cfg.MaxInFlight, cfg.MaxBacklog = 50, 5, 1000
		cfg.Timeout = time.Minute
		report := run(t, cfg)
		if rec.peak != 5 || report.Requests.PeakInFlight != 5 || report.Requests.Succeeded != 50 {
			t.Fatalf("server saw %d at once; %+v", rec.peak, report.Requests)
		}
	})
}

func TestTimeoutsAreFailuresWithALatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(serve(10 * time.Second))
		cfg.Rate, cfg.Timeout = 10, 200*time.Millisecond
		report := run(t, cfg)
		if report.Requests.Failed != 10 || report.Errors.ByKind["timeout"] != 10 || report.Requests.Succeeded != 0 {
			t.Fatalf("%+v %+v", report.Requests, report.Errors)
		}
		// A stall that ends in timeouts must not make the latency vanish.
		near(t, "failed p50", report.Latency.Failed.P50Ms, 200)
		if report.Latency.All.Count != 0 || report.Rates.ErrorRate != 1 || report.Rates.SuccessRate != 0 {
			t.Fatalf("%+v %+v", report.Latency.All, report.Rates)
		}
	})
}

func TestRequestsStillRunningAfterTheDrainAreCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(serve(time.Hour))
		cfg.Rate, cfg.Timeout, cfg.Drain = 10, 2*time.Hour, 500*time.Millisecond
		report := run(t, cfg)
		if report.Requests.Sent != 10 || report.Errors.ByKind["canceled"] != 10 {
			t.Fatalf("%+v %+v", report.Requests, report.Errors)
		}
		// 900 ms was the last scheduled send; the drain began there.
		if report.ElapsedSeconds < 1.3 || report.ElapsedSeconds > 1.5 {
			t.Fatalf("elapsed %.2f s", report.ElapsedSeconds)
		}
	})
}

func TestRequestsQueuedAtTheDrainAreAbandonedNotSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := record(serve(time.Hour))
		cfg := base(rec)
		cfg.Rate, cfg.MaxInFlight, cfg.MaxBacklog = 10, 1, 100
		cfg.Timeout, cfg.Drain = 2*time.Hour, time.Second
		report := run(t, cfg)
		c := report.Requests
		if c.Sent != 1 || c.Abandoned != 9 || c.Dropped != 0 || len(rec.sorted()) != 1 {
			t.Fatalf("%+v, %d reached the server", c, len(rec.sorted()))
		}
		if report.Valid {
			t.Fatal("unsent requests make the run invalid")
		}
	})
}

func TestTransportErrorsAreCountedNotHidden(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(nil)
		cfg.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, io.ErrClosedPipe
		})}
		report := run(t, cfg)
		if report.Errors.ByKind["transport"] != 100 || report.Requests.Failed != 100 || report.Requests.Succeeded != 0 {
			t.Fatalf("%+v %+v", report.Requests, report.Errors)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCancelingTheContextStopsTheScheduleAndInvalidatesTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan *Report)
		go func() {
			report, err := Run(ctx, base(serve(0)))
			if err != nil {
				t.Error(err)
			}
			done <- report
		}()
		time.Sleep(450 * time.Millisecond)
		cancel()
		report := <-done
		if c := report.Requests; c.Scheduled < 40 || c.Scheduled > 50 || c.Succeeded != c.Sent {
			t.Fatalf("%+v", c)
		}
		if report.Valid || !strings.Contains(strings.Join(report.Problems, ";"), "interrupted") {
			t.Fatalf("valid %v, problems %v", report.Valid, report.Problems)
		}
	})
}

// TestScheduleAndLatencyHoldWhenTheServerIsFast checks the no-load baseline
// the gateway comparison relies on: with an instant server, latency is the
// recording floor and nothing is late.
func TestScheduleAndLatencyHoldWhenTheServerIsFast(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		report := run(t, base(serve(0)))
		if report.Latency.All.MaxMs > 0.01 || report.Requests.Late != 0 || !report.Valid {
			t.Fatalf("%+v", report)
		}
	})
}

// sent counts calls, for tests that only need to know the server was reached.
type sent struct{ n atomic.Int64 }

func (s *sent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.n.Add(1)
	io.Copy(io.Discard, r.Body)
	io.WriteString(w, `{}`)
}
