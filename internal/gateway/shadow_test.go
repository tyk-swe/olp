package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestShadowDrainWaitsForAccountingAndStopsNewMirrors(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		s.Routes[routeSlug].Targets[1].Shadow = &runtime.Shadow{SampleRate: 1}
	})
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.mock.set("a", completion(modelA, answerText))
	h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		completion(modelB, answerText)(w, r)
	})
	if resp, body := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s", resp.Status, body)
	}
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- h.gateway.DrainShadows(ctx) }()
	select {
	case err := <-drained:
		t.Fatalf("drained before shadow finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release <- struct{}{}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	h.sink.mu.Lock()
	envs := append([]Envelope(nil), h.sink.envs...)
	h.sink.mu.Unlock()
	if len(envs) != 2 || envs[1].Origin != usage.OriginShadow || !envs[1].Attempts[0].UsageObserved {
		t.Fatalf("shadow accounting was not drained: %+v", envs)
	}
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("caller after drain: %s", resp.Status)
	}
	h.gateway.WaitShadows()
	if h.mock.count("b") != 1 {
		t.Fatal("accepted a new mirror after shutdown started")
	}
}

func TestShadowDrainCancelsAtTheShutdownDeadline(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		s.Routes[routeSlug].Targets[1].Shadow = &runtime.Shadow{SampleRate: 1}
	})
	started, cancelled := make(chan struct{}), make(chan struct{})
	h.mock.set("a", completion(modelA, answerText))
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	h.mock.set("b", func(_ http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
		close(cancelled)
	})
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("caller: %s", resp.Status)
	}
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := h.gateway.DrainShadows(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown deadline did not cancel mirror dispatch")
	}
	h.gateway.WaitShadows()
}
