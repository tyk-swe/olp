package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
)

func vendorConfiguration(endpoint, parameters string) json.RawMessage {
	return json.RawMessage(`{"endpoint":` + strconv.Quote(endpoint) + `,"credential_ref":"credential","content_egress":true` + strings.TrimSuffix(`,"parameters":`+parameters, `,"parameters":`) + `}`)
}

func fakeRunner(t *testing.T, handler http.HandlerFunc) (*ExternalRunner, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &ExternalRunner{Egress: egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}, Clients: egress.NewConnectionClientCache(8), Credentials: func(context.Context, string) ([]byte, error) {
		return []byte(`{"api_key":"opaque-api-key","signing_key":"01234567890123456789012345678901","access_key_id":"ACCESS","secret_access_key":"SECRET"}`), nil
	}}, server.URL
}

func TestNamedVendorFailurePoliciesAndCircuitIsolation(t *testing.T) {
	var calls atomic.Int32
	runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"content":"sensitive","results":[{}]}`)
	})
	config := vendorConfiguration(endpoint, "")
	for _, policy := range []OnError{FailOpen, FailClosed} {
		d := Revision{ID: string(policy), GuardrailID: "g", Type: "openai_moderation", Configuration: config, TimeoutMS: 1000, OnError: policy}
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
	config := vendorConfiguration(endpoint, "")
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
				return blockIf(true, "policy.test"), nil
			})}
			engine := testEngine(t, Kind(tc.kind), tc.config, Input, Block, false, runner)
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

func TestVendorTransformationRequiresEveryFlaggedSegment(t *testing.T) {
	for _, failurePolicy := range []OnError{FailClosed, FailOpen} {
		t.Run(string(failurePolicy), func(t *testing.T) {
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
			config := vendorConfiguration(endpoint, "")
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

func TestVendorInterventionWithoutReplacement(t *testing.T) {
	for _, kind := range []string{"generic_guardrail_api", "deepkeep", "qostodian_nexus", "noma_v2", "straiker", "ovalix"} {
		for _, action := range []Action{Block, Redact} {
			for _, policy := range []OnError{FailClosed, FailOpen} {
				t.Run(kind+"/"+string(action)+"/"+string(policy), func(t *testing.T) {
					runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, _ *http.Request) {
						if kind == "ovalix" {
							io.WriteString(w, `{"action_type":"anonymize","modified_data":{"content":"unchanged"}}`)
							return
						}
						io.WriteString(w, `{"action":"GUARDRAIL_INTERVENED","texts":["unchanged"]}`)
					})
					parameters := json.RawMessage(`{}`)
					if kind == "deepkeep" {
						parameters = json.RawMessage(`{"firewall_id":"firewall"}`)
					} else if kind == "ovalix" {
						parameters = json.RawMessage(`{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`)
					}
					config := vendorConfiguration(endpoint, string(parameters))
					definition := Revision{ID: "revision", GuardrailID: "guard", Type: Kind(kind), Configuration: config, TimeoutMS: 1000, OnError: policy}
					engine, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: Input, Action: action}}, map[string]Revision{"revision": definition}, runner)
					if err != nil {
						t.Fatal(err)
					}
					result, err := evaluateText(t, engine, "unchanged", Input, nil)
					if err != ErrBlocked || !result.Blocked || result.Changed || len(result.Decisions) != 1 || len(result.Decisions[0].Categories) == 0 {
						t.Fatalf("explicit intervention lost: %+v error=%v", result, err)
					}
				})
			}
		}
	}
}

func TestConductIdentityFailuresRemainPolicyBlocks(t *testing.T) {
	for _, reply := range []string{"error", "structured", "unavailable"} {
		for _, policy := range []OnError{FailOpen, FailClosed} {
			t.Run(reply+"/"+string(policy), func(t *testing.T) {
				runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					_ = json.NewDecoder(r.Body).Decode(&request)
					response := map[string]any{"jsonrpc": "2.0", "id": request["id"]}
					switch reply {
					case "error":
						response["error"] = map[string]any{"code": -32000, "data": map[string]bool{"identity_required": true}}
					case "structured":
						response["result"] = map[string]any{"structuredContent": map[string]bool{"identity_required": true}}
					case "unavailable":
						response["result"] = map[string]any{"content": []any{map[string]string{"type": "text", "text": "ok"}}}
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(response)
				})
				config := vendorConfiguration(endpoint, "")
				definition := Revision{ID: "revision", GuardrailID: "guard", Type: "conduct", Configuration: config, TimeoutMS: 1000, OnError: policy}
				engine, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: Input, Action: Block}}, map[string]Revision{"revision": definition}, runner)
				if err != nil {
					t.Fatal(err)
				}
				result, err := evaluateText(t, engine, "content", Input, nil)
				wantBlocked := reply != "unavailable" || policy == FailClosed
				if result.Blocked != wantBlocked || (err == ErrBlocked) != wantBlocked || len(result.Decisions) != 1 {
					t.Fatalf("identity failure result=%+v error=%v", result, err)
				}
				if reply != "unavailable" && result.Decisions[0].Outcome != "blocked" {
					t.Fatalf("identity denial became an availability error: %+v", result.Decisions)
				}
			})
		}
	}
}

func TestLassoTransformationRequiresEveryFinding(t *testing.T) {
	for _, tc := range []struct {
		name, findings string
		blocked        bool
	}{
		{"mask_only", `{"pii":[{"action":"AUTO_MASKING"}]}`, false},
		{"warning_same_category", `{"pii":[{"action":"AUTO_MASKING"},{"action":"WARN"}]}`, true},
		{"warning_other_category", `{"pii":[{"action":"AUTO_MASKING"}],"jailbreak":[{"action":"WARN"}]}`, true},
		{"block_other_category", `{"pii":[{"action":"AUTO_MASKING"}],"jailbreak":[{"action":"BLOCK"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, policy := range []OnError{FailClosed, FailOpen} {
				t.Run(string(policy), func(t *testing.T) {
					runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, _ *http.Request) {
						io.WriteString(w, `{"violations_detected":true,"findings":`+tc.findings+`,"messages":[{"content":"masked"}]}`)
					})
					config := vendorConfiguration(endpoint, `{"mask":true}`)
					definition := Revision{ID: "revision", GuardrailID: "guard", Type: "lasso", Configuration: config, TimeoutMS: 1000, OnError: policy}
					engine, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: Input, Action: Redact}}, map[string]Revision{"revision": definition}, runner)
					if err != nil {
						t.Fatal(err)
					}
					result, err := evaluateText(t, engine, "private and unsafe", Input, nil)
					if tc.blocked {
						if err != ErrBlocked || !result.Blocked || result.Changed {
							t.Fatalf("partial repair escaped: %+v error=%v", result, err)
						}
					} else if err != nil || result.Blocked || !result.Changed || result.Segments[0].Text != "masked" {
						t.Fatalf("complete repair failed: %+v error=%v", result, err)
					}
				})
			}
		})
	}
}

