package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tyk-swe/olp/internal/oif"
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

func TestActionsAndRequestLocalMasking(t *testing.T) {
	for _, action := range []string{Block, Redact, Mask, Annotate} {
		for _, monitor := range []bool{false, true} {
			t.Run(action+map[bool]string{true: "_monitor"}[monitor], func(t *testing.T) {
				e := testEngine(t, "builtin.regex", `{"rules":[{"id":"account","pattern":"secret","replacement":"$1"}]}`, Input, action, monitor, nil)
				m := &Masking{}
				r, err := evaluateText(t, e, "a secret", Input, m)
				if monitor || action == Annotate {
					if err != nil || r.Segments[0].Text != "a secret" || r.Changed {
						t.Fatalf("evaluation-only mutation: %+v %v", r, err)
					}
				} else if action == Block {
					if !errors.Is(err, ErrBlocked) || !r.Blocked {
						t.Fatal(r, err)
					}
				} else if action == Redact {
					if err != nil || r.Segments[0].Text != "a $1" {
						t.Fatal(r, err)
					}
				} else {
					if err != nil || r.Segments[0].Text == "a secret" || m.Restore(r.Segments[0].Text) != "a secret" {
						t.Fatal(r, err)
					}
					if (&Masking{}).Restore(r.Segments[0].Text) != r.Segments[0].Text {
						t.Fatal("mapping escaped its request")
					}
				}
				data, _ := json.Marshal(r.Decisions)
				if strings.Contains(string(data), "secret") || strings.Contains(string(data), "$1") {
					t.Fatal("content in evidence")
				}
				if err := ValidateEvidence(r.Decisions); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
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
func TestUnicodeKeywordAutomaton(t *testing.T) {
	e := testEngine(t, "builtin.keywords", `{"words":["strasse","ΣΊΓΜΑ","秘密"]}`, Input, Block, false, nil)
	for _, value := range []string{"Straße", "σίγμα", "秘密"} {
		if _, err := evaluateText(t, e, value, Input, nil); !errors.Is(err, ErrBlocked) {
			t.Fatalf("missed %q: %v", value, err)
		}
	}
	if _, err := evaluateText(t, e, "astrasseb", Input, nil); err != nil {
		t.Fatal("word boundary false positive", err)
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
func TestFailurePoliciesAndDeadline(t *testing.T) {
	d := Revision{ID: "revision", GuardrailID: "guard", Type: "webhook", Configuration: json.RawMessage(`{"endpoint":"https://example.com","credential_ref":"key","content_egress":true}`), TimeoutMS: 1, OnError: FailClosed}
	for _, policy := range []string{FailOpen, FailClosed} {
		d.OnError = policy
		e, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: Input, Action: Block}}, map[string]Revision{d.ID: d}, EvaluatorFunc(func(ctx context.Context, _ Revision, _ Evaluation) (Verdict, error) {
			<-ctx.Done()
			return Verdict{}, ctx.Err()
		}))
		if err != nil {
			t.Fatal(err)
		}
		r, err := evaluateText(t, e, "private", Input, nil)
		if r.Blocked != (policy == FailClosed) || (err != nil) != (policy == FailClosed) {
			t.Fatal(r, err)
		}
	}
}
func TestStreamSplitMatchAndStrictFrames(t *testing.T) {
	e := testEngine(t, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`, OutputStream, Block, false, nil)
	s, err := NewStream(e, Evaluation{}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.Push(t.Context(), []byte("first"), "sec"); err != nil || len(out) != 0 {
		t.Fatal(out, err)
	}
	if _, err := s.Push(t.Context(), []byte("second"), "ret"); !errors.Is(err, ErrBlocked) {
		t.Fatal("split match released", err)
	}
	s, _ = NewStream(e, Evaluation{}, 256)
	frames := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	var emitted [][]byte
	for _, frame := range frames {
		out, err := s.Push(t.Context(), frame, "ordinary text ")
		if err != nil {
			t.Fatal(err)
		}
		emitted = append(emitted, out...)
	}
	last, err := s.Finish(t.Context())
	emitted = append(emitted, last...)
	if err != nil || len(emitted) != len(frames) {
		t.Fatal(emitted, err)
	}
	for i := range frames {
		if string(emitted[i]) != string(frames[i]) {
			t.Fatal("strict frame changed")
		}
	}
	unbounded := testEngine(t, "builtin.regex", `{"rules":[{"id":"all","pattern":"a.*z"}]}`, OutputStream, Block, false, nil)
	if _, err := NewStream(unbounded, Evaluation{}, 256); !errors.Is(err, ErrSurface) {
		t.Fatal("unbounded rule accepted", err)
	}
}
func TestCompilationPinsConfiguration(t *testing.T) {
	d := Revision{ID: "r", GuardrailID: "g", Type: "builtin.regex", Configuration: json.RawMessage(`{"rules":[{"id":"x","pattern":"one"}]}`), TimeoutMS: 100, OnError: FailClosed}
	e, err := Compile([]Entry{{GuardrailID: "g", RevisionID: "r", Phase: Input, Action: Block}}, map[string]Revision{"r": d}, nil)
	if err != nil {
		t.Fatal(err)
	}
	copy(d.Configuration, strings.Repeat(" ", len(d.Configuration)))
	if _, err := evaluateText(t, e, "one", Input, nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("mutable configuration", err)
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

func TestIBANRegistryChecksums(t *testing.T) {
	for _, value := range []string{"BI4210000100010000332045181", "DJ2100010000000154000100186", "EG380019000500000000263180002", "FK88SC123456789012", "HN88CABF00000000000250005469", "LY83002048000020100120361", "MN121234123456789123", "NI45BAPR00000013000003558124", "OM810180000001299123456", "RU0304452522540817810538091310419", "SD2129010501234001", "SO211000001001000100141", "YE15CBYE0001018861234567891234"} {
		if !validIBAN(value) {
			t.Errorf("missed registered IBAN %s", value[:2])
		}
		invalid := value[:2] + "00" + value[4:]
		if validIBAN(invalid) {
			t.Errorf("accepted invalid checksum %s", value[:2])
		}
	}
}

func TestPhaseActionMatrix(t *testing.T) {
	for _, phase := range []string{Input, InputParallel, Output, OutputStream} {
		for _, action := range []string{Block, Redact, Mask, Annotate} {
			for _, monitor := range []bool{false, true} {
				t.Run(phase+"/"+action+map[bool]string{true: "/monitor"}[monitor], func(t *testing.T) {
					d := Revision{ID: "r", GuardrailID: "g", Type: "builtin.regex", Configuration: json.RawMessage(`{"rules":[{"id":"s","pattern":"secret","replacement":"hidden"}]}`), TimeoutMS: 1000, OnError: FailClosed}
					e, err := Compile([]Entry{{GuardrailID: "g", RevisionID: "r", Phase: phase, Action: action, Monitor: monitor}}, map[string]Revision{"r": d}, nil)
					incompatible := action == Mask && phase != Input || action == Redact && phase == InputParallel && !monitor
					if incompatible {
						if err == nil {
							t.Fatal("incompatible action accepted")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					result, err := evaluateText(t, e, "secret", phase, &Masking{})
					if monitor || action == Annotate {
						if err != nil || result.Changed || result.Segments[0].Text != "secret" {
							t.Fatal(result, err)
						}
					} else if action == Block {
						if !errors.Is(err, ErrBlocked) {
							t.Fatal(result, err)
						}
					} else if err != nil || !result.Changed {
						t.Fatal(result, err)
					}
				})
			}
		}
	}
}
func TestStreamingRedactionAndMaskRestoration(t *testing.T) {
	e := testEngine(t, "builtin.regex", `{"rules":[{"id":"s","pattern":"secret","replacement":"hidden"}]}`, OutputStream, Redact, false, nil)
	stream, err := NewStream(e, Evaluation{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	stream.Rewriter(func(_ []byte, text string) ([]byte, error) { return []byte(text), nil })
	var output []byte
	for _, chunk := range []string{"sec", "ret"} {
		frames, err := stream.Push(t.Context(), []byte(chunk), chunk)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			output = append(output, f...)
		}
	}
	frames, err := stream.Finish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		output = append(output, f...)
	}
	if string(output) != "hidden" {
		t.Fatalf("stream redaction: %q", output)
	}
	masks := &Masking{}
	e = testEngine(t, "builtin.pii", `{"categories":["pii.email"]}`, Input, Mask, false, nil)
	masked, err := evaluateText(t, e, "alice@example.com", Input, masks)
	if err != nil {
		t.Fatal(err)
	}
	stream, err = NewStream(e, Evaluation{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	stream.Masking(masks)
	stream.Rewriter(func(_ []byte, text string) ([]byte, error) { return []byte(text), nil })
	token := masked.Segments[0].Text
	output = nil
	for _, chunk := range []string{token[:10], token[10:]} {
		frames, err := stream.Push(t.Context(), []byte(chunk), chunk)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			output = append(output, f...)
		}
	}
	frames, err = stream.Finish(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		output = append(output, f...)
	}
	if string(output) != "alice@example.com" {
		t.Fatalf("stream restoration failed: %q", output)
	}
}

func TestNativeToolArgumentEscapesCannotBypassInspection(t *testing.T) {
	document, err := oif.ParseJSON([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"send","arguments":"{\"address\":\"\\u0061lice@example.com\"}"}}]}]}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	surface, err := NativeSurface(document, false)
	if err != nil {
		t.Fatal(err)
	}
	engine := testEngine(t, "builtin.pii", `{"categories":["pii.email"]}`, Input, Block, false, nil)
	_, err = engine.Evaluate(t.Context(), Evaluation{Phase: Input, Segments: surface.Segments}, nil)
	if !errors.Is(err, ErrBlocked) {
		t.Fatal("escaped tool argument bypassed inspection", err)
	}
	preserved, err := surface.Apply(surface.Segments)
	if err != nil || string(preserved.Bytes()) != string(document.Bytes()) {
		t.Fatal("inspection changed native bytes")
	}
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

func TestKeywordWordCannotMatchHalfOfUnicodeExpansion(t *testing.T) {
	engine := testEngine(t, "builtin.keywords", `{"words":["s"]}`, Input, Block, false, nil)
	if _, err := evaluateText(t, engine, "ß", Input, nil); err != nil {
		t.Fatal("one letter matched half of a case-fold expansion")
	}
	engine = testEngine(t, "builtin.keywords", `{"words":["ss"]}`, Input, Block, false, nil)
	if _, err := evaluateText(t, engine, "ß", Input, nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("whole case-fold expansion did not match")
	}
}
