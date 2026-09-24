//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/tests/fidelity"
)

func TestPublicNegotiatedUsagePreservesNativeCacheCategoriesOrFailsClosed(t *testing.T) {
	frozen, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	withCache := strings.Replace(string(frozen), `"usage":{"input_tokens":18,"output_tokens":1}`, `"usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":5}}`, 1)
	expectedUsage := []byte(`{"input_tokens":3,"output_tokens":28,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":5}}`)
	for _, tc := range []struct {
		name, events string
		valid        bool
	}{
		{"known cache categories", withCache, true},
		{"unknown native category", strings.Replace(withCache, `"cache_read_input_tokens":20`, `"future_token_category":1,"cache_read_input_tokens":20`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccessHarness(t)
			var calls atomic.Int64
			slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.events)
			})
			source := strings.Replace(continuationInput, "ROUTE", slug, 1)
			headers := continuationHeaders()
			status, body, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
			if calls.Load() != 1 {
				t.Fatalf("provider dispatches=%d", calls.Load())
			}
			if !tc.valid {
				if bytes.Contains(body, []byte(`"tool_calls"`)) || bytes.Contains(body, []byte(`"ready":true`)) || bytes.Contains(body, []byte(`"handle"`)) {
					t.Fatalf("unknown native usage became actionable: %d %s", status, body)
				}
				status, retry, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
				if status != 409 || !bytes.Contains(retry, []byte("continuation_outcome_unknown")) || calls.Load() != 1 {
					t.Fatalf("unknown usage replayed inference: %d calls=%d %s", status, calls.Load(), retry)
				}
				return
			}
			if status != 200 {
				t.Fatalf("known cache usage rejected: %d %s", status, body)
			}
			var handle string
			found := false
			for line := range strings.SplitSeq(string(body), "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var frame struct {
					Usage struct {
						PromptTokens int64 `json:"prompt_tokens"`
						Details      struct {
							CachedTokens int64 `json:"cached_tokens"`
						} `json:"prompt_tokens_details"`
					} `json:"usage"`
					OLP struct {
						Handle      string          `json:"handle"`
						Ready       bool            `json:"ready"`
						NativeUsage json.RawMessage `json:"native_usage"`
					} `json:"olp"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
					t.Fatal(err)
				}
				if !frame.OLP.Ready {
					continue
				}
				if frame.Usage.PromptTokens != 53 || frame.Usage.Details.CachedTokens != 20 || fidelity.Compare(expectedUsage, frame.OLP.NativeUsage) != nil {
					t.Fatalf("client lost cache accounting: %s", line)
				}
				handle, found = frame.OLP.Handle, true
			}
			if !found || !strings.HasPrefix(handle, "continuation_") {
				t.Fatal("cache-aware ready delivery omitted a handle")
			}
			status, replay, replayHeaders := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
			if status != 200 || !bytes.Equal(body, replay) || replayHeaders.Get("X-OLP-Delivery-Replay") != "true" || calls.Load() != 1 {
				t.Fatalf("cached usage delivery replay changed: %d calls=%d", status, calls.Load())
			}
		})
	}
}
