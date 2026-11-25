package kernel

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

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func installTestGuardrail(t *testing.T, h *harness, phase guardrailpolicy.Phase, action guardrailpolicy.Action, kind guardrails.Kind, configuration string) {
	t.Helper()
	current := h.rt.release
	snapshot := current.Snapshot().Clone()
	route := snapshot.Routes[routeSlug]
	snapshot.Guardrails = &guardrails.Registry{Definitions: map[string]guardrails.Revision{"revision": {ID: "revision", GuardrailID: "guard", ProjectID: "installation", Name: "Guard", Type: kind, Configuration: json.RawMessage(configuration), TimeoutMS: 1000, OnError: guardrailpolicy.FailClosed}}, Policies: map[string]guardrails.PolicyRevision{"policy-revision": {ID: "policy-revision", PolicyID: "policy", ProjectID: "installation", Entries: []guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: phase, Action: action}}}}, Attachments: []guardrails.Attachment{{ID: "attachment", Scope: "route", ScopeID: route.ID, PolicyRevisionID: "policy-revision"}}}
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
	installTestGuardrail(t, h, guardrailpolicy.Input, guardrailpolicy.Mask, "builtin.pii", `{"categories":["pii.email"]}`)
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
	installTestGuardrail(t, h, guardrailpolicy.InputParallel, guardrailpolicy.Block, "webhook", `{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`)
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
	installTestGuardrail(t, h, guardrailpolicy.OutputStream, guardrailpolicy.Block, "builtin.tools", `{"allow":["*"],"deny":["delete*"]}`)
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
	for _, phase := range []guardrailpolicy.Phase{guardrailpolicy.Input, guardrailpolicy.Output} {
		h := strictHarness(t, strictEmbeddings)
		installTestGuardrail(t, h, phase, guardrailpolicy.Block, "builtin.keywords", `{"words":["forbidden"]}`)
		h.mock.set("a", status(200, `{"object":"list","model":"model-a","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`))
		resp := h.do(t.Context(), "POST", "/v1/embeddings", fullKey, []byte(`{"model":"team-chat","input":"safe"}`), nil)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if phase == guardrailpolicy.Input && resp.StatusCode != 200 {
			t.Fatalf("inspectable input refused: %s", raw)
		}
		if phase == guardrailpolicy.Output && (resp.StatusCode != 422 || !strings.Contains(string(raw), "guardrail_surface_unavailable")) {
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
	definition := guardrails.Revision{ID: "revision", GuardrailID: "guard", ProjectID: "installation", Name: "Guard", Type: "webhook", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`), TimeoutMS: 1000, OnError: guardrailpolicy.FailClosed}
	engine, err := guardrails.Compile([]guardrails.Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: guardrailpolicy.Input, Action: guardrailpolicy.Block, Mandatory: true}}, map[string]guardrails.Revision{"revision": definition})
	if err != nil {
		t.Fatal(err)
	}

	evaluate := func(text string) (guardrails.Result, error) {
		return h.gateway.EvaluateToolGuardrails(t.Context(), *authority, h.rt.release, engine, "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50901", guardrailpolicy.Input, []guardrails.Segment{{ID: "arguments", Kind: "tool_call", ToolName: "t_alias", Text: text}}, &guardrails.Masking{})
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

func TestPreparationRetainsFailedGuardrailEvidenceWithoutReevaluation(t *testing.T) {
	evaluations := 0
	evaluator := guardrails.EvaluatorFunc(func(context.Context, guardrails.Revision, guardrails.Spec, guardrails.Evaluation) (guardrails.Verdict, error) {
		evaluations++
		return guardrails.Flag(), nil
	})
	h := newHarness(t, Config{}, Dependencies{Guardrails: func(guardrails.Caller) guardrails.Evaluator { return evaluator }})
	installTestGuardrail(t, h, guardrailpolicy.Input, guardrailpolicy.Block, "webhook", `{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`)
	route := h.rt.release.Snapshot().Route(routeSlug)
	target := route.Targets[0]
	provider := h.rt.release.Snapshot().Provider(target.ProviderID)
	parsed, err := protocols.Parse(wire.Chat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"blocked"}]}`), routeSlug)
	if err != nil {
		t.Fatal(err)
	}
	x := &Call{family: wire.Chat, routeLeg: routeLeg{Route: &route}, generation: generationCall{parsed: parsed}}
	x.request.release = h.rt.release
	x.authority = h.rt.keys[fullKey]
	x.keyID = h.keyID
	if failure := h.gateway.bindGuardrails(t.Context(), x); failure != nil {
		t.Fatal(failure)
	}
	if _, err := x.prepareCandidate(&provider, target.ID, target.ProviderModel); err == nil {
		t.Fatal("flagged input was admitted")
	}
	fact := &events.AttemptFact{TargetID: target.ID, ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, UpstreamModel: target.ProviderModel}
	for range 2 {
		evidence := x.preparation().factEvidence(fact)
		if len(evidence.guardrails) != 1 || evidence.guardrails[0].Outcome != guardrailpolicy.Blocked {
			t.Fatalf("failed preparation evidence = %+v", evidence.guardrails)
		}
		evidence.guardrails[0].Outcome = ""
	}
	if evaluations != 1 {
		t.Fatalf("accounting evidence reran the guardrail, evaluations = %d", evaluations)
	}
}

func streamedChat(deltas ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range deltas {
			frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}}}})
			io.WriteString(w, "data: "+string(frame)+"\n\n")
			w.(http.Flusher).Flush()
		}
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}
}

