package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// carrierFunc stands in for the unconfined plugins that carry their profiles'
// traffic.
type carrierFunc func(ctx context.Context, provider abi.Provider, req *http.Request) (*http.Response, error)

func (f carrierFunc) Carry(ctx context.Context, _ string, provider abi.Provider, req *http.Request, _ []string) (*http.Response, error) {
	return f(ctx, provider, req)
}

// forward carries a request to the upstream as a plugin would, marking it.
func forward(ctx context.Context, req *http.Request) (*http.Response, error) {
	carried := req.Clone(ctx)
	carried.RequestURI = ""
	carried.Header.Set("X-Carried-By", "acme")
	return http.DefaultClient.Do(carried)
}

// newCarryingHarness serves provider a from an unconfined plugin's profile
// whose traffic carrier carries; provider b stays its failover.
func newCarryingHarness(t *testing.T, carrier connectors.Carrier) *harness {
	t.Helper()
	h := newHarness(t, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096, Carrier: carrier, UnconfinedPlugins: true})
	manifest := abi.Manifest{Name: "acme", Version: "1.0.0", Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", CarriesTraffic: true,
		Hosting: abi.Hosting{Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}},
	}}}
	plugin, err := connectors.NewUnconfinedPluginProfile(strings.Repeat("cd", 32), manifest, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	h.pinPlugin(plugin)
	return h
}

// The plugin carries the finished request, placed and authenticated, for the
// provider it serves, and the upstream's response comes back through it.
func TestAPluginCarriesTheFinishedRequest(t *testing.T) {
	var provider abi.Provider
	var received *http.Request
	h := newCarryingHarness(t, carrierFunc(func(ctx context.Context, p abi.Provider, req *http.Request) (*http.Response, error) {
		provider, received = p, req
		return forward(ctx, req)
	}))
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Carried-By") != "acme" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		completion(modelA, answerText)(w, r)
	})
	if resp, body := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK || !strings.Contains(fmt.Sprint(body), answerText) {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if provider.Profile != "acme-chat" || received.URL.String() != h.upstream.URL+"/a/v1/chat/completions" || received.Header.Get("Authorization") != "Bearer "+secretA {
		t.Fatalf("the plugin carried %v for %+v", received, provider)
	}
	if env := h.sink.last(t); len(env.Attempts) != 1 || env.Attempts[0].Class != classSuccess || h.mock.count("b") != 0 {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

// A carried stream reaches the caller as the plugin streams it back: the
// caller reads each event before the plugin has the next.
func TestACarriedStreamReachesTheCallerAsItArrives(t *testing.T) {
	events, stream := io.Pipe()
	h := newCarryingHarness(t, carrierFunc(func(context.Context, abi.Provider, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: events}, nil
	}))
	chunk := func(delta string) string {
		return fmt.Sprintf("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":%s}]}\n\n", modelA, delta, map[bool]string{true: `"stop"`, false: "null"}[delta == "{}"])
	}
	go io.WriteString(stream, chunk(`{"content":"first"}`))
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
	defer resp.Body.Close()
	lines := bufio.NewReader(resp.Body)
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended before its first event: %v", err)
		}
		if strings.Contains(line, "first") {
			break
		}
	}
	go func() {
		io.WriteString(stream, chunk(`{"content":"second"}`)+chunk("{}")+"data: [DONE]\n\n")
		stream.Close()
	}()
	rest, err := io.ReadAll(lines)
	if err != nil || !strings.Contains(string(rest), "second") || !strings.Contains(string(rest), "data: [DONE]") {
		t.Fatalf("the rest of the stream %q: %v", rest, err)
	}
}

// A failure the plugin reports as not sent fails over. Any other failure the
// plugin carrying the request has, or a server failure it carries back, leaves
// the upstream's outcome unknown, so the request does not fail over; a
// rejection the upstream stated fails over as it would without the plugin.
func TestOnlyCarriedFailuresReportedNotSentFailOver(t *testing.T) {
	var mu sync.Mutex
	var failure string
	h := newCarryingHarness(t, carrierFunc(func(ctx context.Context, _ abi.Provider, req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch failure {
		case "not sent":
			return nil, fmt.Errorf("%w: the plugin could not connect", connectors.ErrNotSent)
		case "lost":
			resp, err := forward(ctx, req)
			if err == nil {
				resp.Body.Close()
			}
			return nil, errors.New("the plugin lost the response")
		}
		return forward(ctx, req)
	}))
	fail := func(how string) {
		mu.Lock()
		defer mu.Unlock()
		failure = how
	}

	fail("lost")
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "ambiguous_upstream_result" || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
		t.Fatalf("status %d body %v a=%d b=%d", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"))
	}
	if env := h.sink.last(t); len(env.Attempts) != 1 || env.Attempts[0].Class != classAmbiguous {
		t.Fatalf("attempts %+v", env.Attempts)
	}

	fail("")
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	if resp, body = h.chat(fullKey, nil); resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "ambiguous_upstream_result" || h.mock.count("b") != 0 {
		t.Fatalf("status %d body %v b=%d", resp.StatusCode, body, h.mock.count("b"))
	}

	fail("not sent")
	if resp, _ = h.chat(fullKey, nil); resp.StatusCode != http.StatusOK || h.mock.count("a") != 2 || h.mock.count("b") != 1 {
		t.Fatalf("status %d a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
	}
	if env := h.sink.last(t); len(env.Attempts) != 2 || env.Attempts[0].Class != classConnect || env.Attempts[1].Class != classSuccess {
		t.Fatalf("attempts %+v", env.Attempts)
	}

	fail("")
	h.mock.set("a", status(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	if resp, _ = h.chat(fullKey, nil); resp.StatusCode != http.StatusOK || h.mock.count("b") != 2 {
		t.Fatalf("status %d b=%d", resp.StatusCode, h.mock.count("b"))
	}
	if env := h.sink.last(t); len(env.Attempts) != 2 || env.Attempts[0].Class != classRateLimit {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

// When the caller goes away, the plugin carrying its request learns so.
func TestTheCallersCancellationReachesTheCarrier(t *testing.T) {
	carrying, cancelled := make(chan struct{}), make(chan error, 1)
	h := newCarryingHarness(t, carrierFunc(func(ctx context.Context, _ abi.Provider, _ *http.Request) (*http.Response, error) {
		close(carrying)
		<-ctx.Done()
		cancelled <- ctx.Err()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-carrying
		cancel()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the carrier's call ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller's cancellation never reached the carrier")
	}
	if env := h.sink.last(t); env.Outcome != "cancelled" || h.mock.count("b") != 0 {
		t.Fatalf("outcome %s, b=%d", env.Outcome, h.mock.count("b"))
	}
}
