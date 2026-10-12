//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type retainedBufferedFlushFailure struct {
	*httptest.ResponseRecorder
	flushes int
}

func (*retainedBufferedFlushFailure) SetWriteDeadline(time.Time) error { return nil }
func (w *retainedBufferedFlushFailure) FlushError() error {
	w.flushes++
	return io.ErrClosedPipe
}

func TestRetainedResponseFailedFirstFlushDoesNotReportDelivery(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newOpenAIFixture(t, "")
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"}, nil, map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"retained output","store":true}`), map[string]string{"Content-Type": "application/json"})
	var response struct{ ID string }
	if status != http.StatusOK || json.Unmarshal(raw, &response) != nil || response.ID == "" {
		t.Fatalf("create: %d %s", status, raw)
	}
	fixture.respGetStream.Store("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-up-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"" + vendorModel + "\",\"output\":[]}}\n\n")
	sink := &captureSink{}
	h.Gateway.Sink = sink
	request := httptest.NewRequest(http.MethodGet, "/v1/responses/"+response.ID+"?stream=true", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	writer := &retainedBufferedFlushFailure{ResponseRecorder: httptest.NewRecorder()}
	h.HTTP.Config.Handler.ServeHTTP(writer, request)
	if writer.flushes != 1 || !strings.Contains(writer.Body.String(), `"id":"`+response.ID+`"`) {
		t.Fatalf("did not exercise retained event delivery: flushes=%d body=%s", writer.flushes, writer.Body.String())
	}
	envelope := sink.last()
	if envelope.Outcome != "cancelled" || !envelope.Committed || len(envelope.Attempts) != 1 {
		t.Fatalf("failed delivery became successful retrieval: %+v", envelope)
	}
	if envelope.FirstByte != nil {
		t.Fatalf("failed first flush reported delivered output after %v", *envelope.FirstByte)
	}
}

func TestRealtimeUpgradeWithoutProviderFramesDoesNotReportPayloadDelivery(t *testing.T) {
	testRealtimePayloadDelivery(t, false)
}

func TestRealtimeNativeFrameRecordsPayloadDelivery(t *testing.T) {
	testRealtimePayloadDelivery(t, true)
}

func testRealtimePayloadDelivery(t *testing.T, send bool) {
	t.Helper()
	h := newAccessHarness(t)
	fixture := newStrictRealtimeFixture(t, "openai")
	slug, key := provisionStrictRealtime(t, h, "openai", fixture.URL+"/v1")
	sink := &captureSink{}
	h.Gateway.Sink = sink
	before := fixture.dials.Load()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, strings.Replace(h.HTTP.URL, "http://", "ws://", 1)+"/v1/realtime?model="+slug,
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	glEventually(t, "upstream realtime upgrade", func() bool { return fixture.dials.Load() > before })
	if send {
		if err = connection.Write(ctx, websocket.MessageText, strictRealtimeClientFrames[0]); err != nil {
			t.Fatal(err)
		}
		kind, frame, readErr := connection.Read(ctx)
		if readErr != nil || kind != websocket.MessageText || !bytes.Equal(frame, strictRealtimeProviderFrames[0]) {
			t.Fatalf("native session frame: %q %v", frame, readErr)
		}
	}
	// The provider waits for a client frame before producing any native output.
	// Closing without sending a frame leaves both sockets upgraded but silent.
	_ = connection.Close(websocket.StatusNormalClosure, "")
	glEventually(t, "silent realtime terminal envelope", func() bool { return sink.count() > 0 })
	envelope := sink.last()
	if len(envelope.Attempts) != 1 || !envelope.Committed {
		t.Fatalf("did not exercise the accepted native session: %+v", envelope)
	}
	if (envelope.FirstByte != nil) != send {
		t.Fatalf("payload delivery send=%t firstByte=%v", send, envelope.FirstByte)
	}
}

type retainedReplayWriteFailure struct {
	*httptest.ResponseRecorder
	failure  string
	deadline bool
}

func (w *retainedReplayWriteFailure) SetWriteDeadline(deadline time.Time) error {
	w.deadline = !deadline.IsZero()
	return nil
}
func (*retainedReplayWriteFailure) SetReadDeadline(time.Time) error { return nil }
func (w *retainedReplayWriteFailure) Write(data []byte) (int, error) {
	if !w.deadline || w.failure == "write" {
		return 0, io.ErrClosedPipe
	}
	if w.failure == "short" {
		return len(data) - 1, nil
	}
	return w.ResponseRecorder.Write(data)
}
func (w *retainedReplayWriteFailure) FlushError() error {
	if w.failure == "flush" {
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestContinuationReplayDeliveryFailureDoesNotReportSuccess(t *testing.T) {
	h := newAccessHarness(t)
	wire, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(wire)
	})
	body := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := continuationHeaders()
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/chat/completions", key, strings.NewReader(body), headers)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"ready":true`)) {
		t.Fatalf("initial durable delivery: %d %s", status, raw)
	}
	var handle string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame struct{ OLP struct{ Handle string } }
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.OLP.Handle != "" {
			handle = frame.OLP.Handle
		}
	}
	if handle == "" {
		t.Fatal("initial delivery omitted the owned recovery handle")
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for _, failure := range []string{"write", "flush", "short", ""} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				sink := &captureSink{}
				h.Gateway.Sink = sink
				path, input := "/v1/chat/completions", body
				if method == http.MethodGet {
					path, input = "/v1/continuations/"+handle, ""
				}
				request := httptest.NewRequest(method, path, strings.NewReader(input))
				request.Header.Set("Authorization", "Bearer "+key)
				for name, value := range headers {
					request.Header.Set(name, value)
				}
				writer := &retainedReplayWriteFailure{ResponseRecorder: httptest.NewRecorder(), failure: failure}
				h.HTTP.Config.Handler.ServeHTTP(writer, request)
				envelope := sink.last()
				if calls.Load() != 1 || len(envelope.Attempts) != 0 || writer.Code != http.StatusOK {
					t.Fatalf("replay dispatched provider work: calls=%d envelope=%+v headers=%v", calls.Load(), envelope, writer.Header())
				}
				if method == http.MethodPost && writer.Header().Get("X-OLP-Delivery-Replay") != "true" {
					t.Fatal("did not reach cached replay")
				}
				wantOutcome := "cancelled"
				if failure == "" {
					wantOutcome = "success"
				}
				if envelope.Outcome != wantOutcome || !envelope.Committed || (envelope.FirstByte != nil) != (failure == "") || !writer.deadline {
					t.Fatalf("failed replay write reported success: %+v", envelope)
				}
				if failure == "" {
					if method == http.MethodPost && !bytes.Equal(writer.Body.Bytes(), raw) {
						t.Fatal("cached native delivery changed")
					}
					if method == http.MethodGet && !bytes.Contains(writer.Body.Bytes(), []byte(`"handle":"`+handle+`"`)) {
						t.Fatal("recovered owned handle changed")
					}
				}
			})
		}
	}
}
