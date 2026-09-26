//go:build integration

package integration_test

import (
	"bytes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNegotiatedContinuationRejectsUnqualifiedOutputAndControlsBeforeDispatch(t *testing.T) {
	h := newAccessHarness(t)
	var calls atomic.Int64
	slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		calls.Add(1)
		http.Error(w, "unexpected provider dispatch", 500)
	})
	for _, tc := range []struct {
		name, addition, category string
	}{
		{"multiple candidates", `,"n":2`, "target_capability"},
		{"citation projection", `,"citations":{"enabled":true}`, "target_capability"},
		{"structured output", `,"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}`, "target_capability"},
		{"parallel tool switch", `,"parallel_tool_calls":false`, "target_capability"},
		{"foreign reasoning budget", `,"reasoning_effort":"high"`, "reasoning_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.Replace(continuationInput, "ROUTE", slug, 1)
			source = strings.Replace(source, `"stream":true`, `"stream":true`+tc.addition, 1)
			headers := continuationHeaders()
			status, result, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
			if status != 400 || !bytes.Contains(result, []byte(`"code":"`+tc.category+`"`)) || calls.Load() != 0 {
				t.Fatalf("unqualified %s dispatched or lost its precise reason: %d calls=%d %s", tc.name, status, calls.Load(), result)
			}
			var claims int
			if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE kind='continuation' AND submission_id=$1`, headers["X-OLP-Submission-ID"]).Scan(&claims); err != nil || claims != 0 {
				t.Fatalf("pre-dispatch rejection left accepted work: claims=%d err=%v", claims, err)
			}
		})
	}
}