func streamedText(t *testing.T, raw []byte) string {
	t.Helper()
	var text strings.Builder
	for line := range strings.SplitSeq(string(raw), "\n") {
		data, found := strings.CutPrefix(line, "data: ")
		if !found || data == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("stream frame %q: %v", data, err)
		}
		for _, choice := range frame.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}
	return text.String()
}

func TestStreamedOutputIsRedactedBlockedOrUnmaskedAcrossFrameBoundaries(t *testing.T) {
	request := func(content string) []byte {
		return []byte(`{"model":"` + routeSlug + `","stream":true,"messages":[{"role":"user","content":"` + content + `"}]}`)
	}
	outcomes := func(h *harness) string {
		evidence, _ := json.Marshal(h.sink.last(t).GuardrailDecisions)
		return string(evidence)
	}

	redacting := newHarness(t, Config{})
	installTestGuardrail(t, redacting, guardrailpolicy.OutputStream, guardrailpolicy.Redact, "builtin.keywords", `{"words":["forbidden"]}`)
	redacting.mock.set("a", streamedChat("this is for", "bidden text", " and more"))
	resp := redacting.do(t.Context(), "POST", "/v1/chat/completions", fullKey, request("hello"), nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := streamedText(t, raw)
	if resp.StatusCode != 200 || strings.Contains(string(raw), "forbidden") || strings.Contains(text, "bidden") ||
		!strings.HasPrefix(text, "this is ") || !strings.HasSuffix(text, " text and more") || !strings.HasSuffix(string(raw), "data: [DONE]\n\n") {
		t.Fatalf("redacted stream: %d %s", resp.StatusCode, raw)
	}
	if evidence := outcomes(redacting); !strings.Contains(evidence, `"outcome":"redacted"`) || strings.Contains(evidence, "forbidden") {
		t.Fatalf("redaction evidence: %s", evidence)
	}

	blocking := newHarness(t, Config{})
	installTestGuardrail(t, blocking, guardrailpolicy.OutputStream, guardrailpolicy.Block, "builtin.keywords", `{"words":["forbidden"]}`)
	blocking.mock.set("a", streamedChat("this is for", "bidden text", " and more"))
	resp = blocking.do(t.Context(), "POST", "/v1/chat/completions", fullKey, request("hello"), nil)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "guardrail_blocked") || strings.Contains(string(raw), "bidden") || strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("blocked stream: %d %s", resp.StatusCode, raw)
	}
	if evidence := outcomes(blocking); !strings.Contains(evidence, `"outcome":"blocked"`) {
		t.Fatalf("block evidence: %s", evidence)
	}

	masking := newHarness(t, Config{})
	installTestGuardrail(t, masking, guardrailpolicy.Input, guardrailpolicy.Mask, "builtin.pii", `{"categories":["pii.email"]}`)
	masking.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil || len(sent.Messages) != 1 || strings.Contains(sent.Messages[0].Content, "alice") {
			t.Errorf("provider input: %+v %v", sent, err)
			return
		}
		masked := sent.Messages[0].Content
		streamedChat(masked[:len(masked)-8], masked[len(masked)-8:])(w, r)
	})
	resp = masking.do(t.Context(), "POST", "/v1/chat/completions", fullKey, request("email alice@example.com"), nil)
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if text := streamedText(t, raw); resp.StatusCode != 200 || text != "email alice@example.com" || strings.Contains(string(raw), "[OLP_") {
		t.Fatalf("unmasked stream: %d %s", resp.StatusCode, raw)
	}
}