func TestNamedVendorPolicyCompilation(t *testing.T) {
	for _, tc := range []struct {
		name, kind     string
		phase          Phase
		action         Action
		monitor, valid bool
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
			definition := Revision{ID: "revision", GuardrailID: "guard", Type: Kind(tc.kind), Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
			_, err := Compile([]Entry{{GuardrailID: "guard", RevisionID: "revision", Phase: tc.phase, Action: tc.action, Monitor: tc.monitor}}, map[string]Revision{"revision": definition}, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t error=%v", tc.valid, err)
			}
		})
	}
}

func TestVendorRequestsUseCanonicalRoles(t *testing.T) {
	for _, tc := range []struct {
		phase Phase
		role  Role
		want  string
	}{
		{Input, Developer, "system"},
		{Input, System, "system"},
		{Input, Assistant, "assistant"},
		{Input, Tool, "user"},
		{Input, "", "user"},
		{Output, User, "assistant"},
	} {
		t.Run(string(tc.phase)+"/"+string(tc.role), func(t *testing.T) {
			runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Role string `json:"role"`
					} `json:"messages"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) != 1 || body.Messages[0].Role != tc.want {
					t.Errorf("messages %+v, want role %s", body.Messages, tc.want)
				}
				_, _ = io.WriteString(w, `{"required_action":{},"analysis_result":{}}`)
			})
			d := Revision{ID: "revision", GuardrailID: "guard", Type: "aim", Configuration: vendorConfiguration(endpoint, ""), TimeoutMS: 1000, OnError: FailClosed}
			if _, err := runner.Evaluate(t.Context(), d, Evaluation{Phase: tc.phase, Segments: []Segment{{ID: "s", Role: tc.role, Text: "text"}}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
