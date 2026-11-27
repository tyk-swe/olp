package capture

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func testRedactor(t *testing.T, words ...string) *guardrails.Engine {
	t.Helper()
	config, err := json.Marshal(map[string]any{"words": words})
	if err != nil {
		t.Fatal(err)
	}
	revision := guardrails.Revision{
		ID:            "rev-1",
		GuardrailID:   "guard-1",
		ProjectID:     "project-1",
		Type:          "builtin.keywords",
		Configuration: config,
		TimeoutMS:     1000,
		OnError:       guardrailpolicy.FailClosed,
	}
	engine, err := guardrails.Compile(
		[]guardrails.Entry{{
			GuardrailID: revision.GuardrailID,
			RevisionID:  revision.ID,
			Phase:       guardrailpolicy.Input,
			Action:      guardrailpolicy.Redact,
		}},
		map[string]guardrails.Revision{revision.ID: revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestRedactScrubsNestedInputOutputAndToolArguments(t *testing.T) {
	t.Parallel()
	engine := testRedactor(t, "secret-token", "424242")
	input := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"use secret-token and 424242","nested":{"secret-token":"ok","num":424242}},{"type":"function_call","name":"lookup","arguments":"{\"query\":\"secret-token\"}"}]`),
	}
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"tool_use","name":"fetch","arguments":"{\"token\":\"secret-token\"}"}`),
	}
	got, err := redact(t.Context(), engine, content{
		input:  input,
		output: "reply with secret-token",
		tools:  tools,
	})
	if err != nil {
		t.Fatalf("redact returned unexpected error: %v", err)
	}
	if strings.Contains(got.output, "secret-token") {
		t.Errorf("output retained secret: %q", got.output)
	}
	if strings.Contains(string(got.input["messages"]), "secret-token") || strings.Contains(string(got.input["messages"]), "424242") {
		t.Errorf("input retained secret: %s", got.input["messages"])
	}
	if !json.Valid(got.input["messages"]) {
		t.Fatalf("redacted input is not valid JSON: %s", got.input["messages"])
	}
	if len(got.tools) != 1 || strings.Contains(string(got.tools[0]), "secret-token") {
		t.Fatalf("tool call retained secret: %s", got.tools)
	}
	var toolCall map[string]any
	if err := json.Unmarshal(got.tools[0], &toolCall); err != nil {
		t.Fatalf("redacted tool call is not valid JSON: %v", err)
	}
	encodedArgs, ok := toolCall["arguments"].(string)
	if !ok || !json.Valid([]byte(encodedArgs)) {
		t.Fatalf("redacted tool arguments did not remain a JSON-encoded string: %#v", toolCall["arguments"])
	}
}

func TestRedactFailsClosedOnUnsafeStructuresAndErrors(t *testing.T) {
	t.Parallel()
	engine := testRedactor(t, "dup-a", "dup-b")
	deep := strings.Repeat(`{"a":`, maxJSONDepth+2) + `"leaf"` + strings.Repeat(`}`, maxJSONDepth+2)
	oversized := `"` + strings.Repeat("x", fieldMax+1) + `"`

	cases := map[string]content{
		"opaque image_url type": {
			input: map[string]json.RawMessage{"messages": json.RawMessage(`[{"type":"image_url","url":"https://example.test/a.png"}]`)},
		},
		"opaque inlineData field": {
			input: map[string]json.RawMessage{"contents": json.RawMessage(`[{"inlineData":{"mimeType":"image/png","data":"abc"}}]`)},
		},
		"opaque fileData field": {
			input: map[string]json.RawMessage{"contents": json.RawMessage(`[{"fileData":{"fileUri":"gs://bucket/a"}}]`)},
		},
		"key collision after redaction": {
			input: map[string]json.RawMessage{"messages": json.RawMessage(`{"dup-a":"first","dup-b":"second"}`)},
		},
		"scalar inside tool call context": {
			tools: []json.RawMessage{json.RawMessage(`"not-an-object"`)},
		},
		"malformed json inside tool arguments": {
			tools: []json.RawMessage{json.RawMessage(`{"type":"function_call","arguments":"{broken"}`)},
		},
		"nesting exceeds maxJSONDepth": {
			input: map[string]json.RawMessage{"messages": json.RawMessage(deep)},
		},
		"invalid utf8 in output": {
			output: string([]byte{0xff, 0xfe}),
		},
		"oversized output exceeds fieldMax": {
			output: strings.Repeat("a", fieldMax+1),
		},
		"oversized input exceeds fieldMax": {
			input: map[string]json.RawMessage{"messages": json.RawMessage(oversized)},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := redact(t.Context(), engine, tc); !errors.Is(err, errRedaction) {
				t.Fatalf("redact = %v, want %v", err, errRedaction)
			}
		})
	}

	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := redact(ctx, engine, content{output: "hello"})
		if !errors.Is(err, errRedaction) {
			t.Fatalf("cancelled redact = %v, want %v", err, errRedaction)
		}
	})
}

