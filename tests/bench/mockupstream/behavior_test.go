//go:build bench

package mockupstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// dialectCase describes one way to ask for a stream and what its frames look like.
type dialectCase struct {
	name, path, body string
	// preamble is the number of frames before the first token frame; tokens
	// are the frames that carry one token each.
	preamble int
	isToken  func(frame) bool
	terminal func(frame) bool
	errFrame func(frame) bool
}

var streamCases = []dialectCase{
	{
		name: "openai", path: "/v1/chat/completions", body: openAIBody("m", true, "x"), preamble: 1,
		isToken:  func(f frame) bool { return strings.Contains(f.data, `"delta":{"content":"`) },
		terminal: func(f frame) bool { return f.data == "[DONE]" },
		errFrame: func(f frame) bool { return strings.HasPrefix(f.data, `{"error"`) },
	},
	{
		name: "responses", path: "/v1/responses", body: responsesBody("m", true, "x"), preamble: 0,
		isToken:  func(f frame) bool { return f.event == "response.output_text.delta" },
		terminal: func(f frame) bool { return f.event == "response.completed" },
		errFrame: func(f frame) bool { return f.event == "error" },
	},
	{
		name: "anthropic", path: "/v1/messages", body: anthropicBody("m", true, "x"), preamble: 3,
		isToken:  func(f frame) bool { return f.event == "content_block_delta" },
		terminal: func(f frame) bool { return f.event == "message_stop" },
		errFrame: func(f frame) bool { return f.event == "error" },
	},
	{
		name: "gemini", path: "/v1beta/models/m:streamGenerateContent?alt=sse", body: geminiBody, preamble: 0,
		isToken:  func(f frame) bool { return strings.Contains(f.data, `"text":" `) },
		terminal: func(f frame) bool { return strings.Contains(f.data, `"finishReason":"STOP"`) },
		errFrame: func(f frame) bool { return strings.HasPrefix(f.data, `{"error"`) },
	},
}

// stream opens a stream and returns its frames, with times relative to the
// moment the request was sent, and the error that ended the body.
func openStream(t *testing.T, s *Server, c dialectCase, headers map[string]string) ([]frame, error, time.Duration) {
	t.Helper()
	start := time.Now()
	resp, err := client(s).Do(request{path: c.path, body: c.body, headers: headers}.build(t))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	headersAt := time.Since(start)
	frames, err := readFrames(resp.Body, start)
	return frames, err, headersAt
}

func TestStreamPacing(t *testing.T) {
	for _, c := range streamCases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := defaultMock(t)
				frames, err, headersAt := openStream(t, s, c, map[string]string{"x-mock-ttft-ms": "100", "x-mock-interval-ms": "20", "x-mock-output-tokens": "5"})
				if err != nil {
					t.Fatal(err)
				}
				// Nothing, not even the response headers, precedes the time to first token.
				if headersAt != ms(100) || frames[0].at != ms(100) {
					t.Fatalf("headers at %v, first frame at %v, want 100ms", headersAt, frames[0].at)
				}
				var tokens []time.Duration
				for _, f := range frames {
					if c.isToken(f) {
						tokens = append(tokens, f.at)
					}
				}
				// Token i is due at TTFT + i*interval, whatever the timer's lateness before it.
				if want := []time.Duration{ms(100), ms(120), ms(140), ms(160), ms(180)}; !reflect.DeepEqual(tokens, want) {
					t.Fatalf("tokens at %v, want %v", tokens, want)
				}
				if last := frames[len(frames)-1]; !c.terminal(last) || last.at != ms(180) {
					t.Fatalf("last frame %+v: the terminal event follows the last token at once", last)
				}
			})
		})
	}
}

func TestZeroIntervalStreamsBackToBack(t *testing.T) {
	for _, c := range streamCases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := defaultMock(t)
				frames, err, _ := openStream(t, s, c, map[string]string{"x-mock-ttft-ms": "50", "x-mock-output-tokens": "40"})
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range frames {
					if f.at != ms(50) {
						t.Fatalf("frame %+v: with no interval everything follows the first frame", f)
					}
				}
			})
		})
	}
}

