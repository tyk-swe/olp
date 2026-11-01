package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"hash/crc32"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestToolJSONGuardrailsInspectDecodedValuesAndPreserveOtherContent(t *testing.T) {
	const original = ` {"a/b~c":[{"sensitive":"forbid\u0064en","safe":"k\u0065ep"}],"count":3,"empty":{},"none":null} `
	const replacement = `clean "quoted" \ path`
	configuration, err := json.Marshal(KeywordConfig{Words: []string{"forbidden"}, Replacement: replacement})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{Input, Output} {
		t.Run(phase, func(t *testing.T) {
			document, err := oif.ParseJSON([]byte(original), oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			surface := ToolJSONSurface(document, "read", phase == Output)
			blocked, blockErr := testEngine(t, "builtin.keywords", string(configuration), phase, Block, false, nil).Evaluate(t.Context(), Evaluation{Phase: phase, Segments: surface.Segments()}, nil)
			if !errors.Is(blockErr, ErrBlocked) || !blocked.Blocked {
				t.Fatalf("escaped blocked text was allowed: %v", blockErr)
			}
			redacted, redactErr := testEngine(t, "builtin.keywords", string(configuration), phase, Redact, false, nil).Evaluate(t.Context(), Evaluation{Phase: phase, Segments: surface.Segments()}, nil)
			if redactErr != nil || !redacted.Changed {
				t.Fatalf("escaped text was not transformed: %v", redactErr)
			}
			updated, applyErr := surface.Apply(redacted.Segments)
			if applyErr != nil {
				t.Fatal(applyErr)
			}
			value, found := updated.Lookup("/a~1b~0c/0/sensitive")
			text, isText := value.Text()
			if !found || !isText || text != replacement {
				t.Fatalf("replacement did not retain valid JSON string semantics: %q", text)
			}
			if !strings.Contains(updated.Raw(), `"safe":"k\u0065ep"`) || !strings.Contains(updated.Raw(), `"count":3,"empty":{},"none":null`) {
				t.Fatal("transformation changed unrelated JSON content")
			}
			if document.Raw() != original {
				t.Fatal("transformation modified the original document")
			}
			unchanged, unchangedErr := surface.Apply(surface.Segments())
			if unchangedErr != nil || unchanged.Raw() != original {
				t.Fatalf("unmodified evaluation changed source bytes: %v", unchangedErr)
			}
		})
	}
}

func TestTypeSafeWorkflowPolicies(t *testing.T) {
	for _, tc := range []struct {
		name, configuration string
		valid               bool
	}{
		{"route", `{"route":"relevance-route"}`, true},
		{"missing route", `{}`, false},
		{"invalid route", `{"route":"bad route"}`, false},
		{"unknown field", `{"route":"relevance-route","extra":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := Revision{Type: "typesafe", Configuration: json.RawMessage(tc.configuration), TimeoutMS: 1000, OnError: FailClosed}
			if err := ValidateDefinition(definition); (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
		})
	}

	definitions := map[string]Revision{
		"active":  {ID: "active", GuardrailID: "active", Type: "typesafe", Configuration: json.RawMessage(`{"route":"relevance-route"}`), TimeoutMS: 1000, OnError: FailOpen},
		"monitor": {ID: "monitor", GuardrailID: "monitor", Type: "typesafe", Configuration: json.RawMessage(`{"route":"monitor-route"}`), TimeoutMS: 1000, OnError: FailClosed},
		"output":  {ID: "output", GuardrailID: "output", Type: "typesafe", Configuration: json.RawMessage(`{"route":"output-route"}`), TimeoutMS: 1000, OnError: FailClosed},
		"keyword": {ID: "keyword", GuardrailID: "keyword", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["secret"]}`), TimeoutMS: 1000, OnError: FailClosed},
	}
	entries := []Entry{
		{GuardrailID: "active", RevisionID: "active", Phase: Input, Action: Block},
		{GuardrailID: "monitor", RevisionID: "monitor", Phase: Input, Action: Block, Monitor: true},
		{GuardrailID: "output", RevisionID: "output", Phase: Output, Action: Block},
		{GuardrailID: "keyword", RevisionID: "keyword", Phase: Input, Action: Block},
	}
	engine, err := Compile(entries, definitions, nil)
	if err != nil {
		t.Fatal(err)
	}
	policies := engine.TypeSafePolicies()
	if len(policies) != 1 || policies[0].Config.Route != "relevance-route" || policies[0].Entry.RevisionID != "active" || policies[0].Revision.OnError != FailOpen {
		t.Fatalf("unexpected effective policies: %+v", policies)
	}
	if engine.OnlyTypeSafe() || engine.WithoutModelGuardrails() == nil {
		t.Fatal("mixed policy lost its local guardrail")
	}
	if _, err := evaluateText(t, engine.WithoutModelGuardrails(), "secret", Input, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("local input policy did not remain effective: %v", err)
	}
	only := testEngine(t, "typesafe", `{"route":"relevance-route"}`, Input, Block, false, nil)
	if !only.OnlyTypeSafe() || only.WithoutModelGuardrails() != nil {
		t.Fatal("TypeSafe was not isolated from recursive model evaluations")
	}
	result, err := only.Evaluate(t.Context(), Evaluation{Phase: Input, Segments: []Segment{{ID: "user", Text: "request"}}, InternalModel: true}, nil)
	if err != nil || result.Blocked || len(result.Decisions) != 1 || result.Decisions[0].Outcome != "skipped" {
		t.Fatalf("internal model unexpectedly evaluated TypeSafe: %+v, %v", result, err)
	}
}