func TestCollectorLifecycleSpoilageAndLimits(t *testing.T) {
	t.Parallel()
	newCollector := func(include []Include, maxBytes int, redactor *guardrails.Engine) (*Manager, *Collector) {
		project := "project-1"
		m := NewManager(nil, nil, "", nil, nil)
		var r redaction = unredacted{}
		if redactor != nil {
			r = pinned{engine: redactor, project: project}
		}
		m.cache.Store(&snapshot{
			at: time.Now(),
			policies: []active{{
				Policy: Policy{
					ID:       "policy-1",
					Settings: Settings{ProjectID: &project, MaxBytes: maxBytes, Include: include},
				},
				ratio:     big.NewRat(1, 1),
				redaction: r,
			}},
		})
		c := m.Resolve(project, "route-1", "", "", "req-1")
		if c == nil {
			t.Fatal("Resolve returned nil collector")
		}
		return m, c
	}

	t.Run("failed stream spoils input-only capture", func(t *testing.T) {
		t.Parallel()
		m, c := newCollector([]Include{Input}, 4096, nil)
		c.CaptureDocument(map[string]json.RawMessage{"prompt": json.RawMessage(`"hello"`)})
		if c.Finish(t.Context(), Identity{ProjectID: "project-1", Outcome: "failure", Mode: Streaming}, time.Now()) {
			t.Fatal("failed stream was queued")
		}
		if len(m.queue) != 0 || m.dropped.Load() != 1 || m.collectors.Load() != 0 {
			t.Fatalf("queue=%d dropped=%d collectors=%d", len(m.queue), m.dropped.Load(), m.collectors.Load())
		}
	})

	t.Run("empty capture drops without queueing and second Finish is ignored", func(t *testing.T) {
		t.Parallel()
		m, c := newCollector([]Include{Output}, 4096, nil)
		if c.Finish(t.Context(), Identity{ProjectID: "project-1", Outcome: "success", Mode: Unary}, time.Now()) {
			t.Fatal("empty capture was queued")
		}
		if m.dropped.Load() != 1 {
			t.Fatalf("dropped = %d, want 1", m.dropped.Load())
		}
		if c.Finish(t.Context(), Identity{ProjectID: "project-1", Outcome: "success", Mode: Unary}, time.Now()) {
			t.Fatal("second Finish succeeded")
		}
		if m.dropped.Load() != 1 || m.collectors.Load() != 0 {
			t.Fatalf("second Finish changed counters: dropped=%d collectors=%d", m.dropped.Load(), m.collectors.Load())
		}
	})

	t.Run("oversized input spoils collector and redaction failure counts separately", func(t *testing.T) {
		t.Parallel()
		engine := testRedactor(t, "secret")
		m, c := newCollector([]Include{Input}, 4096, engine)
		c.CaptureDocument(map[string]json.RawMessage{"prompt": json.RawMessage(`"` + strings.Repeat("a", fieldMax+1) + `"`)})
		if c.Finish(t.Context(), Identity{ProjectID: "project-1", Outcome: "success", Mode: Unary}, time.Now()) {
			t.Fatal("spoiled capture was queued")
		}
		if m.dropped.Load() != 1 || m.redactionFailed.Load() != 1 {
			t.Fatalf("dropped=%d redactionFailed=%d, want 1 and 1", m.dropped.Load(), m.redactionFailed.Load())
		}
	})

	t.Run("record exceeding policy maxBytes is dropped", func(t *testing.T) {
		t.Parallel()
		m, c := newCollector([]Include{Output}, 1024, nil)
		body := `{"choices":[{"finish_reason":"stop","message":{"content":"` + strings.Repeat("b", 1200) + `"}}]}`
		c.CaptureResponse(wire.Chat, []byte(body))
		if c.Finish(t.Context(), Identity{ProjectID: "project-1", Outcome: "success", Mode: Unary}, time.Now()) {
			t.Fatal("record larger than maxBytes was queued")
		}
		if len(m.queue) != 0 || m.dropped.Load() != 1 {
			t.Fatalf("queue=%d dropped=%d, want 0 and 1", len(m.queue), m.dropped.Load())
		}
	})

	t.Run("valid redacted capture enqueues scrubbed envelope with valid trace hex only", func(t *testing.T) {
		t.Parallel()
		engine := testRedactor(t, "top-secret")
		m, c := newCollector([]Include{Input, Output}, 8192, engine)
		c.CaptureDocument(map[string]json.RawMessage{"prompt": json.RawMessage(`"hide top-secret"`)})
		c.CaptureResponse(wire.Chat, []byte(`{"choices":[{"finish_reason":"stop","message":{"content":"echo top-secret"}}]}`))
		ok := c.Finish(t.Context(), Identity{
			ProjectID: "project-1",
			Route:     "route-1",
			Outcome:   "success",
			Mode:      Unary,
			TraceID:   "0123456789abcdef0123456789abcdef",
			SpanID:    "0000000000000000",
		}, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
		if !ok || len(m.queue) != 1 {
			t.Fatalf("Finish=%v queue=%d, want true and 1", ok, len(m.queue))
		}
		queuedItem := <-m.queue
		if strings.Contains(string(queuedItem.record), "top-secret") {
			t.Fatalf("queued capture leaked secret: %s", queuedItem.record)
		}
		var env envelope
		if err := json.Unmarshal(queuedItem.record, &env); err != nil {
			t.Fatal(err)
		}
		var data payload
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.TraceID != "0123456789abcdef0123456789abcdef" || data.SpanID != "" {
			t.Fatalf("trace=%q span=%q, want valid trace and cleared all-zero span", data.TraceID, data.SpanID)
		}
	})
}