func TestUnaryIsHeldForTheTimeAStreamWouldTake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := defaultMock(t)
		for _, tc := range []struct {
			headers map[string]string
			want    time.Duration
		}{
			{nil, 0},
			{map[string]string{"x-mock-ttft-ms": "100"}, ms(100)},
			{map[string]string{"x-mock-ttft-ms": "100", "x-mock-interval-ms": "20", "x-mock-output-tokens": "5"}, ms(180)},
			{map[string]string{"x-mock-interval-ms": "2.5", "x-mock-output-tokens": "9"}, ms(20)},
		} {
			start := time.Now()
			s.post(t, request{path: "/v1/chat/completions", body: openAIBody("m", false, "x"), headers: tc.headers})
			if got := time.Since(start); got != tc.want {
				t.Errorf("%v took %v, want %v", tc.headers, got, tc.want)
			}
		}
	})
}

func TestMidStreamFailure(t *testing.T) {
	for _, c := range streamCases {
		t.Run(c.name+"/abort", func(t *testing.T) {
			s := defaultMock(t)
			frames, err, _ := openStream(t, s, c, map[string]string{"x-mock-fail-after-tokens": "3"})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("read error %v: an aborted stream must not look complete", err)
			}
			tokens := 0
			for _, f := range frames {
				if c.terminal(f) || c.errFrame(f) {
					t.Fatalf("terminal or error frame %+v in an aborted stream", f)
				}
				if c.isToken(f) {
					tokens++
				}
			}
			if tokens != 3 || len(frames) != c.preamble+3 {
				t.Fatalf("%d token frames in %d, want 3 after a preamble of %d", tokens, len(frames), c.preamble)
			}
			if st := s.Stats(); st.AbortedStreams != 1 || st.ErrorFrames != 0 {
				t.Fatalf("stats %+v", st)
			}
		})
		t.Run(c.name+"/error", func(t *testing.T) {
			s := defaultMock(t)
			frames, err, _ := openStream(t, s, c, map[string]string{"x-mock-fail-after-tokens": "3", "x-mock-fail-mode": "error"})
			if err != nil {
				t.Fatalf("an in-band failure ends the stream cleanly, got %v", err)
			}
			tokens := 0
			for _, f := range frames[:len(frames)-1] {
				if c.terminal(f) || c.errFrame(f) {
					t.Fatalf("early terminal or error frame %+v", f)
				}
				if c.isToken(f) {
					tokens++
				}
			}
			if last := frames[len(frames)-1]; tokens != 3 || !c.errFrame(last) || c.terminal(last) {
				t.Fatalf("%d tokens then %+v, want 3 then the error event and no terminator", tokens, last)
			}
			if st := s.Stats(); st.ErrorFrames != 1 || st.AbortedStreams != 0 {
				t.Fatalf("stats %+v", st)
			}
		})
	}
	t.Run("past the end", func(t *testing.T) {
		// Failing after more tokens than the stream has fails after the last.
		s := defaultMock(t)
		frames, err, _ := openStream(t, s, streamCases[0], map[string]string{"x-mock-fail-after-tokens": "99", "x-mock-output-tokens": "4"})
		if !errors.Is(err, io.ErrUnexpectedEOF) || len(frames) != 1+4 {
			t.Fatalf("%d frames, error %v", len(frames), err)
		}
	})
	t.Run("unary ignores it", func(t *testing.T) {
		s := defaultMock(t)
		resp, body := s.post(t, request{path: "/v1/chat/completions", body: openAIBody("m", false, "x"), headers: map[string]string{"x-mock-fail-after-tokens": "3"}})
		if resp.StatusCode != 200 || !json.Valid(body) {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	})
}

func TestStatusCodes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := defaultMock(t)
		// Errors are immediate unless asked to hang.
		start := time.Now()
		resp, _ := s.post(t, request{path: "/v1/messages", body: anthropicBody("m", false, "x"), headers: map[string]string{"x-mock-status": "529", "x-mock-ttft-ms": "500"}})
		if resp.StatusCode != 529 || time.Since(start) != 0 {
			t.Fatalf("status %d after %v", resp.StatusCode, time.Since(start))
		}
		start = time.Now()
		resp, _ = s.post(t, request{path: "/v1/messages", body: anthropicBody("m", false, "x"), headers: map[string]string{"x-mock-status": "503", "x-mock-error-delay-ms": "250"}})
		if resp.StatusCode != 503 || time.Since(start) != ms(250) {
			t.Fatalf("status %d after %v", resp.StatusCode, time.Since(start))
		}
		resp, _ = s.post(t, request{path: "/v1/messages", body: anthropicBody("m", false, "x"), headers: map[string]string{"x-mock-status": "200"}})
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}

