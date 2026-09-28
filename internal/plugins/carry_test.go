package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// carrierFixture is a host whose only plugin is the carrier plugin, built
// natively to carry its profile's traffic to upstream, and the plugin's
// digest.
func carrierFixture(t *testing.T, upstream string, limits Limits) (*Host, string) {
	t.Helper()
	dir := t.TempDir()
	testutil.BuildExecutablePlugin(t, filepath.Join(dir, "carrier"), "./internal/plugins/testdata/carrier", "-X=main.upstream="+upstream+"/v1")
	u := NewUnconfined(dir, limits, slog.New(slog.DiscardHandler))
	files, err := u.Executables()
	if err != nil || len(files) != 1 {
		t.Fatalf("executables %+v: %v", files, err)
	}
	return newUnconfinedHost(t, u, files[0]), files[0].Digest
}

// carriedCredential authorizes the requests the tests carry.
const carriedCredential = "sk-carried"

// carry hands the plugin with digest a chat request saying prompt to carry to
// upstream.
func carry(ctx context.Context, host *Host, digest, upstream, prompt string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"`+prompt+`"}]}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+carriedCredential)
	req.Header.Set("Content-Type", "application/json")
	return host.Carry(ctx, digest, abi.Provider{Profile: "carrier-chat"}, req, []string{carriedCredential})
}

// prompted reports whether an upstream request says prompt, reading its body.
func prompted(r *http.Request, prompt string) bool {
	body, _ := io.ReadAll(r.Body)
	return strings.Contains(string(body), prompt)
}

// An unconfined plugin carries a request to the upstream and streams the
// response back as it arrives: each event reaches OLP before the upstream
// sends the next, however long the stream outlasts the time limit. An
// unsuccessful response comes back as the upstream sent it.
func TestUnconfinedPluginCarriesAStreamAsItArrives(t *testing.T) {
	t.Parallel()
	next := make(chan struct{})
	received := make(chan http.Header, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		if prompted(r, "limited") {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-next:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	limits := DefaultLimits
	limits.Time = time.Second
	host, digest := carrierFixture(t, upstream.URL, limits)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	resp, err := carry(ctx, host, digest, upstream.URL, "stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if header := <-received; header.Get("X-Carried-By") != "carrier" || header.Get("Authorization") != "Bearer "+carriedCredential {
		t.Fatalf("the upstream received %v", header)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("carried %d %v", resp.StatusCode, resp.Header)
	}
	events := bufio.NewReader(resp.Body)
	if first, err := events.ReadString('\n'); err != nil || first != "data: {\"n\":1}\n" {
		t.Fatalf("first event %q: %v", first, err)
	}
	time.Sleep(1500 * time.Millisecond)
	close(next)
	if rest, err := io.ReadAll(events); err != nil || string(rest) != "\ndata: [DONE]\n\n" {
		t.Fatalf("rest of the stream %q: %v", rest, err)
	}

	resp, err = carry(ctx, host, digest, upstream.URL, "limited")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "7" || string(body) != `{"error":{"message":"slow down"}}` || err != nil {
		t.Fatalf("carried %d %v %s: %v", resp.StatusCode, resp.Header, body, err)
	}
}

// A request the plugin reports not sent, or OLP never hands it, fails as not
// sent; any other failure leaves the upstream's outcome unknown.
func TestCarryReportsWhetherARequestWasSent(t *testing.T) {
	t.Parallel()
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		// Closing each connection makes the plugin dial every request, so the
		// closed upstream refuses the last one. A pooled connection could
		// otherwise carry it before the plugin noticed the upstream closing
		// that connection, leaving the request's outcome unknown.
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
	}))
	host, digest := carrierFixture(t, upstream.URL, DefaultLimits)

	if _, err := carry(t.Context(), host, digest, upstream.URL, "carrier:not-sent"); !errors.Is(err, connectors.ErrNotSent) || reached.Load() != 0 {
		t.Fatalf("a request the plugin did not send failed with %v, reaching the upstream %d times", err, reached.Load())
	}
	_, err := carry(t.Context(), host, digest, upstream.URL, "carrier:fail")
	if reported, ok := errors.AsType[*abi.Error](err); !ok || reported.Code != "carrier_failed" || errors.Is(err, connectors.ErrNotSent) || reached.Load() != 1 {
		t.Fatalf("a request the plugin failed after sending failed with %v", err)
	}
	if _, err = carry(t.Context(), host, strings.Repeat("0", 64), upstream.URL, "hi"); !errors.Is(err, connectors.ErrNotSent) || !isCode(err, CodeExecutableChanged) {
		t.Fatalf("a request for a plugin OLP can't run failed with %v", err)
	}
	upstream.Close()
	if _, err = carry(t.Context(), host, digest, upstream.URL, "hi"); !errors.Is(err, connectors.ErrNotSent) {
		t.Fatalf("a request whose connection failed failed with %v", err)
	}
}

