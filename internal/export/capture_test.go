package export

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
)

func drainQueue(m *Manager) {
	for len(m.queue) > 0 {
		<-m.queue
	}
}

func testManager(policies ...CapturePolicy) *Manager {
	m := &Manager{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.policies = policies
	m.loaded = true
	m.loadedAt = time.Now()
	return m
}

func sampledPolicy(ratio string) CapturePolicy {
	return CapturePolicy{ID: "01900000-0000-7000-8000-000000000001", SampleRatio: ratio, Include: []string{"input", "output", "tool_calls"}, MaxBytes: 1048576, Enabled: true, ETag: "etag-1"}
}

func TestCaptureCollectorFinishSemantics(t *testing.T) {
	m := testManager(sampledPolicy("1"))
	c := m.Resolve("", "", "", "", "req-meta")
	if c == nil {
		t.Fatal("not sampled")
	}
	if c.Finish(CaptureIdentity{Route: "r"}, time.Now()) {
		t.Fatal("metadata-only capture queued")
	}
	c = m.Resolve("", "", "", "", "req-twice")
	c.CaptureText("output", "text")
	if !c.Finish(CaptureIdentity{Route: "r"}, time.Now()) {
		t.Fatal("content capture not queued")
	}
	if c.Finish(CaptureIdentity{Route: "r"}, time.Now()) {
		t.Fatal("second Finish queued a duplicate")
	}
	c.CaptureText("output", "after-close")
	c.CaptureDocument(map[string]json.RawMessage{"messages": json.RawMessage(`[]`)})
	m.mu.Lock()
	queued := len(m.queue)
	m.mu.Unlock()
	if queued != 1 {
		t.Fatalf("post-close appends retained: queue %d", queued)
	}
	drainQueue(m)
}
