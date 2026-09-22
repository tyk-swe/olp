//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestContinuationConnectionLossDoesNotInventAcceptedWork(t *testing.T) {
	h := newAccessHarness(t)
	events, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	var calls atomic.Int64
	slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		// A provider has accepted this request and supplied enough non-actionable
		// events for the client to observe a live stream. No tool is complete.
		prefix := bytes.SplitN(events, []byte("event: content_block_stop"), 2)[0]
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(prefix)
		_ = http.NewResponseController(w).Flush()
		accepted <- struct{}{}
		<-r.Context().Done()
	})
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	beforeSend := continuationHeaders()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, "POST", h.HTTP.URL+"/v1/chat/completions", strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	req.Header.Set("Authorization", "Bearer "+key)
	for name, value := range beforeSend {
		req.Header.Set(name, value)
	}
	if response, err := http.DefaultClient.Do(req); err == nil {
		response.Body.Close()
		t.Fatal("canceled request unexpectedly sent")
	}
	var reserved int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources WHERE submission_id=$1`, beforeSend["X-OLP-Submission-ID"]).Scan(&reserved); err != nil || reserved != 0 || calls.Load() != 0 {
		t.Fatalf("pre-send loss reserved work: rows=%d calls=%d err=%v", reserved, calls.Load(), err)
	}
	partial := continuationHeaders()
	origin, err := url.Parse(h.HTTP.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", origin.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fragment := []byte(source[:len(source)/2])
	_, err = fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nX-OLP-Continuation: %s\r\nX-OLP-Submission-ID: %s\r\nContent-Length: %d\r\n\r\n%s", origin.Host, key, continuationClientVersion, partial["X-OLP-Submission-ID"], len(source), fragment)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources WHERE submission_id=$1`, partial["X-OLP-Submission-ID"]).Scan(&reserved); err != nil || reserved != 0 || calls.Load() != 0 {
		t.Fatalf("incomplete ingress upload reserved work: rows=%d calls=%d err=%v", reserved, calls.Load(), err)
	}
	// Once the provider accepts work, cancellation cannot turn a retry into a
	// fresh inference even if the caller saw only early reasoning observations.
	headers := continuationHeaders()
	ctx, cancel = context.WithCancel(t.Context())
	req, err = http.NewRequestWithContext(ctx, "POST", h.HTTP.URL+"/v1/chat/completions", strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	clientResponse := make(chan *http.Response, 1)
	clientError := make(chan error, 1)
	go func() {
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			clientError <- e
		} else {
			clientResponse <- resp
		}
	}()
	select {
	case <-accepted:
	case e := <-clientError:
		t.Fatal(e)
	case <-time.After(5 * time.Second):
		t.Fatal("provider acceptance was not observed")
	}
	cancel()
	select {
	case response := <-clientResponse:
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if bytes.Contains(body, []byte(`"tool_calls"`)) {
			t.Fatal("tool escaped an incomplete canceled delivery")
		}
	case <-clientError:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled client did not terminate")
	}
	status, retry, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 409 || !bytes.Contains(retry, []byte("continuation_outcome_unknown")) || calls.Load() != 1 {
		t.Fatalf("accepted canceled work repeated: status=%d calls=%d %s", status, calls.Load(), retry)
	}
	status, recovered, _ := h.gatewayRaw("GET", "/v1/continuation-submissions/"+headers["X-OLP-Submission-ID"], key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 409 || !bytes.Contains(recovered, []byte("continuation_outcome_unknown")) || calls.Load() != 1 {
		t.Fatalf("incomplete canceled work became ready: %d calls=%d %s", status, calls.Load(), recovered)
	}
}
