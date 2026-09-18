package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
)

type mediaSinkFunc func(Envelope)

func (f mediaSinkFunc) Terminal(e Envelope) { f(e) }

func TestMediaStreamCancellationAndDeadline(t *testing.T) {
	for _, cause := range []string{"client cancellation", "attempt deadline", "deadline before first event"} {
		t.Run(cause, func(t *testing.T) {
			h := newMediaHarness(t)
			if cause != "client cancellation" {
				route := h.rt.release.Snapshot.Routes[routeSlug]
				route.Targets[0].Timeout = 100
				h.rt.release.Snapshot.Routes[routeSlug] = route
			}
			ended := make(chan Envelope, 1)
			h.gateway.Sink = mediaSinkFunc(func(e Envelope) { ended <- e })
			cancelled := make(chan struct{})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				if cause != "deadline before first event" {
					io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\"}\n\n")
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(cancelled)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			resp := h.do(ctx, "POST", "/v1/images/generations", fullKey,
				[]byte(`{"model":"team-chat","prompt":"photo","stream":true}`), nil)
			if cause == "client cancellation" {
				if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
					t.Fatal(err)
				}
				cancel()
			} else {
				io.Copy(io.Discard, resp.Body)
			}
			resp.Body.Close()
			select {
			case <-cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream stream survived cancellation/deadline")
			}
			select {
			case env := <-ended:
				if env.Outcome == "success" || !env.Attempts[0].BillingUncertain {
					t.Fatalf("failed stream evidence: %+v", env)
				}
				if cause == "client cancellation" && env.Outcome != "cancelled" {
					t.Fatalf("cancellation outcome: %+v", env)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stream never finalized")
			}
			if h.mock.count("b") != 0 {
				t.Fatal("accepted stream was retried after cancellation/deadline")
			}
		})
	}
}

func TestMediaAmbiguousFailureDoesNotFailOver(t *testing.T) {
	h := newMediaHarness(t)
	h.mock.set("a", status(500, `{"error":{"message":"generation failed after acceptance"}}`))
	resp := h.do(t.Context(), "POST", "/v1/images/generations", fullKey,
		[]byte(`{"model":"team-chat","prompt":"photo"}`), nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 || h.mock.count("b") != 0 || !h.sink.last(t).Attempts[0].BillingUncertain {
		t.Fatalf("ambiguous work retried or treated as free: status=%d, calls=%d", resp.StatusCode, h.mock.count("b"))
	}
}

type failedMediaWriter struct{ header http.Header }

func (w *failedMediaWriter) Header() http.Header              { return w.header }
func (w *failedMediaWriter) WriteHeader(int)                  {}
func (w *failedMediaWriter) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (w *failedMediaWriter) Flush()                           {}
func (w *failedMediaWriter) SetReadDeadline(time.Time) error  { return nil }
func (w *failedMediaWriter) SetWriteDeadline(time.Time) error { return nil }

func TestMediaImageDeliveryFailureCleansEveryArtifact(t *testing.T) {
	h := newMediaHarness(t)
	h.mock.set("a", status(200, `{"data":[{"b64_json":"AQID"},{"b64_json":"BAUG"}]}`))
	req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"team-chat","prompt":"photo"}`))
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	h.gateway.imageGenerations(&failedMediaWriter{header: make(http.Header)}, req)
	if used := h.gateway.transport().Spool.UsedBytes(); used != 0 {
		t.Fatalf("failed delivery leaked %d spool bytes", used)
	}
	if h.sink.last(t).Outcome != "cancelled" {
		t.Fatal("failed image delivery recorded as successful")
	}
}

func TestMediaStreamHoldsTargetConcurrencyUntilCompletion(t *testing.T) {
	for _, scope := range []string{"provider", "credential"} {
		t.Run(scope, func(t *testing.T) {
			limiter := mediaLimiter(t)
			h := newMediaHarness(t)
			h.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, h.gateway.log)
			var request limits.Request
			for id, p := range h.rt.release.Snapshot.Providers {
				if p.Name != "a" {
					continue
				}
				one := int64(1)
				if scope == "provider" {
					p.Limits = &runtime.Limits{MaxConcurrency: &one}
					request = connectionRequest(&p, 0, time.Second)
				} else {
					p.Slots[0].MaxConcurrency = &one
					request = slotRequest(&p.Slots[0], 0, time.Second)
				}
				h.rt.release.Snapshot.Providers[id] = p
			}
			finish := make(chan struct{})
			defer func() {
				select {
				case <-finish:
				default:
					close(finish)
				}
			}()
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\"}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-finish:
					io.WriteString(w, "data: {\"type\":\"image_generation.completed\"}\n\n")
				case <-r.Context().Done():
				}
			})
			resp := h.do(t.Context(), "POST", "/v1/images/generations", fullKey,
				[]byte(`{"model":"team-chat","prompt":"photo","stream":true}`), nil)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			_, err := limiter.Reserve(t.Context(), request)
			var exceeded *limits.ExceededError
			if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionConcurrency {
				t.Fatalf("%s concurrency released at response headers: %v", scope, err)
			}
			close(finish)
			io.Copy(io.Discard, reader)
			lease, err := limiter.Reserve(t.Context(), request)
			if err != nil {
				t.Fatalf("%s concurrency not released after completion: %v", scope, err)
			}
			if err := lease.Refund(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
