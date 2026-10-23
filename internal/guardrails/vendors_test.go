package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoreVendorContracts(t *testing.T) {
	for _, tc := range []struct {
		name, kind, phase, response, action, replacement string
	}{
		{"moderation_block", "openai_moderation", Input, `{"results":[{"flagged":true}]}`, Block, ""},
		{"moderation_pass", "openai_moderation", Output, `{"results":[{"flagged":false}]}`, "pass", ""},
		{"moderation_missing", "openai_moderation", Input, `{"results":[{}]}`, "error", ""},
		{"moderation_null", "openai_moderation", Input, `null`, "error", ""},
		{"generic_block", "generic_guardrail_api", Input, `{"action":"BLOCKED","texts":["replacement"]}`, Block, ""},
		{"generic_pass", "generic_guardrail_api", Input, `{"action":"NONE"}`, "pass", ""},
		{"generic_replace", "generic_guardrail_api", Output, `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`, Block, "safe"},
		{"generic_empty_replace", "generic_guardrail_api", Output, `{"action":"GUARDRAIL_INTERVENED","texts":[""]}`, Block, ""},
		{"generic_missing", "generic_guardrail_api", Input, `{}`, "error", ""},
		{"generic_no_replacement", "generic_guardrail_api", Input, `{"action":"GUARDRAIL_INTERVENED"}`, "error", ""},
		{"generic_null_replacement", "generic_guardrail_api", Input, `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`, "error", ""},
		{"generic_wrong_count", "generic_guardrail_api", Input, `{"action":"NONE","texts":["a","b"]}`, "error", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid vendor request")
				}
				if tc.kind == "openai_moderation" {
					if r.Header.Get("Authorization") != "Bearer opaque-api-key" || string(body["input"]) != `"éabc"` {
						t.Error("moderation authentication or input lost")
					}
				} else {
					inputType := `"request"`
					if tc.phase == Output {
						inputType = `"response"`
					}
					if r.Header.Get("x-api-key") != "opaque-api-key" || string(body["texts"]) != `["éabc"]` || string(body["input_type"]) != inputType {
						t.Error("generic authentication or phase mapping lost")
					}
				}
				_, _ = io.WriteString(w, tc.response)
			})
			config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential"})
			d := Revision{ID: tc.name, GuardrailID: "g", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
			v, err := runner.Evaluate(t.Context(), d, Evaluation{Phase: tc.phase, Segments: []Segment{{ID: "s", Text: "éabc"}}})
			if tc.action == "error" {
				if !errors.Is(err, errEvaluation) {
					t.Fatalf("expected sanitized failure: %v", err)
				}
				return
			}
			if err != nil || v.Action != tc.action {
				t.Fatalf("verdict=%+v error=%v", v, err)
			}
			if strings.Contains(tc.name, "replace") && !strings.Contains(tc.name, "replacement") {
				if len(v.Redactions) != 1 || v.Redactions[0].Start != 0 || v.Redactions[0].End != len("éabc") || v.Redactions[0].Replacement != tc.replacement {
					t.Fatalf("invalid Unicode replacement: %+v", v)
				}
			}
		})
	}
}

func TestNamedVendorFailurePoliciesAndCircuitIsolation(t *testing.T) {
	var calls atomic.Int32
	runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"content":"sensitive","results":[{}]}`)
	})
	config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential"})
	for _, policy := range []string{FailOpen, FailClosed} {
		d := Revision{ID: policy, GuardrailID: "g", Type: "openai_moderation", Configuration: config, TimeoutMS: 1000, OnError: policy}
		engine, err := Compile([]Entry{{GuardrailID: "g", RevisionID: d.ID, Phase: Input, Action: Block}}, map[string]Revision{d.ID: d}, runner)
		if err != nil {
			t.Fatal(err)
		}
		for range 6 {
			result, err := engine.Evaluate(t.Context(), Evaluation{Phase: Input, Segments: []Segment{{ID: "s", Text: "secret"}}}, nil)
			if (policy == FailClosed) != (err != nil) || len(result.Decisions) != 1 {
				t.Fatalf("failure policy %s: %+v, %v", policy, result, err)
			}
			encoded, _ := json.Marshal(result.Decisions)
			if strings.Contains(string(encoded), "sensitive") || strings.Contains(string(encoded), "secret") {
				t.Fatal("vendor payload leaked into evidence")
			}
		}
	}
	if calls.Load() != 10 {
		t.Fatalf("circuits were not isolated per revision or did not open: %d calls", calls.Load())
	}
}

func TestExternalDeadlineIncludesCredentialResolution(t *testing.T) {
	runner, endpoint := fakeRunner(t, func(http.ResponseWriter, *http.Request) { t.Error("request after credential deadline") })
	runner.Credentials = func(ctx context.Context, _ string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("credential lookup has no deadline")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "credential"})
	d := Revision{ID: "timeout", GuardrailID: "g", Type: "openai_moderation", Configuration: config, TimeoutMS: 5, OnError: FailClosed}
	started := time.Now()
	_, err := runner.Evaluate(t.Context(), d, Evaluation{Phase: Input, Segments: []Segment{{ID: "s", Text: "secret"}}})
	if !errors.Is(err, errEvaluation) || time.Since(started) > time.Second {
		t.Fatalf("deadline was not enforced: %v", err)
	}
}

func TestModelGuardrailAliases(t *testing.T) {
	for _, tc := range []struct{ kind, config string }{
		{"decision_model", `{"route":"judge","rule":{"pointer":"/blocked","equals":true,"category":"policy.decision"}}`},
		{"semantic_guard", `{"route":"embeddings","semantic":{"category":"policy.semantic","similarity_threshold":0.8,"utterances":["unsafe request"]}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			calls := 0
			runner := &ExternalRunner{Model: EvaluatorFunc(func(context.Context, Revision, Evaluation) (Verdict, error) {
				calls++
				return vendorBoolean(true, "policy.test"), nil
			})}
			engine := testEngine(t, tc.kind, tc.config, Input, Block, false, runner)
			evaluation := Evaluation{Phase: Input, Segments: []Segment{{ID: "s", Text: "text"}}, InternalModel: true}
			result, err := engine.Evaluate(t.Context(), evaluation, nil)
			if err != nil || calls != 0 || len(result.Decisions) != 1 || result.Decisions[0].Outcome != "skipped" {
				t.Fatalf("recursive model was not skipped: %+v %v", result, err)
			}
			evaluation.InternalModel = false
			_, err = engine.Evaluate(t.Context(), evaluation, nil)
			if !errors.Is(err, ErrBlocked) || calls != 1 {
				t.Fatalf("accounted model evaluator was not selected: calls=%d error=%v", calls, err)
			}
		})
	}
}
