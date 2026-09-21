package contentpolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, raw string) *Policy {
	t.Helper()
	policy, err := Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Decode(%s) failed: %v", raw, err)
	}
	return policy
}

func decodeErr(t *testing.T, raw string) error {
	t.Helper()
	_, err := Decode(json.RawMessage(raw))
	if err == nil {
		t.Fatalf("Decode(%s) unexpectedly succeeded", raw)
	}
	return err
}

func TestDecodeValidPolicy(t *testing.T) {
	policy := decode(t, `{"rules":[
		{"id":"mask-secret","phase":"input","pattern":"s3cr3t","action":"redact","replacement":"***"},
		{"id":"block-topic","phase":"output","pattern":"forbidden","action":"block"},
		{"id":"default-replacement","phase":"input","pattern":"token","action":"redact"}
	]}`)
	if len(policy.Rules) != 3 {
		t.Fatalf("rules=%d", len(policy.Rules))
	}
	if policy.Rules[0].Replacement != "***" {
		t.Fatalf("replacement=%q", policy.Rules[0].Replacement)
	}
	if policy.Rules[2].Replacement != DefaultReplacement {
		t.Fatalf("default replacement=%q", policy.Rules[2].Replacement)
	}
	if policy.Rules[1].Replacement != "" {
		t.Fatalf("block rule replacement=%q", policy.Rules[1].Replacement)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"not an object":          `[{"id":"a"}]`,
		"trailing document":      `{"rules":[]} {"rules":[]}`,
		"trailing token":         `{"rules":[]} true`,
		"unknown field":          `{"rules":[],"extra":1}`,
		"rule unknown field":     `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"block","bogus":1}]}`,
		"missing id":             `{"rules":[{"phase":"input","pattern":"x","action":"block"}]}`,
		"bad id leading digit":   `{"rules":[{"id":"1bad","phase":"input","pattern":"x","action":"block"}]}`,
		"bad id character":       `{"rules":[{"id":"has space","phase":"input","pattern":"x","action":"block"}]}`,
		"duplicate id":           `{"rules":[{"id":"dup","phase":"input","pattern":"x","action":"block"},{"id":"dup","phase":"output","pattern":"y","action":"block"}]}`,
		"missing phase":          `{"rules":[{"id":"r1","pattern":"x","action":"block"}]}`,
		"bad phase":              `{"rules":[{"id":"r1","phase":"side","pattern":"x","action":"block"}]}`,
		"missing action":         `{"rules":[{"id":"r1","phase":"input","pattern":"x"}]}`,
		"bad action":             `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"drop"}]}`,
		"missing pattern":        `{"rules":[{"id":"r1","phase":"input","action":"block"}]}`,
		"empty pattern":          `{"rules":[{"id":"r1","phase":"input","pattern":"","action":"block"}]}`,
		"invalid regex":          `{"rules":[{"id":"r1","phase":"input","pattern":"a(","action":"block"}]}`,
		"empty match star":       `{"rules":[{"id":"r1","phase":"input","pattern":"a*","action":"block"}]}`,
		"empty match anchor":     `{"rules":[{"id":"r1","phase":"input","pattern":"^","action":"block"}]}`,
		"empty match optional":   `{"rules":[{"id":"r1","phase":"input","pattern":"(a)?","action":"block"}]}`,
		"block with replacement": `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"block","replacement":"y"}]}`,
		"oversized replacement":  `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"redact","replacement":"` + strings.Repeat("r", 129) + `"}]}`,
		"oversized pattern":      `{"rules":[{"id":"r1","phase":"input","pattern":"` + strings.Repeat("p", 513) + `","action":"block"}]}`,
	}
	for name, raw := range cases {
		if err := decodeErr(t, raw); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestDecodeRejectsTooManyRules(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`{"rules":[`)
	for i := 0; i < MaxRules+1; i++ {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"id":"rule-` + strings.Repeat("x", 1) + string(rune('a'+i%26)) + strings.Repeat("y", i/26) + `","phase":"input","pattern":"p` + strings.Repeat("z", i) + `q","action":"block"}`)
	}
	builder.WriteString(`]}`)
	decodeErr(t, builder.String())
}

func TestDecodeRejectsPatternBudget(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`{"rules":[`)
	for i := 0; i < 33; i++ {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"id":"rule-` + string(rune('a'+i)) + `","phase":"input","pattern":"` + strings.Repeat("p", 512) + `","action":"block"}`)
	}
	builder.WriteString(`]}`)
	decodeErr(t, builder.String())
}

