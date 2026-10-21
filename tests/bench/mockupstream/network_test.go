//go:build bench

package mockupstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
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
