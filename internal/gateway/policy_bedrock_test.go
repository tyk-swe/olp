package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
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

// These structured-data regressions exercise the Bedrock destination contract
// without an unrelated OpenAI fallback target.
func useOnlyBedrockProvider(h *harness) {
	useBedrockProvider(h)
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	var targets []routing.Target
	for _, target := range route.Targets {
		if snapshot.Providers[target.ProviderID].Kind == "bedrock" {
			targets = append(targets, target)
		}
	}
	route.Targets = targets
	snapshot.Routes[routeSlug] = route
}

func bedrockToolArguments(t *testing.T, input string) string {
	t.Helper()
	arguments, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return `,"messages":[{"role":"user","content":"run the tool"},{"role":"assistant","content":null,"tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":` + string(arguments) + `}}]},{"role":"tool","tool_call_id":"call","content":"done"}]`
}

func TestContentPolicyBedrockStructuredInputRedactsBeforeDispatch(t *testing.T) {
	h := newHarness(t, Config{})
	useOnlyBedrockProvider(h)
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-structured-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t|12345", Action: contentpolicy.ActionRedact, Replacement: "[MASK]"})
	calls := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		calls <- string(data)
		converseReply(w, r)
	})
	resp, body := h.chat(fullKey, nil, bedrockToolArguments(t, `{"s3cr3t":12345,"safe":9007199254740993}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	select {
	case upstream := <-calls:
		if !json.Valid([]byte(upstream)) || strings.Contains(upstream, "s3cr3t") || strings.Contains(upstream, "12345") ||
			!strings.Contains(upstream, `"input":{"[MASK]":"[MASK]","safe":9007199254740993}`) {
			t.Fatalf("unsafe or incomplete structured redaction: %s", upstream)
		}
	case <-time.After(time.Second):
		t.Fatal("bedrock target not dispatched")
	}
	decisions := policyDecisions(h)
	if len(decisions) != 1 || decisions[0].Outcome != "redacted" || decisions[0].RuleID != "mask-structured-secret" {
		t.Fatalf("decisions %+v", decisions)
	}
}
