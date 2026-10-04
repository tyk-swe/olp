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

func TestStreamTimeToFirstTokenAndLatency(t *testing.T) {
	for _, d := range dialects {
		t.Run(string(d), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := base(mock(t))
				cfg.Dialect, cfg.Rate, cfg.StreamShare = d, 20, 1
				report := run(t, cfg)
				c, l := report.Requests, report.Latency
				if c.Streams != 20 || c.Unary != 0 || c.Succeeded != 20 || report.Errors.StatusCodes["200"] != 20 {
					t.Fatalf("%+v %+v", c, report.Errors)
				}
				// The first frame arrives at the time to first token; the stream
				// ends four intervals later.
				near(t, "ttft p50", l.TTFT.P50Ms, 50)
				near(t, "ttft p99", l.TTFT.P99Ms, 50)
				near(t, "stream p50", l.Stream.P50Ms, 90)
				near(t, "stream p99", l.Stream.P99Ms, 90)
				near(t, "service", l.Service.P50Ms, 90)
				if l.TTFT.Count != 20 || l.Unary.Count != 0 || l.All.Count != 20 || report.Requests.ResponseBytes < 20*500 {
					t.Fatalf("%+v %+v", l, c)
				}
				if !report.Valid {
					t.Fatal(report.Problems)
				}
			})
		})
	}
}

// piece is part of a streamed answer, written after a wait.
type piece struct {
	after time.Duration
	text  string
}

// answering streams its pieces and then holds the response open for linger, as
// a gateway does that settles a budget after its last frame and before it
// returns, which is when the response ends.
func answering(linger time.Duration, pieces ...piece) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, p := range pieces {
			time.Sleep(p.after)
			io.WriteString(w, p.text)
			w.(http.Flusher).Flush()
		}
		time.Sleep(linger)
	})
}

// TestStreamLatencyEndsWhenTheClosingEventIsComplete: a client has its answer
// at the closing event, so what a gateway does between that and ending the
// response is not charged to the stream, and the body is still read to its
// end. A stream that has no closing event, or ends in an error, has no such
// moment and is timed to the end of the response.
func TestStreamLatencyEndsWhenTheClosingEventIsComplete(t *testing.T) {
	const linger = time.Second
	closing := map[Dialect]string{
		OpenAI:    "data: {\"a\":1}\n\ndata: [DONE]\n\n",
		Anthropic: "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n",
		Gemini:    "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\r\n\r\n",
	}
	for _, d := range dialects {
		t.Run(string(d)+" lingers after the closing event", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := base(answering(linger, piece{0, closing[d]}))
				cfg.Dialect, cfg.Rate, cfg.StreamShare = d, 10, 1
				report := run(t, cfg)
				l := report.Latency
				if report.Requests.Succeeded != 10 || l.Stream.Count != 10 || report.Requests.ResponseBytes != int64(10*len(closing[d])) {
					t.Fatalf("%+v %+v", report.Requests, report.Errors)
				}
				near(t, "stream p50", l.Stream.P50Ms, 0)
				near(t, "stream max", l.Stream.MaxMs, 0)
				near(t, "all p99", l.All.P99Ms, 0)
				near(t, "service", l.Service.MaxMs, 0)
				// The last response was open for a second after its closing
				// event, and the run waited for it.
				if report.ElapsedSeconds < 1.5 {
					t.Fatalf("the run took %.2f s, so the lingering responses were not read to their end", report.ElapsedSeconds)
				}
			})
		})
	}

	for _, tc := range []struct {
		name   string
		pieces []piece
		// kind is how the stream failed, "" when it succeeded; ms is its latency.
		kind string
		ms   float64
	}{
		{"the closing event is complete when its blank line arrives",
			[]piece{{0, "data: {\"a\":1}\n\ndata: [DONE]\n"}, {300 * time.Millisecond, "\n"}}, "", 300},
		{"a closing event with no blank line is complete at the end of the response",
			[]piece{{0, "data: {\"a\":1}\n\ndata: [DONE]"}}, "", 1000},
		{"a truncated stream is timed to the end of the response",
			[]piece{{0, "data: {\"a\":1}\n\n"}}, "truncated", 1000},
		{"an in-band error is timed to the end of the response",
			[]piece{{0, "data: {\"a\":1}\n\ndata: {\"error\":{}}\n\n"}}, "stream_error", 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := base(answering(linger, tc.pieces...))
				cfg.Rate, cfg.StreamShare = 10, 1
				report := run(t, cfg)
				if tc.kind == "" {
					if report.Requests.Succeeded != 10 {
						t.Fatalf("%+v %+v", report.Requests, report.Errors)
					}
					near(t, "stream p50", report.Latency.Stream.P50Ms, tc.ms)
					return
				}
				if report.Requests.Failed != 10 || report.Errors.ByKind[tc.kind] != 10 {
					t.Fatalf("%+v %+v", report.Requests, report.Errors)
				}
				near(t, "failed p50", report.Latency.Failed.P50Ms, tc.ms)
			})
		})
	}
}