func TestValidateStoredPolicy(t *testing.T) {
	if err := Validate(nil); err != nil {
		t.Fatalf("nil policy: %v", err)
	}
	valid := &Policy{Rules: []Rule{{ID: "r1", Phase: PhaseInput, Pattern: "x", Action: ActionBlock}}}
	if err := Validate(valid); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	invalid := &Policy{Rules: []Rule{{ID: "r1", Phase: PhaseInput, Pattern: "a(", Action: ActionBlock}}}
	if err := Validate(invalid); err == nil {
		t.Fatal("invalid stored pattern accepted")
	}
}

func TestCompilePartitionsAndPreservesOrder(t *testing.T) {
	policy := &Policy{Rules: []Rule{
		{ID: "out-1", Phase: PhaseOutput, Pattern: "a", Action: ActionBlock},
		{ID: "in-1", Phase: PhaseInput, Pattern: "b", Action: ActionRedact, Replacement: "R"},
		{ID: "out-2", Phase: PhaseOutput, Pattern: "c", Action: ActionRedact, Replacement: "S"},
		{ID: "in-2", Phase: PhaseInput, Pattern: "d", Action: ActionBlock},
	}}
	compiled, err := Compile(policy)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(compiled.Input) != 2 || compiled.Input[0].ID != "in-1" || compiled.Input[1].ID != "in-2" {
		t.Fatalf("input order: %+v", compiled.Input)
	}
	if len(compiled.Output) != 2 || compiled.Output[0].ID != "out-1" || compiled.Output[1].ID != "out-2" {
		t.Fatalf("output order: %+v", compiled.Output)
	}
	if !compiled.HasInput() || !compiled.HasOutput() {
		t.Fatal("phase flags unset")
	}
}

func TestCompileNilAndEmpty(t *testing.T) {
	if compiled, err := Compile(nil); err != nil || compiled != nil {
		t.Fatalf("nil: %v %v", compiled, err)
	}
	if compiled, err := Compile(&Policy{Rules: []Rule{}}); err != nil || compiled != nil {
		t.Fatalf("empty: %v %v", compiled, err)
	}
	var empty *Compiled
	if empty.HasInput() || empty.HasOutput() {
		t.Fatal("nil compiled reports rules")
	}
}

func TestValidateDecisions(t *testing.T) {
	valid := []Decision{{RuleID: "r1", Phase: PhaseInput, Action: ActionRedact, Outcome: OutcomeRedacted}}
	if err := ValidateDecisions(valid); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for i, d := range []Decision{
		{RuleID: "bad id", Phase: PhaseInput, Action: ActionBlock, Outcome: OutcomeBlocked},
		{RuleID: "r1", Phase: "side", Action: ActionBlock, Outcome: OutcomeBlocked},
		{RuleID: "r1", Phase: PhaseInput, Action: "drop", Outcome: OutcomeBlocked},
		{RuleID: "r1", Phase: PhaseInput, Action: ActionBlock, Outcome: "ignored"},
	} {
		if err := ValidateDecisions([]Decision{d}); err != nil {
			continue
		}
		t.Fatalf("case %d accepted: %+v", i, d)
	}
	tooMany := make([]Decision, MaxDecisions+1)
	for i := range tooMany {
		tooMany[i] = valid[0]
	}
	if err := ValidateDecisions(tooMany); err == nil {
		t.Fatal("over-cap decisions accepted")
	}
}

func TestDecisionsJSON(t *testing.T) {
	if string(DecisionsJSON(nil)) != "[]" {
		t.Fatal("nil decisions must encode as []")
	}
	encoded := DecisionsJSON([]Decision{{RuleID: "r1", Phase: PhaseOutput, Action: ActionRedact, Outcome: OutcomeRedacted}})
	var decoded []Decision
	if err := json.Unmarshal(encoded, &decoded); err != nil || len(decoded) != 1 || decoded[0].RuleID != "r1" {
		t.Fatalf("round trip: %v %s", err, encoded)
	}
}