func installModelGuardrailScenario(t *testing.T, h *harness, kind guardrails.Kind, configuration string, configureEval func(snapshot *routing.Snapshot, evalProvider *routing.Provider, evalRoute *routing.Route)) {
	t.Helper()
	h.republish(func(snapshot *routing.Snapshot, a, b routing.Provider) {
		mainRoute := snapshot.Routes[routeSlug]
		evalRouteID := uuid.NewString()
		evalRoute := routing.Route{
			ID: evalRouteID, RoutingID: evalRouteID, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(),
			Slug: "judge", Operations: []string{"generation"}, MaxAttempts: 1, OverallTimeout: 5000,
			Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed},
			Targets:  []routing.Target{{ID: uuid.NewString(), RoutingID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Priority: 0, Weight: 1, Timeout: 2000}},
		}
		mainRoute.Targets = mainRoute.Targets[:1]
		snapshot.Routes[routeSlug] = mainRoute
		snapshot.Guardrails = &guardrails.Registry{
			Definitions: map[string]guardrails.Revision{
				"eval-rev": {
					ID: "eval-rev", GuardrailID: "model-guard", ProjectID: "installation", Name: "Model Guard",
					Type: kind, Configuration: json.RawMessage(configuration), TimeoutMS: 2000, OnError: guardrailpolicy.FailClosed,
				},
			},
			Policies: map[string]guardrails.PolicyRevision{
				"eval-pol": {
					ID: "eval-pol", PolicyID: "policy", ProjectID: "installation",
					Entries: []guardrails.Entry{{GuardrailID: "model-guard", RevisionID: "eval-rev", Phase: guardrailpolicy.Input, Action: guardrailpolicy.Block}},
				},
			},
			Attachments: []guardrails.Attachment{
				{ID: "eval-att", Scope: "route", ScopeID: mainRoute.ID, PolicyRevisionID: "eval-pol"},
			},
		}
		configureEval(snapshot, &b, &evalRoute)
		snapshot.Providers[a.ID] = a
		snapshot.Providers[b.ID] = b
		snapshot.Routes[evalRoute.Slug] = evalRoute
	})
}

func findEnvelope(t *testing.T, envs []events.Envelope, route string) events.Envelope {
	t.Helper()
	for _, env := range envs {
		if env.Route == route {
			return env
		}
	}
	t.Fatalf("missing envelope for route %q in %+v", route, envs)
	return events.Envelope{}
}

func TestModelGuardrailRoutingAndAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		kind          guardrails.Kind
		config        string
		configureEval func(snapshot *routing.Snapshot, evalProvider *routing.Provider, evalRoute *routing.Route)
		blockReply    http.HandlerFunc
		passReply     http.HandlerFunc
		wantCategory  string
		expectUsage   bool
	}{
		{
			name:   "generation",
			kind:   "decision_model",
			config: `{"route":"judge","rule":{"pointer":"/blocked","equals":true,"category":"policy.decision","prompt":"Judge input"}}`,
			configureEval: func(_ *routing.Snapshot, _ *routing.Provider, _ *routing.Route) {
			},
			blockReply:   completion(modelB, `{"blocked":true}`),
			passReply:    completion(modelB, `{"blocked":false}`),
			wantCategory: "policy.decision",
			expectUsage:  true,
		},
		{
			name:   "moderation",
			kind:   "decision_model",
			config: `{"route":"judge","rule":{"pointer":"/results/0/flagged","equals":true,"category":"policy.moderation"}}`,
			configureEval: func(_ *routing.Snapshot, b *routing.Provider, r *routing.Route) {
				b.Kind = connectors.KindOpenAI
				b.ProfileID, b.ProfileRevision = "openai-moderation", connectors.ProfileRevision
				b.Capabilities = []routing.Capability{{Model: modelB, Operation: "moderation", Surface: "openai", Mode: "unary"}}
				r.Operations = []string{"moderation"}
				r.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
			},
			blockReply:   status(http.StatusOK, `{"id":"modr-1","model":"`+modelB+`","results":[{"flagged":true,"categories":{"harassment":true},"category_scores":{"harassment":0.95}}]}`),
			passReply:    status(http.StatusOK, `{"id":"modr-2","model":"`+modelB+`","results":[{"flagged":false,"categories":{"harassment":false},"category_scores":{"harassment":0.01}}]}`),
			wantCategory: "policy.moderation",
		},
		{
			name:   "classification",
			kind:   "decision_model",
			config: `{"route":"judge","rule":{"pointer":"/0/label","equals":"TOXIC","category":"policy.classification"}}`,
			configureEval: func(_ *routing.Snapshot, b *routing.Provider, r *routing.Route) {
				b.Kind = connectors.KindOpenAICompatible
				b.ProfileID, b.ProfileRevision = "tei-classification", connectors.ProfileRevision
				b.Capabilities = []routing.Capability{{Model: modelB, Operation: "classification", Surface: "native", Mode: "unary"}}
				r.Operations = []string{"classification"}
				r.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
			},
			blockReply:   status(http.StatusOK, `[{"label":"TOXIC","score":0.95}]`),
			passReply:    status(http.StatusOK, `[{"label":"SAFE","score":0.99}]`),
			wantCategory: "policy.classification",
		},
		{
			name:   "semantic_transformed",
			kind:   "semantic_guard",
			config: `{"route":"judge","semantic":{"category":"policy.semantic","similarity_threshold":0.9,"utterances":["forbidden request"]}}`,
			configureEval: func(_ *routing.Snapshot, b *routing.Provider, r *routing.Route) {
				b.Capabilities = append(b.Capabilities, routing.Capability{Model: modelB, Operation: "embeddings", Surface: "openai", Mode: "unary"})
				r.Operations = []string{"embeddings"}
			},
			blockReply:   status(http.StatusOK, `{"object":"list","model":"`+modelB+`","data":[{"object":"embedding","index":0,"embedding":[1,0]},{"object":"embedding","index":1,"embedding":[1,0]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`),
			passReply:    status(http.StatusOK, `{"object":"list","model":"`+modelB+`","data":[{"object":"embedding","index":0,"embedding":[1,0]},{"object":"embedding","index":1,"embedding":[0,1]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`),
			wantCategory: "policy.semantic",
			expectUsage:  true,
		},
		{
			name:   "semantic_strict",
			kind:   "semantic_guard",
			config: `{"route":"judge","semantic":{"category":"policy.semantic","similarity_threshold":0.9,"utterances":["forbidden request"]}}`,
			configureEval: func(_ *routing.Snapshot, b *routing.Provider, r *routing.Route) {
				b.Kind = connectors.KindOpenAI
				b.ProfileID, b.ProfileRevision = "openai-embeddings", connectors.ProfileRevision
				b.Capabilities = []routing.Capability{{Model: modelB, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
				r.Operations = []string{"embeddings"}
				r.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
			},
			blockReply:   status(http.StatusOK, `{"object":"list","model":"`+modelB+`","data":[{"object":"embedding","index":0,"embedding":[1,0]},{"object":"embedding","index":1,"embedding":[1,0]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`),
			passReply:    status(http.StatusOK, `{"object":"list","model":"`+modelB+`","data":[{"object":"embedding","index":0,"embedding":[1,0]},{"object":"embedding","index":1,"embedding":[0,1]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`),
			wantCategory: "policy.semantic",
			expectUsage:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			installModelGuardrailScenario(t, h, tc.kind, tc.config, tc.configureEval)

			h.mock.set("b", tc.blockReply)
			resp, _ := h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"block me"}]`)
			if resp.StatusCode != http.StatusBadRequest || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
				t.Fatalf("blocked call: status=%d main_calls=%d eval_calls=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
			}
			envs := h.sink.await(t, 2)
			parent := findEnvelope(t, envs, routeSlug)
			eval := findEnvelope(t, envs, "judge")
			if eval.Origin != usage.OriginClassifier || eval.ParentRequestID != parent.AccountingID || eval.EvaluatorGuardrailID != "model-guard" || eval.KeyID != h.keyID || eval.Outcome != "success" {
				t.Fatalf("classifier envelope mismatch: eval=%+v parent=%+v", eval, parent)
			}
			if tc.expectUsage && (eval.Usage == nil || eval.Usage.TotalTokens == 0) {
				t.Fatalf("expected evaluator usage accounting, got %+v", eval.Usage)
			}
			if len(parent.GuardrailDecisions) != 1 || parent.GuardrailDecisions[0].Outcome != guardrailpolicy.Blocked || len(parent.GuardrailDecisions[0].Categories) != 1 || parent.GuardrailDecisions[0].Categories[0] != tc.wantCategory {
				t.Fatalf("parent guardrail decisions = %+v", parent.GuardrailDecisions)
			}

			h.mock.set("b", tc.passReply)
			resp, _ = h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"allow me"}]`)
			if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 2 {
				t.Fatalf("passing call: status=%d main_calls=%d eval_calls=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
			}
			allEnvs := h.sink.await(t, 4)
			secondParent := findEnvelope(t, allEnvs[2:], routeSlug)
			secondEval := findEnvelope(t, allEnvs[2:], "judge")
			if secondEval.Origin != usage.OriginClassifier || secondEval.ParentRequestID != secondParent.AccountingID || secondEval.EvaluatorGuardrailID != "model-guard" {
				t.Fatalf("passing classifier envelope mismatch: eval=%+v parent=%+v", secondEval, secondParent)
			}
		})
	}
}

