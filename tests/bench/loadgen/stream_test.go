//go:build bench

package loadgen

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tyk-swe/olp/tests/bench/mockupstream"
)

// mock returns a mock upstream that answers like an LLM: its first frame
// after 50 ms, then a token every 10 ms, five tokens in all. Models named
// "bad", "abort" and "frame" fail with a 503, by aborting after two tokens and
// with an in-band error after two tokens.
func mock(t testing.TB) *mockupstream.Server {
	t.Helper()
	s, err := mockupstream.New(mockupstream.Config{
		Default: mockupstream.Behavior{TTFT: 50 * time.Millisecond, Interval: 10 * time.Millisecond, OutputTokens: 5},
		Models: map[string]mockupstream.Behavior{
			"bad":   {OutputTokens: 5, Status: 503},
			"abort": {OutputTokens: 5, FailAfterTokens: 2, FailMode: mockupstream.FailAbort},
			"frame": {OutputTokens: 5, FailAfterTokens: 2, FailMode: mockupstream.FailFrame},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var dialects = []Dialect{OpenAI, Anthropic, Gemini}

func TestFailuresAreClassifiedByHowTheyFail(t *testing.T) {
	for _, d := range dialects {
		t.Run(string(d), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				for _, tc := range []struct {
					model  string
					stream float64
					kind   string
					status string
					// ttft says whether the failed stream had begun.
					ttft bool
				}{
					{"bad", 0, "http_status", "503", false},
					{"bad", 1, "http_status", "503", false},
					{"abort", 1, "truncated", "200", true},
					{"frame", 1, "stream_error", "200", true},
				} {
					cfg := base(mock(t))
					cfg.Dialect, cfg.Model, cfg.Rate, cfg.StreamShare = d, tc.model, 10, tc.stream
					report := run(t, cfg)
					c := report.Requests
					if c.Succeeded != 0 || c.Failed != 10 || report.Errors.ByKind[tc.kind] != 10 || report.Errors.StatusCodes[tc.status] != 10 {
						t.Fatalf("%s stream=%v: %+v %+v", tc.model, tc.stream, c, report.Errors)
					}
					if report.Latency.All.Count != 0 || report.Latency.Failed.Count != 10 || (report.Latency.TTFT.Count == 10) != tc.ttft {
						t.Fatalf("%s: %+v", tc.model, report.Latency)
					}
					if report.Rates.SuccessRate != 0 || report.Rates.ErrorRate != 1 {
						t.Fatalf("%s: %+v", tc.model, report.Rates)
					}
				}
				// A stream that is aborted is still a success when it is asked for
				// a unary answer: the failure applies to streams.
				cfg := base(mock(t))
				cfg.Dialect, cfg.Model, cfg.Rate = d, "abort", 10
				if report := run(t, cfg); report.Requests.Succeeded != 10 {
					t.Fatalf("%+v", report.Requests)
				}
			})
		})
	}
}

func TestResponsesThatAreNotTheOneAskedForFail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for name, tc := range map[string]struct {
			h      http.HandlerFunc
			stream float64
		}{
			"empty unary body": {func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) }, 0},
			"json for a stream": {func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"1"}`)
			}, 1},
			"an event stream with no terminator": {func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[]}\n\n")
			}, 1},
		} {
			cfg := base(tc.h)
			cfg.Rate, cfg.StreamShare = 10, tc.stream
			report := run(t, cfg)
			if report.Requests.Succeeded != 0 || report.Requests.Failed != 10 {
				t.Errorf("%s: %+v %+v", name, report.Requests, report.Errors)
			}
		}
	})
}

// Each stream is several seconds long, so every one of the 2,000 is open
// when the last is scheduled.

func TestSlowReaderStopsWhenCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		r := newSlowReader(ctx, strings.NewReader(strings.Repeat("x", 100_000)), 100)
		go func() { time.Sleep(time.Second); cancel() }()
		start := time.Now()
		_, err := io.Copy(io.Discard, r)
		if err == nil || time.Since(start) != time.Second {
			t.Fatalf("%v after %v", err, time.Since(start))
		}
	})
}
