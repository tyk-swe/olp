//go:build bench

package loadgen

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Counts are the request tallies of the measured period. Warmup requests
// are counted apart, since they reach the system under test too.
type Counts struct {
	// Scheduled is how many requests the measured period called for.
	Scheduled int64 `json:"scheduled"`
	Sent      int64 `json:"sent"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	// SucceededInWindow is the successful requests, warmup included, that
	// finished during the measured period. It is what the throughput is
	// counted from: requests that finish after the period, as those of a
	// system that has fallen behind do, are in Succeeded and not in it.
	SucceededInWindow int64 `json:"succeeded_in_window"`
	// Dropped requests found neither a free slot nor room to wait and were
	// never sent. Abandoned ones waited and were still unsent when the drain
	// period ended.
	Dropped   int64 `json:"dropped"`
	Abandoned int64 `json:"abandoned"`
	// Late requests were sent more than the late threshold after their
	// scheduled time.
	Late    int64 `json:"late"`
	Unary   int64 `json:"unary"`
	Streams int64 `json:"streams"`
	// WarmupSent counts the warmup requests sent, excluded from all else.
	WarmupSent   int64 `json:"warmup_sent"`
	WarmupFailed int64 `json:"warmup_failed"`
	// PeakInFlight and PeakBacklog are the most requests running at once and
	// waiting for a slot, over the whole run including warmup.
	PeakInFlight  int   `json:"peak_in_flight"`
	PeakBacklog   int   `json:"peak_backlog"`
	RequestBytes  int64 `json:"request_bytes"`
	ResponseBytes int64 `json:"response_bytes"`
}

// Rates compare what was asked with what was delivered.
type Rates struct {
	TargetRPS float64 `json:"target_rps"`
	// OfferedRPS is the rate the requests were actually sent at: measured
	// requests sent, over the time from the start of the period to the last
	// send plus one interval.
	OfferedRPS float64 `json:"offered_rps"`
	// ThroughputRPS is the successful requests that finished during the
	// measured period, per second of it: what the system under test sustained.
	// A system that falls behind finishes fewer in the period than were sent
	// to it, so unlike Succeeded over the duration this does not read as the
	// offered rate however late the answers come. It assumes a warmup at least
	// as long as a request, or the period's first answers are still missing.
	ThroughputRPS float64 `json:"throughput_rps"`
	// Ratio is OfferedRPS over TargetRPS.
	Ratio float64 `json:"ratio"`
	// SuccessRate is successes over scheduled requests, so a drop counts
	// against it; ErrorRate is failures over requests sent.
	SuccessRate float64 `json:"success_rate"`
	ErrorRate   float64 `json:"error_rate"`
}

// Errors break failures down.
type Errors struct {
	// ByKind counts failed requests by how they failed: http_status, timeout,
	// transport, truncated, stream_error, bad_response or canceled.
	ByKind map[string]int64 `json:"by_kind"`
	// StatusCodes counts every response status seen in the measured period.
	StatusCodes map[string]int64 `json:"status_codes"`
}

// Latencies are the run's histograms. All of them are in milliseconds and,
// except SendLag and Service, measured from each request's scheduled time.
type Latencies struct {
	// All, Unary and Stream cover successful requests; a stream's latency is
	// to the end of its closing event, as a client has its answer then, and
	// not to the end of the response.
	All    Summary `json:"all"`
	Unary  Summary `json:"unary"`
	Stream Summary `json:"stream"`
	// TTFT is the time to the first data frame, of streams that began.
	TTFT Summary `json:"ttft"`
	// Service is the latency of successful requests from their actual send,
	// which is what a closed-loop generator would report. The gap between it
	// and All is the queueing the schedule exposes.
	Service Summary `json:"service"`
	// Failed covers failed requests, so a timeout is visible rather than
	// absent.
	Failed Summary `json:"failed"`
	// SendLag is how late requests left, which measures the generator.
	SendLag Summary `json:"send_lag"`
}

// ConfigSummary records what was run, without the credential.
type ConfigSummary struct {
	URL            string  `json:"url"`
	Dialect        Dialect `json:"dialect"`
	Model          string  `json:"model"`
	Rate           float64 `json:"rate"`
	DurationSec    float64 `json:"duration_seconds"`
	WarmupSec      float64 `json:"warmup_seconds"`
	StreamShare    float64 `json:"stream_share"`
	PromptTokens   []int   `json:"prompt_tokens"`
	MaxTokens      int     `json:"max_tokens"`
	MaxInFlight    int     `json:"max_in_flight"`
	MaxBacklog     int     `json:"max_backlog"`
	TimeoutSec     float64 `json:"timeout_seconds"`
	SlowReadBPS    int     `json:"slow_read_bytes_per_second"`
	SlowReadRcvBuf int     `json:"slow_read_rcvbuf"`
	H2C            bool    `json:"h2c"`
}

// Report is the result of a run, and what the JSON output holds.
type Report struct {
	// Name labels the report; the caller sets it.
	Name      string        `json:"name,omitempty"`
	Config    ConfigSummary `json:"config"`
	StartedAt time.Time     `json:"started_at"`
	// ElapsedSeconds runs from the start of warmup to the last completion.
	ElapsedSeconds float64   `json:"elapsed_seconds"`
	Rates          Rates     `json:"rates"`
	Requests       Counts    `json:"requests"`
	Errors         Errors    `json:"errors"`
	Latency        Latencies `json:"latency"`
	// ClampedLatencies counts values past the histogram's range, recorded as
	// its maximum.
	ClampedLatencies int64 `json:"clamped_latencies,omitempty"`
	// Valid is false when the run could not deliver the load it was asked to:
	// it dropped or abandoned requests, sent a noticeable share late, offered
	// less than 99% of the target rate, or was interrupted. Latencies from an
	// invalid run describe the generator as much as the system.
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems,omitempty"`
}

