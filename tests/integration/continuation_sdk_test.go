//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/fidelity"
)

func testContinuationSDK(t *testing.T, command string, args ...string) {
	t.Helper()
	h := newAccessHarness(t)
	events, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	next, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var initial map[string]json.RawMessage
	if err := json.Unmarshal(next, &initial); err != nil {
		t.Fatal(err)
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(initial["messages"], &messages); err != nil || len(messages) != 3 {
		t.Fatalf("invalid independent native reference: %v", err)
	}
	initial["messages"], _ = json.Marshal(messages[:1])
	initial["stream"] = json.RawMessage(`true`)
	expectedFirst, _ := json.Marshal(initial)
	var calls, rejected atomic.Int64
	slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, r *http.Request, body []byte) {
		calls.Add(1)
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != vendorSecret || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			rejected.Add(1)
			http.Error(w, "native profile mismatch", http.StatusBadRequest)
			return
		}
		var request map[string]json.RawMessage
		_ = json.Unmarshal(body, &request)
		var actualMessages []json.RawMessage
		_ = json.Unmarshal(request["messages"], &actualMessages)
		expected := expectedFirst
		if len(actualMessages) > 1 {
			expected = next
		}
		if err := fidelity.Compare(expected, body); err != nil {
			rejected.Add(1)
			http.Error(w, "native continuation differs: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(actualMessages) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, frame := range bytes.SplitAfter(events, []byte("\n\n")) {
				if len(frame) == 0 {
					continue
				}
				_, _ = w.Write(frame)
				_ = http.NewResponseController(w).Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-sdk-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Both tools completed."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":4}}`)
	})
	if command == "node" {
		if _, err := os.Stat(filepath.Join("..", "sdk-smoke", "node_modules", "openai")); err != nil {
			t.Fatal("official JavaScript SDK dependency is missing; run pnpm install --frozen-lockfile")
		}
	}
	cmd := exec.CommandContext(t.Context(), command, args...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "OLP_CONTINUATION_ORIGIN="+h.HTTP.URL, "OLP_CONTINUATION_KEY="+key, "OLP_CONTINUATION_ROUTE="+slug)
	started := time.Now()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("official SDK continuation: %v (%s)\n%s", err, time.Since(started), output)
	}
	if calls.Load() != 2 || rejected.Load() != 0 || !strings.Contains(string(output), "two native dispatches") && !strings.Contains(string(output), "openai-js-7.4.0") {
		t.Fatalf("SDK did not complete exactly one native two-turn workflow: calls=%d rejected=%d output=%s", calls.Load(), rejected.Load(), output)
	}
}

func TestNegotiatedContinuationOfficialJavaScriptSDK(t *testing.T) {
	testContinuationSDK(t, "node", "tests/sdk-smoke/negotiated-continuation.mjs")
}
