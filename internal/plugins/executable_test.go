package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/plugin/abi"
)

type capabilityFrameWriter struct {
	frames chan abi.Frame
}

func (w capabilityFrameWriter) Write(data []byte) (int, error) {
	var frame abi.Frame
	if json.Unmarshal(data, &frame) == nil {
		w.frames <- frame
	}
	return len(data), nil
}

func (w capabilityFrameWriter) Close() error { return nil }

func TestReceiveBoundsConcurrentCapabilityRequests(t *testing.T) {
	limit := int64(defaultLimits.Instances)
	const total = 20
	var current, maximum atomic.Int64
	arrived := make(chan struct{}, total)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := current.Add(1)
		for {
			seen := maximum.Load()
			if now <= seen || maximum.CompareAndSwap(seen, now) {
				break
			}
		}
		arrived <- struct{}{}
		<-release
		current.Add(-1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(unblock)

	log := slog.New(slog.DiscardHandler)
	grant := Call{HTTP: &HTTP{Origins: []string{server.URL}, Client: server.Client()}}
	ctx := grant.context(context.Background(), newOutput(log, nil))
	responses := make(chan abi.Frame, total)
	p := &process{
		stdin: capabilityFrameWriter{frames: responses},
		log:   log,
		limit: time.Second,
		calls: map[uint64]*pending{1: {ctx: ctx}},
		ended: map[string]time.Time{},
		done:  make(chan struct{}),
	}

	params, err := json.Marshal(abi.HTTPRequest{Method: http.MethodGet, URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	for id := uint64(1); id <= total; id++ {
		frame := abi.Frame{ID: id, Call: 1, Request: &abi.Request{Method: abi.CapabilityHTTP, Params: params}}
		data, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		input.Write(data)
		input.WriteByte('\n')
	}
	received := make(chan string, 1)
	go func() { received <- p.receive(bufio.NewReader(&input)) }()

	wantRefused := total - int(limit)
	refused, serving := 0, 0
	deadline := time.After(3 * time.Second)
	for refused < wantRefused {
		select {
		case <-arrived:
			serving++
			if serving > int(limit) {
				t.Fatalf("got %d concurrent capability requests at the test server, want at most %d", maximum.Load(), limit)
			}
		case frame := <-responses:
			if frame.Response == nil || frame.Response.Error == nil || frame.Response.Error.Code != abi.CodeInternal {
				t.Fatalf("got response %+v while the test server was still blocked, want an %s refusal", frame.Response, abi.CodeInternal)
			}
			refused++
		case <-deadline:
			t.Fatalf("got %d refused and %d serving capability requests, want %d refused and %d serving", refused, serving, wantRefused, limit)
		}
	}
	for serving < int(limit) {
		select {
		case <-arrived:
			serving++
		case <-time.After(3 * time.Second):
			t.Fatalf("got %d serving capability requests, want %d", serving, limit)
		}
	}
	if got := maximum.Load(); got > limit {
		t.Fatalf("got %d concurrent capability requests at the test server, want at most %d", got, limit)
	}
	unblock()
	for answered := 0; answered < int(limit); answered++ {
		select {
		case frame := <-responses:
			if frame.Response == nil || frame.Response.Error != nil {
				t.Fatalf("got response %+v for a serving capability request, want success", frame.Response)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("got %d answers from serving capability requests, want %d", answered, limit)
		}
	}
	if violation := <-received; violation != "" {
		t.Fatalf("receive reported %q, want no violation", violation)
	}
}
