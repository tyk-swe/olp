package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
)

func setRoutePolicy(h *harness, rules ...contentpolicy.Rule) {
	h.t.Helper()
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	route.ContentPolicy = &contentpolicy.Policy{Rules: rules}
	snapshot.Routes[routeSlug] = route
}

func policyDecisions(h *harness) []contentpolicy.Decision {
	h.t.Helper()
	return h.sink.last(h.t).PolicyDecisions
}

func TestContentPolicyOutputBlocksWithAccounting(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "no-leak", Phase: contentpolicy.PhaseOutput, Pattern: "upstream", Action: contentpolicy.ActionBlock})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "content_policy_blocked" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 1 {
		t.Fatalf("provider calls=%d", h.mock.count("a"))
	}
	env := h.sink.last(t)
	if env.Usage == nil || env.Usage.TotalTokens != 5 {
		t.Fatalf("usage not retained: %+v", env.Usage)
	}
	decisions := env.PolicyDecisions
	if len(decisions) != 1 || decisions[0] != (contentpolicy.Decision{RuleID: "no-leak", Phase: "output", Action: "block", Outcome: "blocked"}) {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestContentPolicyResponsesSurface(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h,
		contentpolicy.Rule{ID: "mask-in", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[REDACTED]"},
		contentpolicy.Rule{ID: "mask-out", Phase: contentpolicy.PhaseOutput, Pattern: "upstream", Action: contentpolicy.ActionRedact, Replacement: "provider"},
	)
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- string(data)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","object":"response","model":"model-a","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello from upstream","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey,
		[]byte(`{"model":"`+routeSlug+`","input":"hide s3cr3t please"}`), nil)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if upstream := <-seen; strings.Contains(upstream, "s3cr3t") || !strings.Contains(upstream, "[REDACTED]") {
		t.Fatalf("upstream body=%s", upstream)
	}
	if !strings.Contains(string(data), "provider") || strings.Contains(string(data), "hello from upstream") {
		t.Fatalf("output not rewritten: %s", data)
	}
	if env := h.sink.last(t); len(env.PolicyDecisions) != 2 {
		t.Fatalf("decisions %+v", env.PolicyDecisions)
	}
}

func TestContentPolicyDecisionDedupMediaSlots(t *testing.T) {
	h := newMediaHarness(t)
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[MASK]"})
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- string(data)
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "audio-payload")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/audio/speech", fullKey,
		[]byte(`{"model":"`+routeSlug+`","input":"say the s3cr3t","instructions":"a s3cr3t voice","voice":"alloy"}`), nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	upstream := <-seen
	if strings.Contains(upstream, "s3cr3t") || strings.Count(upstream, "[MASK]") != 2 {
		t.Fatalf("upstream body=%s", upstream)
	}
	if decisions := policyDecisions(h); len(decisions) != 1 {
		t.Fatalf("decisions %+v", decisions)
	}
}