// When the caller stops waiting, before the response or while it streams,
// the cancellation reaches the plugin, which stops its request upstream.
func TestCarryCancellationReachesTheUpstream(t *testing.T) {
	t.Parallel()
	arrived, cancelled := make(chan struct{}, 1), make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body also lets the server notice a closed connection.
		streaming := prompted(r, "streaming")
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
		}
		arrived <- struct{}{}
		<-r.Context().Done()
		cancelled <- streaming
	}))
	defer upstream.Close()
	host, digest := carrierFixture(t, upstream.URL, DefaultLimits)
	reachesUpstream := func(streaming bool) {
		t.Helper()
		select {
		case got := <-cancelled:
			if got != streaming {
				t.Fatalf("the upstream saw another request cancelled")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the cancellation never reached the upstream")
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	failed := make(chan error, 1)
	go func() {
		_, err := carry(ctx, host, digest, upstream.URL, "waiting")
		failed <- err
	}()
	<-arrived
	cancel()
	if err := <-failed; !errors.Is(err, context.Canceled) || errors.Is(err, connectors.ErrNotSent) {
		t.Fatalf("a cancelled request failed with %v", err)
	}
	reachesUpstream(false)

	resp, err := carry(t.Context(), host, digest, upstream.URL, "streaming")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("carried %v: %v", resp, err)
	}
	<-arrived
	resp.Body.Close()
	reachesUpstream(true)
}

// A plugin that leaves a cancelled carried call unanswered past the time limit
// may be stuck, so OLP stops it, failing the calls in flight on it.
func TestAPluginThatIgnoresACancelledCarryIsStopped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// It answers its first call's head, then reads its calls and answers none.
	script := "#!/bin/sh\necho '{\"abi_version\":1}'\nread call\necho '{\"id\":1,\"part\":{\"status\":200}}'\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(dir, "stuck"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits
	limits.Time = time.Second
	u := NewUnconfined(dir, limits, slog.New(slog.DiscardHandler))
	files, err := u.Executables()
	if err != nil {
		t.Fatal(err)
	}
	host := newUnconfinedHost(t, u, files[0])
	resp, err := carry(t.Context(), host, files[0].Digest, "https://api.example.com", "stuck")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("carried %v: %v", resp, err)
	}
	inFlight := make(chan error, 1)
	go func() {
		_, err := carry(t.Context(), host, files[0].Digest, "https://api.example.com", "in flight")
		inFlight <- err
	}()
	resp.Body.Close()
	select {
	case err = <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("the stuck plugin kept running")
	}
	if !isCode(err, CodeFailed) || !strings.Contains(err.Error(), "left a cancelled call unanswered past its 1s time limit") || errors.Is(err, connectors.ErrNotSent) {
		t.Fatalf("a call in flight on the stopped plugin failed with %v", err)
	}
}

// A carrier that stops reading stdin cannot keep a large request, or another
// call waiting to write, alive after cancellation and the watchdog's limit.
func TestCarryCancellationInterruptsABlockedRequestWrite(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits
	limits.Time = time.Second
	u := NewUnconfined(t.TempDir(), limits, slog.New(slog.DiscardHandler))
	file := script(t, u, "unread", `echo '{"abi_version":1}'
read call
echo '{"id":1,"response":{"result":{}}}'
dd bs=1 count=1 of=/dev/null 2>/dev/null
touch writing
exec sleep 60`)
	host := newUnconfinedHost(t, u, file)
	if _, err := sign(t, host, file.Digest, "sk-fixture"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 2)
	for range 2 {
		go func() {
			resp, err := carry(ctx, host, file.Digest, "https://api.example.com", strings.Repeat("x", 1<<20))
			if resp != nil {
				resp.Body.Close()
			}
			finished <- err
		}()
	}
	// The carrier reads one byte of the first request, then leaves the rest
	// unread. Cancel only once the large write has actually reached stdin.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(u.dir, "writing")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the carried request never reached the plugin's stdin")
		}
	}
	cancel()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for range 2 {
		select {
		case err := <-finished:
			if !errors.Is(err, connectors.ErrNotSent) {
				t.Fatalf("a request the plugin never read failed with %v", err)
			}
		case <-timeout.C:
			t.Fatal("a blocked request write retained a cancelled carried call")
		}
	}
}

// OLP holds a streamed result's parts for its reader without holding up the
// plugin, but not without bound: a reader too far behind loses the result
// after the parts it holds.
func TestAReaderTooFarBehindLosesItsStreamedResult(t *testing.T) {
	t.Parallel()
	result := &streamed{out: newOutput(slog.New(slog.DiscardHandler), nil), arrived: make(chan struct{}, 1)}
	part := json.RawMessage(`"` + strings.Repeat("a", 1<<20-2) + `"`)
	for range maxHeld >> 20 {
		if !result.deliver(part) {
			t.Fatal("a part within the bound was refused")
		}
	}
	if result.deliver(part) {
		t.Fatal("a part beyond the bound was held")
	}
	for range maxHeld >> 20 {
		if got, err := result.next(); err != nil || len(got) != len(part) {
			t.Fatalf("held part %d bytes: %v", len(got), err)
		}
	}
	if _, err := result.next(); !errors.Is(err, errBehind) {
		t.Fatalf("the result ended with %v", err)
	}
}