// report assembles the report of a finished run.
func (r *recorder) report(cfg Config, started time.Time, elapsed time.Duration, interrupted bool, peakInFlight, peakBacklog int) *Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.counts
	c.PeakInFlight, c.PeakBacklog = peakInFlight, peakBacklog
	rep := &Report{
		Config: ConfigSummary{
			URL: redact(cfg.URL), Dialect: cfg.Dialect, Model: cfg.Model, Rate: cfg.Rate, DurationSec: cfg.Duration.Seconds(), WarmupSec: cfg.Warmup.Seconds(),
			StreamShare: cfg.StreamShare, PromptTokens: slices.Clone(cfg.PromptTokens), MaxTokens: cfg.MaxTokens, MaxInFlight: cfg.MaxInFlight,
			MaxBacklog: cfg.MaxBacklog, TimeoutSec: cfg.Timeout.Seconds(), SlowReadBPS: cfg.SlowRead.BytesPerSecond, SlowReadRcvBuf: cfg.SlowRead.RcvBuf, H2C: cfg.H2C,
		},
		StartedAt:        started,
		ElapsedSeconds:   elapsed.Seconds(),
		Requests:         c,
		Errors:           Errors{ByKind: map[string]int64{}, StatusCodes: statusKeys(r.statuses)},
		ClampedLatencies: r.clamped,
		Latency: Latencies{
			All: summarize(r.all), Unary: summarize(r.unary), Stream: summarize(r.stream), TTFT: summarize(r.ttft),
			Service: summarize(r.service), Failed: summarize(r.failed), SendLag: summarize(r.lag),
		},
	}
	for kind, n := range r.kinds {
		rep.Errors.ByKind[string(kind)] = n
	}

	rates := Rates{TargetRPS: cfg.Rate}
	interval := time.Duration(1e9 / cfg.Rate)
	if span := r.lastSend + interval; c.Sent > 0 && span > 0 {
		rates.OfferedRPS = float64(c.Sent) / span.Seconds()
	}
	rates.ThroughputRPS = float64(c.SucceededInWindow) / cfg.Duration.Seconds()
	rates.Ratio = rates.OfferedRPS / rates.TargetRPS
	if c.Scheduled > 0 {
		rates.SuccessRate = float64(c.Succeeded) / float64(c.Scheduled)
	}
	if c.Sent > 0 {
		rates.ErrorRate = float64(c.Failed) / float64(c.Sent)
	}
	rep.Rates = rates

	switch {
	case interrupted:
		rep.Problems = append(rep.Problems, "the run was interrupted before its schedule finished")
	case c.Scheduled < cfg.measuredRequests():
		rep.Problems = append(rep.Problems, fmt.Sprintf("only %d of %d requests were scheduled", c.Scheduled, cfg.measuredRequests()))
	}
	if c.Dropped > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d requests were dropped: no free slot (max in flight %d) and no room to wait (max backlog %d)", c.Dropped, cfg.MaxInFlight, cfg.MaxBacklog))
	}
	if c.Abandoned > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d requests were still waiting to be sent when the drain period ended", c.Abandoned))
	}
	if c.Sent > 0 && float64(c.Late) > 0.01*float64(c.Sent) {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d of %d requests were sent more than %v late", c.Late, c.Sent, cfg.LateAfter))
	}
	if c.Sent > 0 && rates.Ratio < 0.99 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("offered %.1f requests per second, %.1f%% of the %.1f target", rates.OfferedRPS, 100*rates.Ratio, rates.TargetRPS))
	}
	rep.Valid = len(rep.Problems) == 0
	return rep
}