// TestTimeToFirstTokenIncludesQueueing is the stream counterpart of the test
// of bounded concurrency: with one slot and a stream that takes 90 ms against
// an arrival every 50 ms, each request waits for the one before it, and its
// first token is timed from when it was due, not from when it could be sent.
func TestTimeToFirstTokenIncludesQueueing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(mock(t))
		cfg.Rate, cfg.StreamShare, cfg.MaxInFlight, cfg.MaxBacklog = 20, 1, 1, 100
		report := run(t, cfg)
		l := report.Latency
		if l.TTFT.Count != 20 || report.Requests.Dropped != 0 {
			t.Fatalf("%+v", report.Requests)
		}
		// Request k is due at 50k ms, sent at 90k ms when the slot frees, and
		// its first frame arrives 50 ms after that: 50 + 40k ms from the schedule.
		near(t, "ttft min", l.TTFT.MinMs, 50)
		near(t, "ttft p50", l.TTFT.P50Ms, 410)
		near(t, "ttft max", l.TTFT.MaxMs, 810)
		// Timed from the send it is 50 ms for every request, which is what a
		// closed-loop generator would report.
		if l.TTFT.P50Ms < 5*50 {
			t.Fatalf("ttft p50 %.1f ms: the queueing is hidden", l.TTFT.P50Ms)
		}
	})
}

func TestUnaryLatencyIncludesGenerationTime(t *testing.T) {
	for _, d := range dialects {
		t.Run(string(d), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := base(mock(t))
				cfg.Dialect, cfg.Rate = d, 20
				report := run(t, cfg)
				// The mock holds a unary response for as long as it would take to stream.
				near(t, "unary p50", report.Latency.Unary.P50Ms, 90)
				if report.Requests.Unary != 20 || report.Latency.Stream.Count != 0 || report.Latency.TTFT.Count != 0 {
					t.Fatalf("%+v %+v", report.Requests, report.Latency)
				}
			})
		})
	}
}

func TestMixedTrafficIsSplitByKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(mock(t))
		cfg.Rate, cfg.StreamShare = 40, 0.5
		report := run(t, cfg)
		l := report.Latency
		if report.Requests.Streams != 20 || report.Requests.Unary != 20 || l.Stream.Count != 20 || l.Unary.Count != 20 || l.All.Count != 40 || l.TTFT.Count != 20 {
			t.Fatalf("%+v %+v", report.Requests, l)
		}
	})
}

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

// TestSlowReadersHoldStreamsOpen is S6's mechanism: a reader that takes
// bytes at a fixed pace keeps each stream alive for as long as the pace
// dictates, and the generator holds as many at once as the schedule asks.
func TestSlowReadersHoldStreamsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(mock(t))
		cfg.Rate, cfg.StreamShare = 10, 1
		cfg.Timeout = time.Minute
		fast := run(t, cfg)
		size := fast.Requests.ResponseBytes / 10

		cfg.SlowRead = SlowRead{BytesPerSecond: 500}
		slow := run(t, cfg)
		if slow.Requests.Succeeded != 10 || slow.Requests.ResponseBytes != fast.Requests.ResponseBytes {
			t.Fatalf("%+v", slow.Requests)
		}
		// The reader starts when the first frame arrives, after 50 ms, and
		// takes the whole response at 500 bytes a second.
		near(t, "slow stream p50", slow.Latency.Stream.P50Ms, 50+float64(size)/500*1000)
		// Ten streams were scheduled over a second and each outlived it.
		if slow.Requests.PeakInFlight != 10 {
			t.Fatalf("peak in flight %d, want all 10 at once", slow.Requests.PeakInFlight)
		}
		// The first read is not held back, but the first frame is a few hundred
		// bytes and arrives at the reader's pace, well before the end.
		if ttft := slow.Latency.TTFT.P50Ms; ttft < 50 || ttft > slow.Latency.Stream.P50Ms/2 {
			t.Fatalf("ttft %.1f ms against a stream of %.1f ms", ttft, slow.Latency.Stream.P50Ms)
		}
	})
}