func TestCompressionWorkflowRecoveryRespectsRedactionAndExemptions(t *testing.T) {
	definitions := map[string]Revision{
		"redact": {ID: "redact", GuardrailID: "redact", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 1000, OnError: FailClosed},
		"first":  {ID: "first", GuardrailID: "first", Type: "headroom", Configuration: json.RawMessage(`{"endpoint":"https://guardrail.example/scan","credential_ref":"credential","content_egress":true}`), TimeoutMS: 1000, OnError: FailClosed},
		"second": {ID: "second", GuardrailID: "second", Type: "compresr", Configuration: json.RawMessage(`{"endpoint":"https://guardrail.example/scan","credential_ref":"credential","content_egress":true,"parameters":{"query":"task"}}`), TimeoutMS: 1000, OnError: FailClosed},
	}
	entries := []Entry{
		{GuardrailID: "redact", RevisionID: "redact", Phase: Input, Action: Redact},
		{GuardrailID: "first", RevisionID: "first", Phase: Input, Action: Redact},
		{GuardrailID: "second", RevisionID: "second", Phase: Input, Action: Redact},
	}
	evaluator := EvaluatorFunc(func(_ context.Context, definition Revision, evaluation Evaluation) (Verdict, error) {
		for _, segment := range evaluation.Segments {
			if segment.ID == "recovered" || strings.Contains(segment.Text, "private") {
				t.Errorf("compression received protected content: %+v", segment)
			}
		}
		index := slices.IndexFunc(evaluation.Segments, func(segment Segment) bool { return segment.ID == definition.ID })
		if index < 0 {
			return Verdict{Action: "pass"}, nil
		}
		segment := evaluation.Segments[index]
		return Verdict{Action: Redact, Redactions: []Redaction{{Segment: segment.ID, Start: 0, End: len(segment.Text), Replacement: definition.ID + " summary"}}}, nil
	})
	engine, err := Compile(entries, definitions, evaluator)
	if err != nil || !engine.HasCompression() {
		t.Fatalf("compression policy was not available: %v", err)
	}
	segments := []Segment{
		{ID: "first", Role: "user", Text: "first private details"},
		{ID: "recovered", Kind: "tool_result", ToolCallID: "recover-call", Text: "recovered private details"},
		{ID: "second", Role: "user", Text: "second private details"},
	}
	for _, tc := range []struct {
		name              string
		recovery          bool
		first, second     string
		wantRecoveryCount int
	}{
		{"workflow", true, "first summary\n[recovery id: 0]", "second summary\n[recovery id: 1]", 2},
		{"ordinary request", false, "first summary", "second summary", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := engine.Evaluate(t.Context(), Evaluation{Phase: Input, Segments: segments, CompressionExemptIDs: []string{"recovered"}, WorkflowRecovery: tc.recovery}, nil)
			if err != nil || result.Blocked || !result.Changed || len(result.Segments) != 3 {
				t.Fatalf("evaluation failed: %+v, %v", result, err)
			}
			if result.Segments[0].Text != tc.first || result.Segments[1].Text != "recovered [REDACTED] details" || result.Segments[2].Text != tc.second {
				t.Fatalf("unexpected transformed content: %+v", result.Segments)
			}
			if len(result.Recovery) != tc.wantRecoveryCount {
				t.Fatalf("unexpected recovery content: %+v", result.Recovery)
			}
			if tc.recovery && (result.Recovery[0].Text != "first [REDACTED] details" || result.Recovery[1].Text != "second [REDACTED] details") {
				t.Fatalf("recovery did not retain redacted originals: %+v", result.Recovery)
			}
			encoded, err := json.Marshal(result)
			if err != nil || strings.Contains(string(encoded), "first [REDACTED] details") || strings.Contains(string(encoded), "second [REDACTED] details") || strings.Contains(string(encoded), "recover-call") {
				t.Fatalf("private recovery data was serialized: %s, %v", encoded, err)
			}
		})
	}
	if !slices.Equal(segments, []Segment{{ID: "first", Role: "user", Text: "first private details"}, {ID: "recovered", Kind: "tool_result", ToolCallID: "recover-call", Text: "recovered private details"}, {ID: "second", Role: "user", Text: "second private details"}}) {
		t.Fatal("evaluation changed the input segments")
	}
}