func TestFailFirstNIsCountedPerModel(t *testing.T) {
	s := defaultMock(t)
	do := func(model string, headers map[string]string) int {
		resp, _ := s.post(t, request{path: "/v1/chat/completions", body: openAIBody(model, false, "x"), headers: headers})
		return resp.StatusCode
	}
	first2 := map[string]string{"x-mock-fail-first-n": "2"}
	var got []int
	for range 4 {
		got = append(got, do("flaky", first2))
	}
	// A second model has its own count.
	got = append(got, do("other", first2), do("other", first2), do("other", first2))
	if want := []int{503, 503, 200, 200, 503, 503, 200}; !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses %v, want %v", got, want)
	}
	// Reset starts the count again.
	s.Reset()
	if code := do("flaky", first2); code != 503 {
		t.Fatalf("status %d after a reset, want 503", code)
	}
	// A status header fails every request, whatever the count.
	if code := do("always", map[string]string{"x-mock-fail-first-n": "1", "x-mock-status": "429"}); code != 429 {
		t.Fatalf("status %d, want 429", code)
	}
	if code := do("always", map[string]string{"x-mock-fail-first-n": "1", "x-mock-status": "429"}); code != 429 {
		t.Fatalf("status %d, want 429 on every request", code)
	}
}

// TestFailFirstNIsExactUnderConcurrency checks the counter hands out failures
// atomically: with N configured, exactly N of many simultaneous requests fail.
func TestFailFirstNIsExactUnderConcurrency(t *testing.T) {
	s := newMock(t, Config{Default: DefaultBehavior(), Models: map[string]Behavior{"flaky": {OutputTokens: 1, FailFirstN: 7}}})
	const total = 200
	codes := make(chan int, total)
	for range total {
		go func() {
			resp, err := client(s).Do(request{path: "/v1/chat/completions", body: openAIBody("flaky", false, "x")}.build(t))
			if err != nil {
				codes <- -1
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	failed := 0
	for range total {
		if <-codes == 503 {
			failed++
		}
	}
	if failed != 7 {
		t.Fatalf("%d requests failed, want exactly 7", failed)
	}
}

// TestPerModelRules is the S4 shape: a route's first target always fails and
// its second succeeds, which a gateway run can only express by model since
// OLP does not forward client headers.
func TestPerModelRules(t *testing.T) {
	cfg := Config{Default: DefaultBehavior(), Models: map[string]Behavior{
		"primary":   {OutputTokens: 16, Status: 503},
		"secondary": {OutputTokens: 3},
	}}
	s := newMock(t, cfg)
	for _, tc := range []struct {
		name, path, body string
	}{
		{"openai", "/v1/chat/completions", "%s"},
		{"anthropic", "/v1/messages", "%s"},
		{"gemini", "/v1beta/models/%s:generateContent", geminiBody},
	} {
		path := func(model string) string { return strings.Replace(tc.path, "%s", model, 1) }
		body := func(model string) string {
			if tc.name == "gemini" {
				return tc.body
			}
			return openAIBody(model, false, "x")
		}
		resp, got := s.post(t, request{path: path("primary"), body: body("primary")})
		if resp.StatusCode != 503 || !json.Valid(got) {
			t.Errorf("%s primary: %d %s", tc.name, resp.StatusCode, got)
		}
		resp, got = s.post(t, request{path: path("secondary"), body: body("secondary")})
		if resp.StatusCode != 200 || !bytes.Contains(got, []byte(wantText(3)+`"`)) || bytes.Contains(got, []byte(wantText(4)+`"`)) {
			t.Errorf("%s secondary: %d %s", tc.name, resp.StatusCode, got)
		}
		resp, got = s.post(t, request{path: path("unlisted"), body: body("unlisted")})
		if resp.StatusCode != 200 || !bytes.Contains(got, []byte(wantText(16)+`"`)) {
			t.Errorf("%s unlisted: %d %s", tc.name, resp.StatusCode, got)
		}
	}
	// A request header still wins over the model's rule.
	resp, _ := s.post(t, request{path: "/v1/chat/completions", body: openAIBody("primary", false, "x"), headers: map[string]string{"x-mock-status": "200"}})
	if resp.StatusCode != 200 {
		t.Errorf("header override: %d", resp.StatusCode)
	}
	st := s.Stats()
	if got := st.Models["primary"]; got.Requests != 4 || got.Errors != 3 {
		t.Errorf("primary stats %+v", got)
	}
	if got := st.Models["secondary"]; got.Requests != 3 || got.Errors != 0 {
		t.Errorf("secondary stats %+v", got)
	}
}

func TestInvalidControlsAreRejectedVisibly(t *testing.T) {
	s := defaultMock(t)
	for _, tc := range []struct{ header, value string }{
		{"x-mock-ttft-ms", "soon"},
		{"x-mock-ttft-ms", "-1"},
		{"x-mock-ttft-ms", "NaN"},
		{"x-mock-ttft-ms", "999999999"},
		{"x-mock-interval-ms", "-0.5"},
		{"x-mock-error-delay-ms", "x"},
		{"x-mock-output-tokens", "0"},
		{"x-mock-output-tokens", "100001"},
		{"x-mock-output-tokens", "many"},
		{"x-mock-status", "302"},
		{"x-mock-status", "99"},
		{"x-mock-status", "600"},
		{"x-mock-fail-after-tokens", "-1"},
		{"x-mock-fail-after-tokens", "1.5"},
		{"x-mock-fail-first-n", "-2"},
		{"x-mock-fail-mode", "explode"},
		{"x-mock-fail-mode", `a"b\c`},
	} {
		resp, got := s.post(t, request{path: "/v1/chat/completions", body: openAIBody("m", false, "x"), headers: map[string]string{tc.header: tc.value}})
		var doc struct{ Error struct{ Message string } }
		if resp.StatusCode != 400 || json.Unmarshal(got, &doc) != nil || !strings.Contains(doc.Error.Message, textproto.CanonicalMIMEHeaderKey(tc.header)) {
			t.Errorf("%s: %s: %d %s", tc.header, tc.value, resp.StatusCode, got)
		}
	}
	if st := s.Stats(); st.InjectedErrors != 0 {
		t.Errorf("rejected requests are not injected failures: %+v", st)
	}
}

func TestHeaderNamesAreCanonical(t *testing.T) {
	for _, name := range []string{"x-mock-ttft-ms", "x-mock-interval-ms", "x-mock-output-tokens", "x-mock-status", "x-mock-fail-after-tokens", "x-mock-fail-mode", "x-mock-fail-first-n", "x-mock-error-delay-ms"} {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		found := false
		for _, c := range controls {
			found = found || c.header == canonical
		}
		if !found {
			t.Errorf("no control reads %s, which Go delivers as %s", name, canonical)
		}
	}
	if len(controls) != 8 {
		t.Errorf("%d controls", len(controls))
	}
}

func TestFractionalMillisecondsAreAccepted(t *testing.T) {
	b, err := applyHeaders(http.Header{"X-Mock-Ttft-Ms": {"0.25"}, "X-Mock-Interval-Ms": {"1.5"}}, DefaultBehavior())
	if err != nil || b.TTFT != 250*time.Microsecond || b.Interval != 1500*time.Microsecond {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestRuntimeConfiguration(t *testing.T) {
	s := defaultMock(t)
	put := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPut, "http://mock/_mock/config", strings.NewReader(body))
		resp, err := client(s).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(got)
	}
	status := func(model string) int {
		resp, _ := s.post(t, request{path: "/v1/chat/completions", body: openAIBody(model, false, "x")})
		return resp.StatusCode
	}
	if status("primary") != 200 {
		t.Fatal("healthy before the rule")
	}
	// Certification runs against a healthy mock; the rule is armed afterwards.
	code, body := put(`{"default":{"output_tokens":4},"models":{"primary":{"status":503},"slow":{"ttft_ms":50}}}`)
	if code != 200 {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if status("primary") != 503 || status("secondary") != 200 {
		t.Fatal("rule not applied")
	}
	var spec Spec
	req, _ := http.NewRequest(http.MethodGet, "http://mock/_mock/config", nil)
	resp, err := client(s).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cfg, err := spec.Config(Config{Default: DefaultBehavior()})
	if err != nil || cfg.Default.OutputTokens != 4 || cfg.Models["primary"].Status != 503 || cfg.Models["slow"].TTFT != ms(50) || cfg.Models["slow"].OutputTokens != 4 {
		t.Fatalf("effective configuration %+v %v", cfg, err)
	}
	// A model rule inherits the default of the same spec, and the spec
	// replaces rather than merges: "primary" is healthy again.
	if code, body = put(`{"default":{"output_tokens":2},"models":{"other":{}}}`); code != 200 {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if status("primary") != 200 || s.Config().Models["other"].OutputTokens != 2 {
		t.Fatal("replacement not applied")
	}
	for _, bad := range []string{
		`{"default":{"output_tokens":0}}`,
		`{"default":{"status":302}}`,
		`{"default":{"fail_mode":"explode"}}`,
		`{"models":{"m":{"ttft_ms":-5}}}`,
		`{"models":{"bad\"name":{}}}`,
		`{"unknown":1}`,
		`{"default":{"unknown":1}}`,
		`not json`,
	} {
		if code, body := put(bad); code != 400 {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
	if s.Config().Default.OutputTokens != 2 {
		t.Error("a rejected configuration must leave the previous one in place")
	}
}

func TestParseRule(t *testing.T) {
	rule, err := ParseRule("ttft_ms=12.5, interval_ms=20,output_tokens=64,status=503,error_delay_ms=1,fail_after_tokens=5,fail_mode=error,fail_first_n=2")
	if err != nil {
		t.Fatal(err)
	}
	b, err := rule.Apply(DefaultBehavior())
	want := Behavior{TTFT: 12500 * time.Microsecond, Interval: ms(20), OutputTokens: 64, Status: 503, ErrorDelay: ms(1), FailAfterTokens: 5, FailMode: FailFrame, FailFirstN: 2}
	if err != nil || b != want {
		t.Fatalf("%+v %v, want %+v", b, err, want)
	}
	// An unset field inherits.
	rule, _ = ParseRule("status=429")
	if b, _ = rule.Apply(want); b.Status != 429 || b.OutputTokens != 64 {
		t.Fatalf("%+v", b)
	}
	for _, bad := range []string{"ttft_ms", "ttft_ms=x", "unknown=1", "output_tokens=1.5", "output_tokens=0"} {
		rule, err := ParseRule(bad)
		if err == nil {
			_, err = rule.Apply(DefaultBehavior())
		}
		if err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if r, err := ParseRule("  "); err != nil || !reflect.DeepEqual(r, Rule{}) {
		t.Errorf("empty rule %+v %v", r, err)
	}
}

func TestSpecRoundTrip(t *testing.T) {
	cfg := Config{
		Default: Behavior{TTFT: ms(5), Interval: ms(2), OutputTokens: 9, FailMode: FailFrame},
		Models:  map[string]Behavior{"a": {OutputTokens: 1, Status: 503}, "b": {TTFT: ms(1), OutputTokens: 100, FailAfterTokens: 3, FailFirstN: 4, ErrorDelay: ms(9)}},
	}
	encoded, err := json.Marshal(SpecOf(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	if err = json.Unmarshal(encoded, &spec); err != nil {
		t.Fatal(err)
	}
	back, err := spec.Config(Config{})
	if err != nil || !reflect.DeepEqual(back, cfg) {
		t.Fatalf("%+v %v\nwant %+v", back, err, cfg)
	}
}

func TestStatsAndReset(t *testing.T) {
	s := defaultMock(t)
	body := openAIBody("m", false, "hello")
	s.post(t, request{path: "/v1/chat/completions", body: body})
	s.post(t, request{path: "/v1/chat/completions", body: openAIBody("m", true, "hello")})
	s.post(t, request{path: "/v1/messages", body: anthropicBody("m", false, "x"), headers: map[string]string{"x-mock-status": "503"}})
	s.post(t, request{path: "/v1beta/models/g:generateContent", body: geminiBody})
	st := s.Stats()
	if st.Requests != 4 || st.Unary != 3 || st.Streams != 1 || st.InjectedErrors != 1 || st.Dialects["openai"] != 2 || st.Dialects["anthropic"] != 1 || st.Dialects["gemini"] != 1 {
		t.Fatalf("stats %+v", st)
	}
	if want := int64(len(body)+len(openAIBody("m", true, "hello"))+len(anthropicBody("m", false, "x"))) + int64(len(geminiBody)); st.RequestBytes != want {
		t.Fatalf("request bytes %d, want %d", st.RequestBytes, want)
	}
	if st.InFlight != 0 || st.MaxInFlight < 1 || st.Models["m"].Requests != 3 || st.Models["g"].Requests != 1 {
		t.Fatalf("stats %+v", st)
	}
	// The same numbers over HTTP.
	req, _ := http.NewRequest(http.MethodPost, "http://mock/_mock/reset", nil)
	resp, err := client(s).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var after Stats
	if err = json.NewDecoder(resp.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if after.Requests != 0 || after.RequestBytes != 0 || len(after.Models) != 0 || s.Stats().Requests != 0 {
		t.Fatalf("after reset %+v", after)
	}
}

func TestMaxInFlightTracksConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := defaultMock(t)
		done := make(chan struct{})
		for range 25 {
			go func() {
				resp, err := client(s).Do(request{path: "/v1/chat/completions", body: openAIBody("m", false, "x"), headers: map[string]string{"x-mock-ttft-ms": "100"}}.build(t))
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				done <- struct{}{}
			}()
		}
		synctest.Wait()
		if st := s.Stats(); st.InFlight != 25 {
			t.Errorf("in flight %d", st.InFlight)
		}
		for range 25 {
			<-done
		}
		if st := s.Stats(); st.InFlight != 0 || st.MaxInFlight != 25 {
			t.Errorf("stats %+v", st)
		}
	})
}

func TestClientDisconnectEndsAStream(t *testing.T) {
	slow := map[string]string{"x-mock-interval-ms": "100", "x-mock-output-tokens": "1000"}
	t.Run("context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := defaultMock(t)
			ctx, cancel := context.WithCancel(context.Background())
			resp, err := client(s).Do(request{path: "/v1/chat/completions", body: openAIBody("m", true, "x"), headers: slow}.build(t).WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}
			// Reading with a buffer larger than a frame takes the whole first write.
			resp.Body.Read(make([]byte, 4096))
			time.Sleep(50 * time.Millisecond)
			cancel()
			synctest.Wait()
			// The handler woke at once, in the middle of waiting for the next
			// token, rather than finishing its hundred seconds.
			if st := s.Stats(); st.InFlight != 0 || st.ClientGone != 1 {
				t.Fatalf("stats %+v", st)
			}
			resp.Body.Close()
		})
	})
	t.Run("write failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := defaultMock(t)
			resp, err := client(s).Do(request{path: "/v1/chat/completions", body: openAIBody("m", true, "x"), headers: slow}.build(t))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Read(make([]byte, 4096))
			resp.Body.Close()
			time.Sleep(time.Second)
			synctest.Wait()
			if st := s.Stats(); st.InFlight != 0 || st.ClientGone != 1 {
				t.Fatalf("stats %+v", st)
			}
		})
	})
}

func TestResponsesAreDeterministic(t *testing.T) {
	s := defaultMock(t)
	for _, c := range streamCases {
		_, a := s.post(t, request{path: c.path, body: c.body})
		_, b := s.post(t, request{path: c.path, body: c.body})
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two identical requests differ", c.name)
		}
	}
	for i := range words {
		if !strings.HasPrefix(words[i], " ") || strings.TrimSpace(words[i]) == "" || strings.ContainsAny(words[i], `"\`) {
			t.Errorf("word %d %q", i, words[i])
		}
	}
}