func TestManyConcurrentSlowStreams(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := base(mock(t))
		cfg.Rate, cfg.StreamShare, cfg.Duration = 2000, 1, time.Second
		cfg.MaxInFlight, cfg.Timeout = 5000, 10*time.Minute
		cfg.SlowRead = SlowRead{BytesPerSecond: 500}
		report := run(t, cfg)
		c := report.Requests
		// Each stream is several seconds long, so every one of the 2,000 is open
		// when the last is scheduled.
		if c.Succeeded != 2000 || c.PeakInFlight != 2000 || c.Dropped != 0 {
			t.Fatalf("%+v", c)
		}
		if !report.Valid {
			t.Fatal(report.Problems)
		}
	})
}

func TestSlowReaderPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		data := strings.Repeat("x", 5000)
		start := time.Now()
		r := newSlowReader(t.Context(), strings.NewReader(data), 1000)
		n, err := io.Copy(io.Discard, r)
		if err != nil || n != 5000 {
			t.Fatalf("%d, %v", n, err)
		}
		// 5,000 bytes at 1,000 a second: the last byte is due after five seconds.
		if got := time.Since(start); got != 5*time.Second {
			t.Fatalf("took %v", got)
		}
	})
}

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

func TestReadSSE(t *testing.T) {
	long := "data: " + strings.Repeat("x", 10_000) + "\n\n"
	for _, tc := range []struct {
		name, input string
		dialect     Dialect
		frames      int
		terminal    bool
		errored     bool
	}{
		{"openai complete", "data: {\"a\":1}\n\ndata: [DONE]\n\n", OpenAI, 2, true, false},
		{"openai crlf", "data: {\"a\":1}\r\n\r\ndata: [DONE]\r\n\r\n", OpenAI, 2, true, false},
		{"openai no space after the colon", "data:{\"a\":1}\n\ndata:[DONE]\n\n", OpenAI, 2, true, false},
		{"openai truncated", "data: {\"a\":1}\n\n", OpenAI, 1, false, false},
		{"openai unterminated last line", "data: {\"a\":1}\n\ndata: [DONE]", OpenAI, 2, true, false},
		{"openai error frame", "data: {\"a\":1}\n\ndata: {\"error\":{\"message\":\"x\"}}\n\n", OpenAI, 2, false, true},
		{"openai in-band error then done", "data: {\"error\":{}}\n\ndata: [DONE]\n\n", OpenAI, 2, true, true},
		{"openai done text elsewhere is no terminator", "data: {\"text\":\"[DONE]\"}\n\n", OpenAI, 1, false, false},
		{"openai comments and ids are no frames", ": keep-alive\n\nid: 5\n\ndata: [DONE]\n\n", OpenAI, 1, true, false},
		{"long line", long + "data: [DONE]\n\n", OpenAI, 2, true, false},
		{"long line that hides a field", "event: " + strings.Repeat("x", 5000) + "\ndata: [DONE]\n\n", OpenAI, 1, true, false},
		{"anthropic complete", "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n", Anthropic, 2, true, false},
		{"anthropic truncated", "event: message_start\ndata: {}\n\nevent: content_block_delta\ndata: {}\n\n", Anthropic, 2, false, false},
		{"anthropic error event", "event: message_start\ndata: {}\n\nevent: error\ndata: {\"type\":\"error\"}\n\n", Anthropic, 2, false, true},
		{"anthropic [DONE] is no terminator", "event: message_start\ndata: {}\n\ndata: [DONE]\n\n", Anthropic, 2, false, false},
		{"gemini finished", "data: {\"candidates\":[{\"text\":\"a\"}]}\r\n\r\ndata: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\r\n\r\n", Gemini, 2, true, false},
		{"gemini truncated", "data: {\"candidates\":[{\"text\":\"a\"}]}\r\n\r\n", Gemini, 1, false, false},
		{"gemini error", "data: {\"error\":{\"code\":503}}\r\n\r\n", Gemini, 1, false, true},
		{"gemini finish reason after a long text", geminiChunk(6000, `"finishReason":"STOP"`), Gemini, 1, true, false},
		{"gemini long chunk without a finish reason", geminiChunk(6000, `"index":0`), Gemini, 1, false, false},
		{"gemini finish reason in a chunk of several buffers", geminiChunk(20_000, `"finishReason":"STOP"`), Gemini, 1, true, false},
		{"empty", "", OpenAI, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Any split of the same bytes must give the same answer.
			for _, r := range []io.Reader{strings.NewReader(tc.input), &oneByte{r: strings.NewReader(tc.input)}} {
				s, err := readSSE(r, tc.dialect)
				if err != nil || s.frames != tc.frames || s.terminal != tc.terminal || s.errored != tc.errored || s.bytes != int64(len(tc.input)) {
					t.Fatalf("%+v %v, want %d frames, terminal %v, errored %v over %d bytes", s, err, tc.frames, tc.terminal, tc.errored, len(tc.input))
				}
				if (tc.frames > 0) == s.first.IsZero() {
					t.Fatalf("first frame time %v with %d frames", s.first, s.frames)
				}
			}
		})
	}
}

