//go:build bench

package mockupstream

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// startNetwork serves a mock on a loopback port and returns clients for both
// protocols it speaks.
func startNetwork(t *testing.T, cfg Config) (s *Server, origin string, h1, h2c *http.Client) {
	t.Helper()
	s = newMock(t, cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, s) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	transport := func(configure func(*http.Protocols)) *http.Client {
		var p http.Protocols
		configure(&p)
		tr := &http.Transport{Protocols: &p}
		t.Cleanup(tr.CloseIdleConnections)
		return &http.Client{Transport: tr, Timeout: 20 * time.Second}
	}
	h1 = transport(func(p *http.Protocols) { p.SetHTTP1(true) })
	h2c = transport(func(p *http.Protocols) { p.SetUnencryptedHTTP2(true) })
	return s, "http://" + ln.Addr().String(), h1, h2c
}

func TestServesHTTP1AndCleartextHTTP2(t *testing.T) {
	_, origin, h1, h2c := startNetwork(t, Config{Default: DefaultBehavior()})
	for _, tc := range []struct {
		name  string
		c     *http.Client
		major int
	}{{"http1", h1, 1}, {"h2c", h2c, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range wires {
				for _, req := range []request{w.unary, w.stream} {
					httpReq, _ := http.NewRequest(http.MethodPost, origin+req.path, strings.NewReader(req.body))
					resp, err := tc.c.Do(httpReq)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || resp.StatusCode != 200 || resp.ProtoMajor != tc.major {
						t.Fatalf("%s: HTTP/%d %d, %v", req.path, resp.ProtoMajor, resp.StatusCode, err)
					}
					if frames, _ := readFrames(strings.NewReader(string(body)), time.Now()); strings.Contains(req.body, `"stream":true`) || strings.Contains(req.path, "stream") {
						if len(frames) < 16 {
							t.Fatalf("%s: %d frames", req.path, len(frames))
						}
					}
				}
			}
		})
	}
}

// TestAbortedStreamsFailOnTheWire is the point of using ErrAbortHandler
// rather than hijacking: it truncates an HTTP/1 response and resets an
// HTTP/2 stream, and the client must see an error either way, never a short
// success.
func TestAbortedStreamsFailOnTheWire(t *testing.T) {
	s, origin, h1, h2c := startNetwork(t, Config{Default: DefaultBehavior()})
	for name, c := range map[string]*http.Client{"http1": h1, "h2c": h2c} {
		for _, w := range wires {
			httpReq, _ := http.NewRequest(http.MethodPost, origin+w.stream.path, strings.NewReader(w.stream.body))
			httpReq.Header.Set("x-mock-fail-after-tokens", "3")
			resp, err := c.Do(httpReq)
			if err != nil {
				t.Fatalf("%s %s: %v", name, w.name, err)
			}
			frames, err := readFrames(resp.Body, time.Now())
			resp.Body.Close()
			if err == nil {
				t.Errorf("%s %s: the stream ended cleanly after %d frames", name, w.name, len(frames))
			}
		}
	}
	if want := int64(2 * len(wires)); s.Stats().AbortedStreams != want {
		t.Errorf("aborted %d streams, want %d", s.Stats().AbortedStreams, want)
	}
}

func TestPacingOnTheWire(t *testing.T) {
	_, origin, h1, h2c := startNetwork(t, Config{Default: DefaultBehavior()})
	for name, c := range map[string]*http.Client{"http1": h1, "h2c": h2c} {
		start := time.Now()
		httpReq, _ := http.NewRequest(http.MethodPost, origin+wires[0].stream.path, strings.NewReader(wires[0].stream.body))
		httpReq.Header.Set("x-mock-ttft-ms", "60")
		httpReq.Header.Set("x-mock-interval-ms", "10")
		httpReq.Header.Set("x-mock-output-tokens", "6")
		resp, err := c.Do(httpReq)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := readFrames(resp.Body, start)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		// Real timers never fire early. The upper bounds are generous for a
		// busy machine; the exact schedule is covered on the fake clock.
		if frames[0].at < 60*time.Millisecond || frames[len(frames)-1].at < 110*time.Millisecond || frames[len(frames)-1].at > 5*time.Second {
			t.Errorf("%s: first frame at %v, last at %v, want at least 60ms and 110ms", name, frames[0].at, frames[len(frames)-1].at)
		}
	}
}

func TestLargeBodiesAndManyStreamsOnOneConnection(t *testing.T) {
	s, origin, _, h2c := startNetwork(t, Config{Default: DefaultBehavior()})
	prompt := strings.Repeat("the ", 100_000) // about 100K tokens
	body := openAIBody("big", true, prompt)
	const streams = 64
	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			httpReq, _ := http.NewRequest(http.MethodPost, origin+"/v1/chat/completions", strings.NewReader(body))
			httpReq.Header.Set("x-mock-interval-ms", "5")
			resp, err := h2c.Do(httpReq)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			frames, err := readFrames(resp.Body, time.Now())
			switch {
			case err != nil:
				errs <- err
			case len(frames) != 1+16+1+1+1 || frames[len(frames)-1].data != "[DONE]":
				errs <- fmt.Errorf("%d frames", len(frames))
			case !strings.Contains(frames[len(frames)-2].data, fmt.Sprintf(`"prompt_tokens":%d`, promptFor(body))):
				errs <- fmt.Errorf("usage frame %q does not carry the prompt size", frames[len(frames)-2].data)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if st := s.Stats(); st.Streams != streams || st.RequestBytes != int64(streams*len(body)) || st.MaxInFlight < 2 {
		t.Errorf("stats %+v", st)
	}
}

func TestServeStopsWhenItsContextEnds(t *testing.T) {
	s := defaultMock(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, s) }()
	// A stream in progress must not hold the server open.
	httpReq, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+wires[0].stream.path, strings.NewReader(wires[0].stream.body))
	httpReq.Header.Set("x-mock-interval-ms", "1000")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("a sixteen second stream finished after the server was closed")
	}
}
