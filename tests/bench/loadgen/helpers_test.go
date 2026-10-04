//go:build bench

package loadgen

import (
	"context"
	"io"
	"math"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/bench/benchtest"
)

// inProcess returns a client that serves requests with h, without a network.
// Inside a synctest bubble the whole exchange runs on the fake clock.
func inProcess(h http.Handler) *http.Client {
	return &http.Client{Transport: benchtest.HandlerTransport{Handler: h}}
}

// base is a configuration that runs against h at 100 requests per second for
// one second, with no warmup.
func base(h http.Handler) Config {
	return Config{URL: "http://gateway", Dialect: OpenAI, APIKey: "key", Model: "model", Rate: 100, Duration: time.Second, Client: inProcess(h)}
}

// serve answers 200 after delay, honouring the client going away.
func serve(delay time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
}

// recording wraps a handler with the arrival time of each request, relative
// to the moment it was created.
type recording struct {
	h        http.Handler
	zero     time.Time
	mu       sync.Mutex
	arrivals []time.Duration
	active   int
	peak     int
}

func record(h http.Handler) *recording { return &recording{h: h, zero: time.Now()} }

func (r *recording) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.arrivals = append(r.arrivals, time.Since(r.zero))
	r.active++
	r.peak = max(r.peak, r.active)
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
	r.h.ServeHTTP(w, req)
}

func (r *recording) sorted() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]time.Duration(nil), r.arrivals...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func run(t testing.TB, cfg Config) *Report {
	t.Helper()
	report, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// near checks a value in milliseconds against a want, allowing for the
// histogram's three significant digits.
func near(t testing.TB, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.005*want+0.002 {
		t.Errorf("%s = %.3f ms, want %.3f ms", what, got, want)
	}
}

// within checks a difference of two histogram values, each of which is
// rounded to its bucket.
func within(t testing.TB, what string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s = %.3f ms, want %.3f ms within %.3f", what, got, want, tolerance)
	}
}

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

// delayed adds a fixed delay in front of a handler, as a gateway would.
func delayed(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		h.ServeHTTP(w, r)
	})
}
