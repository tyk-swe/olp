package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/guardrails"
)

func installTestGuardrail(t *testing.T, h *harness, phase, action, kind, configuration string) {
	t.Helper()
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	snapshot.Guardrails = &guardrails.Registry{Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: "installation", Name: "Guard", Type: kind, Configuration: json.RawMessage(configuration), TimeoutMS: 1000, OnError: guardrails.FailClosed}}, Policies: map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: "installation", Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: phase, Action: action}}}}, Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: route.ID, PolicyRevisionID: "policy-revision"}}}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
}

func TestGuardrailMaskRestoresBeforeOutputAndRecordsOnlyMetadata(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.Input, guardrails.Mask, "builtin.pii", `{"categories":["pii.email"]}`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "alice@example.com") {
			t.Error("unmasked provider input")
		}
		var value struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &value)
		text := value.Messages[0].Content
		completion(modelA, text)(w, r)
	})
	resp, body := h.chat(fullKey, map[string]string{"X-OLP-Guardrails": "true"}, `,"messages":[{"role":"user","content":"email alice@example.com"}]`)
	encoded, _ := json.Marshal(body)
	if resp.StatusCode != 200 || !strings.Contains(string(encoded), "alice@example.com") || strings.Contains(string(encoded), "[OLP_") {
		t.Fatalf("restoration failed: %d %s", resp.StatusCode, encoded)
	}
	env := h.sink.last(t)
	evidence, _ := json.Marshal(env.GuardrailDecisions)
	if len(env.GuardrailDecisions) == 0 || strings.Contains(string(evidence), "alice") {
		t.Fatalf("unsafe evidence: %s", evidence)
	}
	if resp.Header.Get("X-OLP-Guardrails") == "" {
		t.Fatal("opt-in metadata missing")
	}
}

func TestGuardrailParallelRefusalAfterCommitmentIsInBand(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.InputParallel, guardrails.Block, "webhook", `{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`)
	release := make(chan struct{})
	h.gateway.GuardrailEvaluator = guardrails.EvaluatorFunc(func(ctx context.Context, _ guardrails.Revision, _ guardrails.Evaluation) (guardrails.Verdict, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return guardrails.Verdict{}, ctx.Err()
		}
		return guardrails.Verdict{Action: "block"}, nil
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"started\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	resp := h.do(t.Context(), "POST", "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","stream":true,"messages":[{"role":"user","content":"hello"}]}`), nil)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, "started") {
		t.Fatal(first, err)
	}
	close(release)
	rest, _ := io.ReadAll(reader)
	if resp.StatusCode != 200 || !strings.Contains(string(rest), "guardrail_blocked") || strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("late refusal: %d %s", resp.StatusCode, rest)
	}
}

func TestGuardrailToolArgumentsBufferedBeforeDeny(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.OutputStream, guardrails.Block, "builtin.tools", `{"allow":["*"],"deny":["delete*"]}`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"delete_all","arguments":"{"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		} {
			io.WriteString(w, "data: "+frame+"\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	resp := h.do(t.Context(), "POST", "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","stream":true,"messages":[{"role":"user","content":"hello"}]}`), nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "delete_all") || !strings.Contains(string(raw), "guardrail_blocked") {
		t.Fatalf("unchecked tool escaped: %d %s", resp.StatusCode, raw)
	}
}

func TestNativeOperationOutputUnavailableIsExplicit(t *testing.T) {
	for _, phase := range []string{guardrails.Input, guardrails.Output} {
		h := strictHarness(t, strictEmbeddings)
		installTestGuardrail(t, h, phase, guardrails.Block, "builtin.keywords", `{"words":["forbidden"]}`)
		h.mock.set("a", status(200, `{"object":"list","model":"model-a","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`))
		resp := h.do(t.Context(), "POST", "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":"safe"}`), nil)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if phase == guardrails.Input && resp.StatusCode != 200 {
			t.Fatalf("inspectable input refused: %s", raw)
		}
		if phase == guardrails.Output && (resp.StatusCode != 422 || !strings.Contains(string(raw), "guardrail_surface_unavailable")) {
			t.Fatalf("opaque output not refused: %d %s", resp.StatusCode, raw)
		}
	}
}

func TestToolGuardrailEvaluationReachesConfiguredEvaluator(t *testing.T) {
	h := newHarness(t, Config{})
	authority, err := h.rt.Authenticate(fullKey)
	if err != nil {
		t.Fatal(err)
	}
	definition := guardrails.Revision{ID: "revision", GuardrailID: "guard", ProjectID: "installation", Name: "Guard", Type: "webhook", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}
	engine, err := guardrails.Compile([]guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Block, Mandatory: true}}, map[string]guardrails.Revision{"revision": definition}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.GuardrailEvaluator = guardrails.EvaluatorFunc(func(_ context.Context, _ guardrails.Revision, evaluation guardrails.Evaluation) (guardrails.Verdict, error) {
		if len(evaluation.Segments) != 1 || evaluation.Segments[0].Kind != "tool_call" {
			return guardrails.Verdict{}, guardrails.ErrSurface
		}
		if strings.Contains(evaluation.Segments[0].Text, "bad") {
			return guardrails.Verdict{Action: "block", Categories: []string{"test.block"}}, nil
		}
		return guardrails.Verdict{Action: "pass"}, nil
	})
	evaluate := func(text string) (guardrails.Result, error) {
		return h.gateway.EvaluateToolGuardrails(t.Context(), *authority, h.rt.release, engine, "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50901", guardrails.Input, []guardrails.Segment{{ID: "arguments", Kind: "tool_call", ToolName: "t_alias", Text: text}}, &guardrails.Masking{})
	}
	passed, err := evaluate(`{"query":"hello"}`)
	if err != nil || passed.Blocked || len(passed.Decisions) != 1 || passed.Decisions[0].Outcome != "passed" {
		t.Fatalf("clean tool arguments did not pass through the evaluator: %+v %v", passed, err)
	}
	blocked, err := evaluate(`{"query":"bad"}`)
	if !blocked.Blocked || err == nil {
		t.Fatalf("evaluator verdict did not block tool arguments: %+v %v", blocked, err)
	}
	var refusal *guardrails.Error
	if !errors.As(err, &refusal) || refusal.Code != "guardrail_blocked" {
		t.Fatalf("tool evaluation returned the wrong refusal: %v", err)
	}
	if len(blocked.Decisions) != 1 || blocked.Decisions[0].Outcome != "blocked" {
		t.Fatalf("blocked tool arguments kept no decision evidence: %+v", blocked.Decisions)
	}
}
