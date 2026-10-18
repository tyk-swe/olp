package operationplan_test

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
)

func decisionPlan(t *testing.T, policy *contentpolicy.Policy) *operationplan.Plan {
	t.Helper()
	c := config("systemone", "decision")
	c.Provider.VendorID = "typesafe-jev"
	c.Policy = policy
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	source, err := operationplan.Parse("openai-decisions", []byte(`{"model":"route","input":"hello","questions":[{"name":"secret","type":"predicate","instructions":"Is this text?"}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

const decisionResponse = `{"model":"vendor-model","answers":{"q0":{"type":"noul","noul":0.75}},"usage":{"input_tokens":2,"output_tokens":1}}`

func TestMappedDecisionNamesRemainInspectable(t *testing.T) {
	p := decisionPlan(t, nil)
	input, err := p.GuardrailInput()
	if err != nil || !slices.ContainsFunc(input, func(v operations.Text) bool { return v.Value == "secret" }) {
		t.Fatalf("source name not inspected: %+v %v", input, err)
	}
	result, err := p.Decode([]byte(decisionResponse))
	if err != nil {
		t.Fatal(err)
	}
	output, err := p.GuardrailOutput(result)
	if err != nil || !slices.ContainsFunc(output, func(v operations.Text) bool { return v.Value == "secret" }) {
		t.Fatalf("restored name not inspected: %+v %v", output, err)
	}
}

func TestMappedDecisionNamesRespectBlockingPolicy(t *testing.T) {
	for _, phase := range []string{contentpolicy.PhaseInput, contentpolicy.PhaseOutput} {
		t.Run(phase, func(t *testing.T) {
			p := decisionPlan(t, &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "secret", Phase: phase, Action: contentpolicy.ActionBlock, Pattern: "^secret$"}}})
			if phase == contentpolicy.PhaseInput {
				if _, err := p.CheckInput(); err == nil {
					t.Fatal("source name bypassed input policy")
				}
			} else if _, err := p.Decode([]byte(decisionResponse)); err == nil {
				t.Fatal("restored name bypassed output policy")
			}
		})
	}
}

func TestDecisionInputPolicyIgnoresRoutingMetadata(t *testing.T) {
	p := decisionPlan(t, &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "metadata", Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Pattern: "^(vendor-model|route|model)$"}}})
	if _, err := p.CheckInput(); err != nil {
		t.Fatalf("routing metadata was treated as input: %v", err)
	}
	texts, err := p.GuardrailInput()
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(texts, func(v operations.Text) bool {
		return v.Value == "vendor-model" || v.Value == "route" || v.Value == "model"
	}) {
		t.Fatalf("routing metadata was exposed as text: %+v", texts)
	}
	if !slices.ContainsFunc(texts, func(v operations.Text) bool { return v.Value == "secret" }) {
		t.Fatal("question name lost inspection coverage")
	}
}