// redact drops any credentials embedded in a URL.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	return u.String()
}

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// WriteJSON writes the report as JSON to the file at path.
func (r *Report) WriteJSON(path string) error {
	data, err := r.JSON()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Text renders the report for a person.
func (r *Report) Text() string {
	var b strings.Builder
	c, rt := r.Requests, r.Rates
	fmt.Fprintf(&b, "%s %s %s at %.0f/s for %.0fs (warmup %.0fs)\n", r.Config.Dialect, r.Config.URL, r.Config.Model, r.Config.Rate, r.Config.DurationSec, r.Config.WarmupSec)
	fmt.Fprintf(&b, "  requests   scheduled %d, sent %d, succeeded %d, failed %d, dropped %d, late %d (peak in flight %d, backlog %d)\n", c.Scheduled, c.Sent, c.Succeeded, c.Failed, c.Dropped, c.Late, c.PeakInFlight, c.PeakBacklog)
	fmt.Fprintf(&b, "  rate       offered %.1f/s (%.1f%% of target), throughput %.1f/s, success %.2f%%, errors %.2f%%\n", rt.OfferedRPS, 100*rt.Ratio, rt.ThroughputRPS, 100*rt.SuccessRate, 100*rt.ErrorRate)
	row := func(name string, s Summary) {
		if s.Count > 0 {
			fmt.Fprintf(&b, "  %-10s n=%d  p50 %.2f  p90 %.2f  p95 %.2f  p99 %.2f  p99.9 %.2f  max %.2f ms\n", name, s.Count, s.P50Ms, s.P90Ms, s.P95Ms, s.P99Ms, s.P999Ms, s.MaxMs)
		}
	}
	l := r.Latency
	row("latency", l.All)
	row("unary", l.Unary)
	row("stream", l.Stream)
	row("ttft", l.TTFT)
	row("service", l.Service)
	row("failed", l.Failed)
	row("send lag", l.SendLag)
	if len(r.Errors.ByKind) > 0 {
		kinds := make([]string, 0, len(r.Errors.ByKind))
		for kind, n := range r.Errors.ByKind {
			kinds = append(kinds, fmt.Sprintf("%s %d", kind, n))
		}
		slices.Sort(kinds)
		fmt.Fprintf(&b, "  errors     %s\n", strings.Join(kinds, ", "))
	}
	if r.Valid {
		b.WriteString("  valid run\n")
	} else {
		for _, p := range r.Problems {
			fmt.Fprintf(&b, "  INVALID    %s\n", p)
		}
	}
	return b.String()
}

// Delta is the difference between two percentiles, in milliseconds.
type Delta struct {
	// Present is false when either run had no such requests.
	Present bool    `json:"present"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	P99Ms   float64 `json:"p99_ms"`
}

// Overhead is what a gateway adds, as the difference between a run through it
// and a baseline run straight to the upstream.
type Overhead struct {
	Unary  Delta `json:"unary"`
	Stream Delta `json:"stream"`
	// TTFT is the time-to-first-token overhead of streams.
	TTFT Delta `json:"ttft"`
	// Throughput is the change in successful requests per second.
	ThroughputRPS float64 `json:"throughput_rps"`
}

// Compare returns the overhead of run over baseline. The two should use the
// same workload and rate for the difference to mean anything; Compare does
// not check.
func Compare(baseline, run *Report) Overhead {
	delta := func(base, with Summary) Delta {
		if base.Count == 0 || with.Count == 0 {
			return Delta{}
		}
		return Delta{Present: true, P50Ms: with.P50Ms - base.P50Ms, P95Ms: with.P95Ms - base.P95Ms, P99Ms: with.P99Ms - base.P99Ms}
	}
	return Overhead{
		Unary:         delta(baseline.Latency.Unary, run.Latency.Unary),
		Stream:        delta(baseline.Latency.Stream, run.Latency.Stream),
		TTFT:          delta(baseline.Latency.TTFT, run.Latency.TTFT),
		ThroughputRPS: run.Rates.ThroughputRPS - baseline.Rates.ThroughputRPS,
	}
}