func TestCompressionSkipsExemptContent(t *testing.T) {
	config := `{"endpoint":"https://guardrail.example/scan","credential_ref":"credential","content_egress":true}`
	evaluator := EvaluatorFunc(func(context.Context, Revision, Evaluation) (Verdict, error) {
		t.Error("exempt content was sent for compression")
		return Verdict{Action: "pass"}, nil
	})
	engine := testEngine(t, "headroom", config, Input, Redact, false, evaluator)
	result, err := engine.Evaluate(t.Context(), Evaluation{
		Phase: Input, Segments: []Segment{{ID: "recovered", Text: "original"}},
		CompressionExemptIDs: []string{"recovered"}, WorkflowRecovery: true,
	}, nil)
	if err != nil || result.Changed || len(result.Recovery) != 0 || len(result.Segments) != 1 || result.Segments[0].Text != "original" || len(result.Decisions) != 1 || result.Decisions[0].Outcome != "skipped" {
		t.Fatalf("exempt content was changed: %+v, %v", result, err)
	}
	if testEngine(t, "headroom", config, Input, Redact, true, evaluator).HasCompression() {
		t.Fatal("monitoring compression should not enable recovery")
	}
}

func TestNativeToolResultsKeepRecoveryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, document, pointer, contentPointer, idPointer, callID string
	}{
		{"chat", `{"messages":[{"role":"tool","tool_call_id":"chat-call","content":"original"}]}`, "/messages/0", "/messages/0/content", "/messages/0/tool_call_id", "chat-call"},
		{"responses", `{"input":[{"type":"function_call_output","call_id":"responses-call","output":"original"}]}`, "/input/0", "/input/0/output", "/input/0/call_id", "responses-call"},
		{"anthropic", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"anthropic-call","content":[{"type":"text","text":"original"}]}]}]}`, "/messages/0/content/0", "/messages/0/content/0/content/0/text", "/messages/0/content/0/tool_use_id", "anthropic-call"},
		{"gemini", `{"contents":[{"role":"user","parts":[{"functionResponse":{"id":"gemini-call","name":"read","response":{"nested":{"text":"original"}}},"thoughtSignature":"signed"}]}]}`, "/contents/0/parts/0", "/contents/0/parts/0/functionResponse/response/nested/text", "/contents/0/parts/0/functionResponse/id", "gemini-call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document, err := oif.ParseJSON([]byte(tc.document), oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			surface, err := NativeSurface(document, false)
			if err != nil || len(surface.Segments) != 1 || surface.Segments[0].Kind != "tool_result" || surface.Segments[0].ToolCallID != tc.callID || !slices.Equal(surface.TextAt(tc.pointer), []string{"original"}) {
				t.Fatalf("tool result could not be identified: %+v, %v", surface, err)
			}
			changed := []Segment{{ID: surface.Segments[0].ID, Text: surface.Replacement(0, "[protected]")}}
			updated, err := surface.Apply(changed)
			if err != nil {
				t.Fatal(err)
			}
			content, found := updated.Lookup(tc.contentPointer)
			text, isText := content.Text()
			id, foundID := updated.Lookup(tc.idPointer)
			idText, isID := id.Text()
			if !found || !isText || text != "[protected]" || !foundID || !isID || idText != tc.callID || document.Raw() != tc.document {
				t.Fatalf("transformation lost content identity: %s", updated.Raw())
			}
		})
	}
}

func TestNativeToolArgumentReplacementPreservesSignedContent(t *testing.T) {
	const original = `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"read","args":{"secret":"original"}},"thoughtSignature":"signed"}]}]}`
	document, err := oif.ParseJSON([]byte(original), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	surface, err := NativeSurface(document, false)
	if err != nil || len(surface.Segments) != 1 || surface.Segments[0].Kind != "tool_call" || !slices.Equal(surface.TextAt("/contents/0/parts/0"), []string{`{"secret":"original"}`}) {
		t.Fatalf("unexpected inspectable tool input: %+v, %v", surface, err)
	}
	updated, err := surface.Apply([]Segment{{ID: surface.Segments[0].ID, Text: surface.Replacement(0, "[protected]")}})
	if err != nil {
		t.Fatal(err)
	}
	args, found := updated.Lookup("/contents/0/parts/0/functionCall/args")
	text, isText := args.Text()
	signature, foundSignature := updated.Lookup("/contents/0/parts/0/thoughtSignature")
	signatureText, isSignature := signature.Text()
	if !found || !isText || text != "[protected]" || !foundSignature || !isSignature || signatureText != "signed" || document.Raw() != original {
		t.Fatalf("replacement damaged native content: %s", updated.Raw())
	}
}

func testEngine(t testing.TB, kind string, config string, phase, action string, monitor bool, external Evaluator) *Engine {
	t.Helper()
	d := Revision{ID: "revision", GuardrailID: "guard", ProjectID: "project", Type: kind, Configuration: json.RawMessage(config), TimeoutMS: 1000, OnError: FailClosed}
	e, err := Compile([]Entry{{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: phase, Action: action, Monitor: monitor}}, map[string]Revision{d.ID: d}, external)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func evaluateText(t testing.TB, e *Engine, text, phase string, m *Masking) (Result, error) {
	t.Helper()
	return e.Evaluate(context.Background(), Evaluation{Phase: phase, Segments: []Segment{{ID: "text", Text: text}}}, m)
}

func TestDetectorCorpus(t *testing.T) {
	cases := []struct{ kind, category, positive, negative string }{
		{"builtin.pii", "pii.email", "メール alice@example.com", "alice at example dot com"},
		{"builtin.pii", "pii.phone", "+44 20 7946 0958", "12345"},
		{"builtin.pii", "pii.payment_card", "4111 1111 1111 1111", "4111 1111 1111 1112"},
		{"builtin.pii", "pii.iban", "GB82 WEST 1234 5698 7654 32 EXTRA", "GB83 WEST 1234 5698 7654 32"},
		{"builtin.pii", "pii.us_ssn", "123-45-6789", "000-45-6789"},
		{"builtin.pii", "pii.ip_address", "2001:db8::1", "999.999.999.999"},
		{"builtin.secrets", "secret.private_key", "-----BEGIN PRIVATE KEY-----\nABC\n-----END PRIVATE KEY-----", "-----BEGIN PUBLIC KEY-----ABC-----END PUBLIC KEY-----"},
		{"builtin.secrets", "secret.jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl", "eyJbroken.notjson.signature"},
		{"builtin.secrets", "secret.aws_access_key", "AKIAIOSFODNN7EXAMPLE", "AKIAshort"},
		{"builtin.secrets", "secret.google_cloud", "AIza" + strings.Repeat("a", 35), "AIza123"},
		{"builtin.secrets", "secret.azure", "AccountKey=" + strings.Repeat("a", 86) + "==", "AccountKey=abc"},
		{"builtin.secrets", "secret.github", githubFixture(strings.Repeat("a", 30)), "ghp_short"},
		{"builtin.secrets", "secret.gitlab", "glpat-" + strings.Repeat("x", 20), "glpat-short"},
		{"builtin.secrets", "secret.slack", "xoxb-" + strings.Repeat("x", 30), "xoxb-short"},
		{"builtin.secrets", "secret.stripe", "sk_live_" + strings.Repeat("a", 24), "pk_live_public"},
		{"builtin.secrets", "secret.openai", "sk-proj-" + strings.Repeat("a", 40), "sk-short"},
		{"builtin.secrets", "secret.anthropic", "sk-ant-api03-" + strings.Repeat("a", 40), "sk-ant-short"},
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			config, _ := json.Marshal(EntityConfig{Categories: []string{tc.category}})
			e := testEngine(t, tc.kind, string(config), Input, Block, false, nil)
			positive, err := evaluateText(t, e, tc.positive, Input, nil)
			if !errors.Is(err, ErrBlocked) || !positive.Blocked {
				t.Fatalf("missed positive: %s %+v", tc.category, positive)
			}
			negative, err := evaluateText(t, e, tc.negative, Input, nil)
			if err != nil || negative.Blocked {
				t.Fatalf("false positive: %s", tc.category)
			}
		})
	}
}

func TestSchemaDoesNotFetchRemoteReferences(t *testing.T) {
	d := Revision{Type: "builtin.schema", Configuration: json.RawMessage(`{"schema":{"$ref":"https://127.0.0.1/private"}}`), TimeoutMS: 100, OnError: FailClosed}
	if ValidateDefinition(d) == nil {
		t.Fatal("accepted remote schema")
	}
	e := testEngine(t, "builtin.schema", `{"schema":{"type":"object","required":["ok"],"properties":{"ok":{"const":true}},"additionalProperties":false}}`, Output, Block, false, nil)
	if _, err := evaluateText(t, e, `{"ok":true}`, Output, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluateText(t, e, `{"ok":false}`, Output, nil); !errors.Is(err, ErrBlocked) {
		t.Fatal(err)
	}
}
func TestToolsDenyOverridesAllowAndUntrustedResults(t *testing.T) {
	e := testEngine(t, "builtin.tools", `{"allow":["*"],"deny":["delete*"],"privileged":["admin*"],"untrusted":["web"],"schemas":{"read":{"type":"object","required":["path"]}}}`, Input, Block, false, nil)
	for _, segments := range [][]Segment{
		{{ID: "call", Kind: "tool_call", ToolName: "delete_all", Text: `{}`}},
		{{ID: "result", Kind: "tool_result", ToolName: "web", Text: "go"}, {ID: "call", Kind: "tool_call", ToolName: "admin_run", Text: `{}`}},
		{{ID: "call", Kind: "tool_call", ToolName: "read", Text: `{}`}},
	} {
		if _, err := e.Evaluate(t.Context(), Evaluation{Phase: Input, Segments: segments}, nil); !errors.Is(err, ErrBlocked) {
			t.Fatal(err)
		}
	}
}

func BenchmarkBuiltins100KTokens(b *testing.B) {
	e := testEngine(b, "builtin.pii", `{}`, Input, Block, false, nil)
	text := strings.Repeat("ordinary safe prompt words ", 20000)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = evaluateText(b, e, text, Input, nil)
	}
}
func BenchmarkUnconfigured(b *testing.B) {
	var e *Engine
	request := Evaluation{Phase: Input}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = e.Evaluate(context.Background(), request, nil)
	}
}

func githubFixture(entropy string) string {
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	n := crc32.ChecksumIEEE([]byte(entropy))
	checksum := []byte("000000")
	for i := 5; i >= 0; i-- {
		checksum[i] = alphabet[n%62]
		n /= 62
	}
	return "ghp_" + entropy + string(checksum)
}

func BenchmarkGuardrailStreaming(b *testing.B) {
	engine := testEngine(b, "builtin.keywords", `{"words":["forbidden"]}`, OutputStream, Block, false, nil)
	frame := []byte(strings.Repeat("ordinary safe text ", 256))
	text := string(frame)
	b.ReportAllocs()
	b.SetBytes(int64(len(frame) * 100))
	for b.Loop() {
		stream, err := NewStream(engine, Evaluation{}, DefaultWindowBytes)
		if err != nil {
			b.Fatal(err)
		}
		for range 100 {
			if _, err = stream.Push(b.Context(), frame, text); err != nil {
				b.Fatal(err)
			}
		}
		if _, err = stream.Finish(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}