func TestModelGuardrailDeduplicationAuthorizationRecursionAndPlayground(t *testing.T) {
	t.Parallel()
	const cfg = `{"route":"judge","rule":{"pointer":"/blocked","equals":true,"category":"policy.decision"}}`

	t.Run("deduplicates across candidate targets and skips recursion", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, Config{})
		installModelGuardrailScenario(t, h, "decision_model", cfg, func(snapshot *routing.Snapshot, evalProvider *routing.Provider, evalRoute *routing.Route) {
			mainRoute := snapshot.Routes[routeSlug]
			mainRoute.Targets = []routing.Target{
				mainRoute.Targets[0],
				{ID: uuid.NewString(), RoutingID: uuid.NewString(), ProviderID: evalProvider.ID, ProviderModel: modelB, Priority: 1, Weight: 1, Timeout: 2000},
			}
			snapshot.Routes[routeSlug] = mainRoute
			snapshot.Guardrails.Attachments = append(snapshot.Guardrails.Attachments, guardrails.Attachment{
				ID: "eval-self-att", Scope: "route", ScopeID: evalRoute.ID, PolicyRevisionID: "eval-pol",
			})
		})
		h.mock.set("b", completion(modelB, `{"blocked":false}`))
		resp, _ := h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"same input"}]`)
		if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 1 {
			t.Fatalf("deduplication/recursion failed: status=%d main=%d eval=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
		}
	})

	t.Run("refuses evaluator route forbidden to caller key", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, Config{})
		installModelGuardrailScenario(t, h, "decision_model", cfg, func(_ *routing.Snapshot, _ *routing.Provider, _ *routing.Route) {})
		restricted := h.rt.keys[fullKey]
		restricted.Policy.AllowedRoutes = []string{routeSlug}
		h.rt.keys["restricted-key"] = restricted
		h.mock.set("b", completion(modelB, `{"blocked":false}`))
		resp, _ := h.chat("restricted-key", nil, `,"messages":[{"role":"user","content":"hello"}]`)
		if resp.StatusCode != http.StatusBadRequest || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
			t.Fatalf("forbidden evaluator route was not refused: status=%d main=%d eval=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
		}
		envs := h.sink.await(t, 1)
		if len(envs[0].GuardrailDecisions) != 1 || envs[0].GuardrailDecisions[0].Outcome != guardrailpolicy.FailedClosed {
			t.Fatalf("expected failed_closed evidence, got %+v", envs[0].GuardrailDecisions)
		}
	})

	t.Run("records playground origin for keyless guardrail evaluation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, Config{})
		installModelGuardrailScenario(t, h, "decision_model", cfg, func(_ *routing.Snapshot, _ *routing.Provider, _ *routing.Route) {})
		h.mock.set("b", completion(modelB, `{"blocked":false}`))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://console.test/api/v1/guardrails/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		reg := h.rt.release.Snapshot().Clone().Guardrails
		pinned := guardrails.Pinned{Policy: reg.Policies["eval-pol"], Definitions: reg.Definitions}
		res, err := h.gateway.EvaluateGuardrails(req, Member{UserID: "member-1", Admit: func(*routing.Route) *fault.Error { return nil }}, pinned, guardrails.Application{
			PolicyRevisionID: "eval-pol", Phase: guardrailpolicy.Input, Segments: []guardrails.Segment{{ID: "s0", Text: "safe"}},
		})
		if err != nil || len(res.Decisions) != 1 || res.Decisions[0].Outcome != guardrailpolicy.Passed {
			t.Fatalf("playground evaluation failed: res=%+v err=%v", res, err)
		}
		envs := h.sink.await(t, 2)
		parent := findEnvelope(t, envs, "guardrails")
		eval := findEnvelope(t, envs, "judge")
		if eval.Origin != usage.OriginPlayground || eval.ParentRequestID != parent.AccountingID || eval.EvaluatorGuardrailID != "model-guard" || eval.UserID != "member-1" {
			t.Fatalf("playground classifier envelope mismatch: eval=%+v parent=%+v", eval, parent)
		}
	})
}
