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

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

func installTestGuardrail(t *testing.T, h *harness, phase guardrails.Phase, action guardrails.Action, kind guardrails.Kind, configuration string) {
	t.Helper()
	current := h.rt.release
	snapshot := current.Snapshot().Clone()
	route := snapshot.Routes[routeSlug]
	snapshot.Guardrails = &guardrails.Registry{Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: "installation", Name: "Guard", Type: kind, Configuration: json.RawMessage(configuration), TimeoutMS: 1000, OnError: guardrails.FailClosed}}, Policies: map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: "installation", Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: phase, Action: action}}}}, Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: route.ID, PolicyRevisionID: "policy-revision"}}}
	credentials := map[string][]byte{}
	for _, provider := range snapshot.Providers {
		for _, slot := range provider.Slots {
			if slot.CredentialID != nil {
				credentials[*slot.CredentialID], _ = current.Credential(*slot.CredentialID)
			}
		}
	}
	installed, err := release.NewRelease(current.ID, current.Sequence, snapshot, credentials)
	if err != nil {
		t.Fatal(err)
	}
	h.rt.release = installed
}

func TestGuardrailMaskRestoresBeforeOutputAndRecordsOnlyMetadata(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.Input, guardrails.Mask, "builtin.pii", `{"categories":["pii.email"]}`)
	borrowed := h.rt.release.Snapshot()
	*borrowed.GuardrailEngine(routeSlug, "") = guardrails.Engine{}
	*borrowed = routing.View{}

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
	release := make(chan struct{})
	evaluator := guardrails.EvaluatorFunc(func(ctx context.Context, _ guardrails.Revision, _ guardrails.Spec, _ guardrails.Evaluation) (guardrails.Verdict, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return guardrails.Verdict{}, ctx.Err()
		}
		return guardrails.Flag(), nil
	})
	h := newHarness(t, Config{}, Dependencies{Guardrails: func(guardrails.Caller) guardrails.Evaluator { return evaluator }})
	installTestGuardrail(t, h, guardrails.InputParallel, guardrails.Block, "webhook", `{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`)
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
	for _, phase := range []guardrails.Phase{guardrails.Input, guardrails.Output} {
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
	evaluator := guardrails.EvaluatorFunc(func(_ context.Context, definition guardrails.Revision, _ guardrails.Spec, evaluation guardrails.Evaluation) (guardrails.Verdict, error) {
		if !json.Valid(definition.Configuration) {
			return guardrails.Verdict{}, guardrails.ErrSurface
		}
		clear(definition.Configuration)
		if len(evaluation.Segments) != 1 || evaluation.Segments[0].Kind != "tool_call" {
			return guardrails.Verdict{}, guardrails.ErrSurface
		}
		if strings.Contains(evaluation.Segments[0].Text, "bad") {
			return guardrails.Flag("test.block"), nil
		}
		return guardrails.Verdict{}, nil
	})
	h := newHarness(t, Config{}, Dependencies{Guardrails: func(guardrails.Caller) guardrails.Evaluator { return evaluator }})
	authority, err := h.rt.Authenticate(fullKey)
	if err != nil {
		t.Fatal(err)
	}
	definition := guardrails.Revision{ID: "revision", GuardrailID: "guard", ProjectID: "installation", Name: "Guard", Type: "webhook", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}
	engine, err := guardrails.Compile([]guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrails.Input, Action: guardrails.Block, Mandatory: true}}, map[string]guardrails.Revision{"revision": definition})
	if err != nil {
		t.Fatal(err)
	}

	evaluate := func(text string) (guardrails.Result, error) {
		return h.gateway.EvaluateToolGuardrails(t.Context(), *authority, h.rt.release, engine, "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50901", guardrails.Input, []guardrails.Segment{{ID: "arguments", Kind: "tool_call", ToolName: "t_alias", Text: text}}, &guardrails.Masking{})
	}
	passed, err := evaluate(`{"query":"hello"}`)
	if err != nil || len(passed.Decisions) != 1 || passed.Decisions[0].Outcome != "passed" {
		t.Fatalf("clean tool arguments did not pass through the evaluator: %+v %v", passed, err)
	}
	blocked, err := evaluate(`{"query":"bad"}`)
	if !errors.Is(err, guardrails.ErrBlocked) {
		t.Fatalf("evaluator verdict did not block tool arguments: %+v %v", blocked, err)
	}
	if len(blocked.Decisions) != 1 || blocked.Decisions[0].Outcome != "blocked" {
		t.Fatalf("blocked tool arguments kept no decision evidence: %+v", blocked.Decisions)
	}
}

func TestPublishedPolicySurvivesMutationOfReturnedCompiledRules(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
		route := snapshot.Routes[routeSlug]
		route.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "secret", Phase: contentpolicy.PhaseInput, Pattern: "secret", Action: contentpolicy.ActionBlock}}}
		snapshot.Routes[routeSlug] = route
	})
	route := h.rt.release.Snapshot().Route(routeSlug)
	compiled := route.CompiledContentPolicy()
	if err := compiled.Input[0].Re.UnmarshalText([]byte("public")); err != nil {
		t.Fatal(err)
	}
	compiled.Input[0].Action = contentpolicy.ActionRedact
	compiled.Policy.Rules[0].Pattern = "public"
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"secret"}]`)
	if resp.StatusCode != http.StatusBadRequest || body["error"].(map[string]any)["code"] != "content_policy_blocked" {
		t.Fatalf("published policy changed through returned rules: %d %v", resp.StatusCode, body)
	}
	if calls := h.mock.count("a") + h.mock.count("b"); calls != 0 {
		t.Fatalf("blocked request reached providers: %d", calls)
	}
}

func TestMediaInputRedactionReachesProfiledUpstream(t *testing.T) {
	profiled := func(p *routing.Provider) {
		p.ProfileID, p.ProfileRevision = "compatible-chat", connectors.ProfileRevision
	}
	redact := func(r *routing.Route) {
		r.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cret", Action: contentpolicy.ActionRedact, Replacement: "[gone]"}}}
	}
	for _, tc := range []mediaCase{
		{name: "image generation", operation: media.OpImageGeneration, path: "/v1/images/generations", provider: profiled, route: redact,
			json:    `{"model":"{route}","prompt":"a fox named s3cret"}`,
			replies: reply("application/json", `{"created":1,"data":[{"url":"https://images.example/fox.png"}]}`)},
		{name: "speech", operation: media.OpSpeech, path: "/v1/audio/speech", provider: profiled, route: redact,
			json:    `{"model":"{route}","input":"say s3cret","voice":"alloy","instructions":"whisper s3cret"}`,
			replies: reply("audio/mpeg", "ID3-fake-mp3")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := runMediaCase(t, tc)
			if record.Status != http.StatusOK || len(record.Upstream) != 1 {
				t.Fatalf("status %d with %d upstream calls", record.Status, len(record.Upstream))
			}
			sent := string(record.Upstream[0].JSON)
			if strings.Contains(sent, "s3cret") || !strings.Contains(sent, "[gone]") {
				t.Fatalf("upstream body kept the redacted text: %s", sent)
			}
		})
	}
}
