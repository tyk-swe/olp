package export

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
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

func TestCaptureResolveHonoursScopeAndSampling(t *testing.T) {
	project := "01900000-0000-7000-8000-000000000002"
	route := "team-chat"
	m := testManager(CapturePolicy{ID: "01900000-0000-7000-8000-000000000001", ProjectID: &project, Route: &route, SampleRatio: "1", Include: []string{"output"}, MaxBytes: 1048576, Enabled: true, ETag: "e"})
	if m.Resolve(project, route, "", "", "req-1") == nil {
		t.Fatal("matching request was not sampled")
	}
	if m.Resolve("other-project", route, "", "", "req-1") != nil {
		t.Fatal("foreign project resolved")
	}
	if m.Resolve(project, "other-route", "", "", "req-1") != nil {
		t.Fatal("foreign route resolved")
	}
	m = testManager(CapturePolicy{ID: "01900000-0000-7000-8000-000000000001", SampleRatio: "0", Include: []string{"output"}, MaxBytes: 1048576, Enabled: true, ETag: "e"})
	if m.Resolve(project, route, "", "", "req-1") != nil {
		t.Fatal("zero ratio sampled")
	}
	m.loaded = false
	if m.Resolve(project, route, "", "", "req-1") != nil {
		t.Fatal("unloaded cache resolved")
	}
	m.loaded, m.loadedAt = true, time.Now().Add(-time.Minute)
	if m.Resolve(project, route, "", "", "req-1") != nil {
		t.Fatal("stale cache resolved")
	}
}

func TestCaptureCollectorBounds(t *testing.T) {
	m := testManager(sampledPolicy("1"))
	c := m.Resolve("", "", "", "", "req-1")
	if c == nil {
		t.Fatal("not sampled")
	}
	oversize := strings.Repeat("x", captureFieldMax+1)
	c.CaptureDocument(map[string]json.RawMessage{"messages": json.RawMessage(oversize)})
	if !c.oversized {
		t.Fatal("oversized input field accepted")
	}
	if c.Finish(CaptureIdentity{Route: "r"}, time.Now()) {
		t.Fatal("oversized capture queued")
	}
	drainQueue(m)
	c = m.Resolve("", "", "", "", "req-2")
	c.CaptureText("output", strings.Repeat("y", captureFieldMax))
	c.CaptureText("output", "x")
	if !c.oversized {
		t.Fatal("accumulated output beyond the field bound accepted")
	}
	c.Close()
}

func TestCaptureQueueAndMinuteBounds(t *testing.T) {
	m := testManager(sampledPolicy("1"))
	project := "01900000-0000-7000-8000-000000000002"
	for i := 0; i < captureQueueSize; i++ {
		c := m.Resolve("", "", "", "", "req-"+string(rune(i)))
		if c == nil {
			t.Fatalf("resolve %d", i)
		}
		record, _ := json.Marshal(map[string]any{"request_id": "req"})
		if !c.enqueue(project, record, time.Now()) {
			t.Fatalf("enqueue %d", i)
		}
	}
	over := &Collector{manager: m, policyID: "p", etag: "e", requestID: "over", maxBytes: 1048576}
	if over.enqueue(project, []byte(`{}`), time.Now()) {
		t.Fatal("full queue accepted a record")
	}
	m.mu.Lock()
	charged := m.minute[project].bytes
	m.mu.Unlock()
	record, _ := json.Marshal(map[string]any{"request_id": "req"})
	if charged != captureQueueSize*len(record) {
		t.Fatalf("queue-full drop was still charged: %d", charged)
	}
	drainQueue(m)
	before := time.Now()
	chunk := make([]byte, 900<<10)
	for i := range 4 {
		c := &Collector{manager: m, policyID: "p", etag: "e", requestID: "min", maxBytes: 1048576}
		if !c.enqueue(project, chunk, before) {
			t.Fatalf("minute charge %d", i)
		}
	}
	c := &Collector{manager: m, policyID: "p", etag: "e", requestID: "min5", maxBytes: 1048576}
	if c.enqueue(project, chunk, before) {
		t.Fatal("project minute cap exceeded")
	}
	if !c.enqueue(project, chunk, before.Add(time.Minute+time.Second)) {
		t.Fatal("expired minute window did not reset")
	}
}

func TestCaptureMinuteLedgerUnassignedAndFlood(t *testing.T) {
	m := testManager(sampledPolicy("1"))
	now := time.Now()
	chunk := make([]byte, 900<<10)
	for i := range 4 {
		c := &Collector{manager: m, policyID: "p", etag: "e", requestID: "u", maxBytes: 1048576}
		if !c.enqueue("", chunk, now) {
			t.Fatalf("unassigned charge %d", i)
		}
	}
	c := &Collector{manager: m, policyID: "p", etag: "e", requestID: "u5", maxBytes: 1048576}
	if c.enqueue("", chunk, now) {
		t.Fatal("unassigned project escaped the minute cap")
	}
	m.mu.Lock()
	for i := 0; i < captureMaxProjects; i++ {
		m.minute["proj-"+string(rune(i+0x100))] = minuteCharge{window: now, bytes: 1}
	}
	m.mu.Unlock()
	c = &Collector{manager: m, policyID: "p", etag: "e", requestID: "new", maxBytes: 1048576}
	if c.enqueue("proj-unknown", []byte(`{}`), now) {
		t.Fatal("unknown project admitted past the ledger bound")
	}
	m.mu.Lock()
	m.pruneMinuteLocked(now.Add(2 * time.Minute))
	m.mu.Unlock()
	if !c.enqueue("proj-unknown", []byte(`{}`), now.Add(2*time.Minute)) {
		t.Fatal("expired ledger entries were not reclaimed")
	}
}

func TestCaptureCollectorsBounded(t *testing.T) {
	m := testManager(sampledPolicy("1"))
	var hold []*Collector
	for i := 0; i < captureMaxCollectors; i++ {
		c := m.Resolve("", "", "", "", "req-"+string(rune(i)))
		if c == nil {
			t.Fatalf("collector %d refused", i)
		}
		hold = append(hold, c)
	}
	if m.Resolve("", "", "", "", "req-extra") != nil {
		t.Fatal("collector bound exceeded")
	}
	for _, c := range hold {
		c.Close()
	}
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

func TestCaptureToolCallSnapshot(t *testing.T) {
	m := testManager(sampledPolicy("1"))
	c := m.Resolve("", "", "", "", "req-mut")
	if c == nil {
		t.Fatal("not sampled")
	}
	raw := json.RawMessage(`{"function":{"name":"orig"}}`)
	c.CaptureToolCall(raw)
	for i := range raw {
		raw[i] = 'x'
	}
	if !c.Finish(CaptureIdentity{Route: "r"}, time.Now()) {
		t.Fatal("not queued")
	}
	var record queuedCapture
	select {
	case record = <-m.queue:
	default:
		t.Fatal("queue empty")
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(record.record, &env); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ToolCalls []json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.ToolCalls) != 1 || string(payload.ToolCalls[0]) != `{"function":{"name":"orig"}}` {
		t.Fatalf("queued tool call mutated by caller: %v", payload.ToolCalls)
	}
}
