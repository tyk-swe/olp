package guardrails

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestVendorTransformationRequiresEveryFlaggedSegment(t *testing.T) {
	for _, failurePolicy := range []string{FailClosed, FailOpen} {
		t.Run(failurePolicy, func(t *testing.T) {
			runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Texts []string `json:"texts"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Texts) != 1 {
					t.Error("invalid request")
					w.WriteHeader(400)
					return
				}
				if request.Texts[0] == "private first" {
					io.WriteString(w, `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`)
				} else {
					io.WriteString(w, `{"action":"BLOCKED"}`)
				}
			})
			config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential"})
			definition := Revision{ID: "mixed", GuardrailID: "guard", Type: "generic_guardrail_api", Configuration: config, TimeoutMS: 1000, OnError: failurePolicy}
			engine, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "mixed", Phase: Input, Action: Redact}}, map[string]Revision{"mixed": definition}, runner)
			if err != nil {
				t.Fatal(err)
			}
			input := []Segment{{ID: "first", Text: "private first"}, {ID: "second", Text: "unsafe second"}}
			result, err := engine.Evaluate(t.Context(), Evaluation{Phase: Input, Segments: input}, nil)
			if err != ErrBlocked || !result.Blocked || result.Changed {
				t.Fatalf("partially sanitized unsafe content escaped: %+v error=%v", result, err)
			}
			if len(result.Decisions) != 1 || result.Decisions[0].Outcome != "failed_closed" {
				t.Fatalf("wrong evidence: %+v", result.Decisions)
			}
		})
	}
}

func TestNamedVendorPolicyCompilation(t *testing.T) {
	for _, tc := range []struct {
		name, kind, phase, action string
		monitor, valid            bool
	}{
		{"moderation_blocks", "openai_moderation", Input, Block, false, true},
		{"moderation_cannot_rewrite", "openai_moderation", Input, Redact, false, false},
		{"legacy_lakera_no_output", "lakera_v1", Output, Block, false, false},
		{"javelin_no_output", "javelin", OutputStream, Annotate, false, false},
		{"headroom_only_sync_input", "headroom", InputParallel, Block, false, false},
		{"transformation_sync_input", "guardrails_ai", Input, Mask, false, true},
		{"transformation_output", "guardrails_ai", OutputStream, Redact, false, true},
		{"parallel_cannot_rewrite", "guardrails_ai", InputParallel, Redact, false, false},
		{"parallel_monitors", "guardrails_ai", InputParallel, Redact, true, true},
		{"tools_block", "builtin.tool_permission", Input, Block, false, true},
		{"tools_no_rewrite", "builtin.tool_permission", Output, Redact, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := json.RawMessage(`{"endpoint":"https://guardrail.example/scan","credential_ref":"credential","content_egress":true}`)
			if tc.kind == "builtin.tool_permission" {
				config = json.RawMessage(`{}`)
			}
			definition := Revision{ID: "revision", GuardrailID: "guard", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
			_, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: tc.phase, Action: tc.action, Monitor: tc.monitor}}, map[string]Revision{"revision": definition}, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v", tc.valid, err)
			}
		})
	}
}

func TestCodeExecutionNativeIntentAndUnlabelledFence(t *testing.T) {
	for _, tc := range []struct{ name, phase, text string }{
		{"shell_request", Input, "please run `whoami`"},
		{"shell_request_with_fence", Input, "run `python` with this code:\n```python\nprint(1)\n```"},
		{"unlabelled_output", Output, "```\nprint(1)\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detector, err := compileCodeExecution(json.RawMessage(`{"confidence_threshold":0.8}`))
			if err != nil {
				t.Fatal(err)
			}
			verdict, err := detector.detect(t.Context(), Evaluation{Phase: tc.phase, Segments: []Segment{{ID: "s", Text: tc.text}}})
			if err != nil || verdict.Action != Block {
				t.Fatalf("execution escaped: %+v error=%v", verdict, err)
			}
		})
	}
}
