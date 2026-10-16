package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
)

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