// geminiChunk is a Gemini frame whose text is n bytes long, followed by tail.
func geminiChunk(n int, tail string) string {
	return `data: {"candidates":[{"content":{"parts":[{"text":"` + strings.Repeat("x", n) + `"}]},` + tail + "}]}\r\n\r\n"
}

// TestReadSSEFindsAFinishReasonWhereverItFallsInALongLine moves the finish
// reason of a Gemini chunk across the end of the read buffer, so that it lies
// before it, across it and after it, at the first end and at the second.
func TestReadSSEFindsAFinishReasonWhereverItFallsInALongLine(t *testing.T) {
	const prefix = `data: {"candidates":[{"content":{"parts":[{"text":"`
	suffix := `"}]},"finishReason":"STOP"}]}` + "\r\n\r\n"
	keyAt := len(prefix) + len(`"}]},`)
	for _, end := range []int{4096, 2 * 4096} {
		for text := end - keyAt - 2*len(finishKey); text <= end-keyAt+len(finishKey); text++ {
			input := prefix + strings.Repeat("x", text) + suffix
			for _, r := range []io.Reader{strings.NewReader(input), &oneByte{r: strings.NewReader(input)}} {
				s, err := readSSE(r, Gemini)
				if err != nil || !s.terminal || s.frames != 1 || s.bytes != int64(len(input)) || s.done.IsZero() {
					t.Fatalf("a finish reason at byte %d of the line: %+v %v", keyAt+text, s, err)
				}
			}
		}
	}
}

// TestReadSSETimesTheEventThatClosesTheStream feeds a stream at the pace of a
// fake clock: the closing event is complete at its blank line, not at the line
// that makes it the closing event and not at the end of the stream.
func TestReadSSETimesTheEventThatClosesTheStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialect Dialect
		pieces  []piece
		// done is when the closing event is complete, zero for never.
		done time.Duration
	}{
		{"openai", OpenAI, []piece{{0, "data: {}\n\n"}, {time.Second, "data: [DONE]\n"}, {time.Second, "\n"}}, 2 * time.Second},
		{"anthropic, whose closing event is two lines", Anthropic, []piece{{0, "event: message_stop\n"}, {time.Second, "data: {}\n"}, {time.Second, "\n"}}, 2 * time.Second},
		{"gemini", Gemini, []piece{{0, "data: {}\r\n\r\n"}, {time.Second, "data: {\"finishReason\":\"STOP\"}\r\n"}, {time.Second, "\r\n"}}, 2 * time.Second},
		{"a closing event with no blank line", OpenAI, []piece{{time.Second, "data: [DONE]\n"}}, 0},
		{"a stream that does not close", OpenAI, []piece{{time.Second, "data: {}\n\n"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pr, pw := io.Pipe()
				go func() {
					for _, p := range tc.pieces {
						time.Sleep(p.after)
						io.WriteString(pw, p.text)
					}
					time.Sleep(5 * time.Second)
					pw.Close()
				}()
				start := time.Now()
				s, err := readSSE(pr, tc.dialect)
				if err != nil {
					t.Fatal(err)
				}
				if s.done.IsZero() != (tc.done == 0) || (tc.done > 0 && s.done.Sub(start) != tc.done) {
					t.Fatalf("done after %v, want %v (zero for never)", s.done.Sub(start), tc.done)
				}
				if tc.done > 0 && time.Since(start) != tc.done+5*time.Second {
					t.Fatalf("the stream was read to %v, not to its end", time.Since(start))
				}
			})
		})
	}
}

type oneByte struct{ r io.Reader }

func (o *oneByte) Read(p []byte) (int, error) { return o.r.Read(p[:min(len(p), 1)]) }
