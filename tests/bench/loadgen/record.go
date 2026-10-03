//go:build bench

package loadgen

import (
	"strconv"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// outcome is how a request ended; the empty outcome is success.
type outcome string

const (
	succeeded    outcome = ""
	outStatus    outcome = "http_status"  // the upstream answered with a non-2xx status
	outTimeout   outcome = "timeout"      // the request outlived its timeout
	outTransport outcome = "transport"    // no response: refused, reset, closed
	outTruncated outcome = "truncated"    // a 2xx stream ended without its terminal event
	outStreamErr outcome = "stream_error" // a 2xx stream carried an in-band error
	outBadBody   outcome = "bad_response" // a 2xx answer that was not the one asked for
	outCanceled  outcome = "canceled"     // still running when the drain period ended
)

// result is what one request observed.
type result struct {
	stream  bool
	status  int
	failure outcome
	// end is when the request finished, successfully or not, which for a
	// stream that closed is when its closing event was complete; first is when
	// its first data frame arrived, zero if none did.
	end, first          time.Time
	reqBytes, respBytes int64
}

// maxTrackable is the largest latency a histogram holds, in microseconds.
// A slow-reader stream can last minutes.
const maxTrackable = int64(time.Hour / time.Microsecond)

func newHistogram() *hdrhistogram.Histogram { return hdrhistogram.New(1, maxTrackable, 3) }

// recorder accumulates the measured period's statistics. Histograms are not
// safe for concurrent use, so one mutex guards them all; at a few thousand
// requests a second it is idle.
type recorder struct {
	mu sync.Mutex
	// all holds every successful request; unary and stream split it; ttft is
	// the time to first data frame of streams that began; service is a
	// successful request's latency from its actual send, kept to show what
	// the scheduled-time latency adds; failed holds requests that failed; lag
	// is the delay between a request's scheduled time and its send.
	all, unary, stream, ttft, service, failed, lag *hdrhistogram.Histogram
	clamped                                        int64

	// window is the length of the measured period, which the throughput is
	// counted over.
	window time.Duration

	counts   Counts
	statuses map[int]int64
	kinds    map[outcome]int64
	// lastSend is the latest send of a measured request, relative to the
	// start of the measured period.
	lastSend time.Duration
}

func newRecorder(window time.Duration) *recorder {
	return &recorder{
		window: window,
		all:    newHistogram(), unary: newHistogram(), stream: newHistogram(), ttft: newHistogram(),
		service: newHistogram(), failed: newHistogram(), lag: newHistogram(),
		statuses: map[int]int64{}, kinds: map[outcome]int64{},
	}
}

// micros converts a duration to histogram units, never below the first bucket.
func micros(d time.Duration) int64 { return max(1, d.Microseconds()) }

func (r *recorder) observe(h *hdrhistogram.Histogram, d time.Duration) {
	v := micros(d)
	if v > maxTrackable {
		v = maxTrackable
		r.clamped++
	}
	_ = h.RecordValue(v)
}

// record files a finished request. scheduled and send are the request's
// scheduled and actual send times; late is the lag beyond which a send counts
// as late; measured says whether it belongs to the measured period.
func (r *recorder) record(res result, scheduled, send, measuredStart time.Time, late time.Duration, measured bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// What a system sustains is what it finishes during the period, whichever
	// request it was: in steady state the warmup's tail finishes in it as much
	// as the period's own requests finish after it.
	if res.failure == succeeded && !res.end.Before(measuredStart) && res.end.Before(measuredStart.Add(r.window)) {
		r.counts.SucceededInWindow++
	}
	if !measured {
		r.counts.WarmupSent++
		if res.failure != succeeded {
			r.counts.WarmupFailed++
		}
		return
	}
	lag := max(0, send.Sub(scheduled))
	r.observe(r.lag, lag)
	if lag > late {
		r.counts.Late++
	}
	r.lastSend = max(r.lastSend, send.Sub(measuredStart))
	r.counts.Sent++
	r.counts.RequestBytes += res.reqBytes
	r.counts.ResponseBytes += res.respBytes
	if res.stream {
		r.counts.Streams++
	} else {
		r.counts.Unary++
	}
	if res.status != 0 {
		r.statuses[res.status]++
	}
	latency := res.end.Sub(scheduled)
	if !res.first.IsZero() {
		r.observe(r.ttft, res.first.Sub(scheduled))
	}
	if res.failure != succeeded {
		r.counts.Failed++
		r.kinds[res.failure]++
		r.observe(r.failed, latency)
		return
	}
	r.counts.Succeeded++
	r.observe(r.all, latency)
	r.observe(r.service, res.end.Sub(send))
	if res.stream {
		r.observe(r.stream, latency)
	} else {
		r.observe(r.unary, latency)
	}
}

func (r *recorder) scheduled(measured bool) {
	if measured {
		r.mu.Lock()
		r.counts.Scheduled++
		r.mu.Unlock()
	}
}

func (r *recorder) drop(measured bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if measured {
		r.counts.Dropped++
	}
}

func (r *recorder) abandon(measured bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if measured {
		r.counts.Abandoned++
	}
}

// Summary describes one histogram, in milliseconds.
type Summary struct {
	Count    int64   `json:"count"`
	MinMs    float64 `json:"min_ms"`
	MeanMs   float64 `json:"mean_ms"`
	StdDevMs float64 `json:"stddev_ms"`
	P50Ms    float64 `json:"p50_ms"`
	P90Ms    float64 `json:"p90_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	P999Ms   float64 `json:"p99_9_ms"`
	MaxMs    float64 `json:"max_ms"`
}

func summarize(h *hdrhistogram.Histogram) Summary {
	if h.TotalCount() == 0 {
		return Summary{}
	}
	ms := func(us int64) float64 { return float64(us) / 1000 }
	return Summary{
		Count: h.TotalCount(), MinMs: ms(h.Min()), MeanMs: h.Mean() / 1000, StdDevMs: h.StdDev() / 1000,
		P50Ms: ms(h.ValueAtQuantile(50)), P90Ms: ms(h.ValueAtQuantile(90)), P95Ms: ms(h.ValueAtQuantile(95)),
		P99Ms: ms(h.ValueAtQuantile(99)), P999Ms: ms(h.ValueAtQuantile(99.9)), MaxMs: ms(h.Max()),
	}
}

// statusKeys renders status codes as JSON object keys.
func statusKeys(m map[int]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for code, n := range m {
		out[strconv.Itoa(code)] = n
	}
	return out
}
