package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

const bedrockSecret = `{"access_key_id":"AKIAEXAMPLEEXAMPLE00","secret_access_key":"exampleexampleexampleexampleexample00"}`

// useBedrockProvider turns provider a into an AWS Bedrock connector served by
// the mock upstream and signed with static SigV4 credentials.
func useBedrockProvider(h *harness) {
	h.t.Helper()
	release := h.rt.release
	snapshot := release.Snapshot
	credentials := map[string][]byte{}
	for id, provider := range snapshot.Providers {
		for _, slot := range provider.Slots {
			credentials[*slot.CredentialID], _ = release.Credential(*slot.CredentialID)
		}
		if provider.Name != "a" {
			continue
		}
		provider.Kind, provider.AuthMode, provider.CloudRegion = "bedrock", "static", "us-east-1"
		provider.Endpoint = h.upstream.URL + "/a"
		credentials[*provider.Slots[0].CredentialID] = []byte(bedrockSecret)
		snapshot.Providers[id] = provider
	}
	var err error
	h.rt.release, err = runtime.NewRelease(release.ID, release.Sequence, snapshot, credentials)
	if err != nil {
		h.t.Fatal(err)
	}
}

func converseReply(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"`+answerText+`"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":2,"totalTokens":5}}`)
}

func TestContentPolicyBedrockInputBlocksBeforeDispatch(t *testing.T) {
	h := newHarness(t, Config{})
	useBedrockProvider(h)
	h.mock.set("a", converseReply)
	setRoutePolicy(h, contentpolicy.Rule{ID: "no-secrets", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionBlock})
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"system","content":"keep it short"},{"role":"user","content":"say s3cr3t"}]`)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "content_policy_blocked" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatalf("provider called: a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
	decisions := policyDecisions(h)
	if len(decisions) != 1 || decisions[0] != (contentpolicy.Decision{RuleID: "no-secrets", Phase: "input", Action: "block", Outcome: "blocked"}) {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestContentPolicyBedrockInputRedactReachesConverse(t *testing.T) {
	h := newHarness(t, Config{})
	useBedrockProvider(h)
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[MASK]"})
	type seen struct{ path, body string }
	calls := make(chan seen, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		calls <- seen{r.URL.Path, string(data)}
		converseReply(w, r)
	})
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"system","content":"never echo s3cr3t"},{"role":"user","content":"the s3cr3t plan"}]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	var call seen
	select {
	case call = <-calls:
	case <-time.After(time.Second):
		t.Fatal("bedrock target not dispatched")
	}
	if !strings.HasSuffix(call.path, "/converse") {
		t.Fatalf("upstream path=%s", call.path)
	}
	var converse struct {
		System   []struct{ Text string } `json:"system"`
		Messages []struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(call.body), &converse); err != nil {
		t.Fatalf("upstream body=%s: %v", call.body, err)
	}
	if strings.Contains(call.body, "s3cr3t") || len(converse.System) != 1 || converse.System[0].Text != "never echo [MASK]" ||
		len(converse.Messages) != 1 || len(converse.Messages[0].Content) != 1 || converse.Messages[0].Content[0].Text != "the [MASK] plan" {
		t.Fatalf("upstream body=%s", call.body)
	}
	decisions := policyDecisions(h)
	if len(decisions) != 1 || decisions[0].Outcome != "redacted" || decisions[0].RuleID != "mask-secret" {
		t.Fatalf("decisions %+v", decisions)
	}
}

// Canonical surfaces only encode to inspectable wires today (a bedrock_invoke
// body is refused by the encoder first), so the fail-closed gate is checked
// directly against wires the input rules cannot read.
func TestContentPolicyUninspectableWireFailsClosed(t *testing.T) {
	route := &runtime.Route{Slug: routeSlug, ContentPolicy: &contentpolicy.Policy{Rules: []contentpolicy.Rule{
		{ID: "no-secrets", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionBlock},
	}}}
	compiled, e := compiledPolicy(route)
	if e != nil {
		t.Fatal(e)
	}
	for _, wire := range []openai.Family{openai.FamilyBedrockInvoke, openai.FamilyGeminiInteractions, openai.FamilyRealtime, openai.FamilyImageGeneration} {
		e := inputPolicyWireGate(route, compiled, wire)
		if e == nil || e.Status != http.StatusUnprocessableEntity || e.Code != "content_policy_surface_unavailable" || !strings.Contains(e.Message, string(wire)) {
			t.Fatalf("%s: gate=%+v", wire, e)
		}
	}
	for _, wire := range []openai.Family{openai.FamilyBedrock, "bedrock_count", openai.FamilyBedrockEmbeddings, openai.FamilyChat, openai.FamilyAnthropic, openai.FamilyGemini} {
		if e := inputPolicyWireGate(route, compiled, wire); e != nil {
			t.Fatalf("%s: inspectable wire refused: %+v", wire, e)
		}
	}
	outputOnly := &runtime.Route{Slug: routeSlug, ContentPolicy: &contentpolicy.Policy{Rules: []contentpolicy.Rule{
		{ID: "mask-out", Phase: contentpolicy.PhaseOutput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "x"},
	}}}
	compiled, e = compiledPolicy(outputOnly)
	if e != nil {
		t.Fatal(e)
	}
	if e := inputPolicyWireGate(outputOnly, compiled, openai.FamilyBedrockInvoke); e != nil {
		t.Fatalf("output-only policy refused: %+v", e)
	}
}
