package policy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func fixtureRevision(id, guardrailID string, kind guardrails.Kind, configuration string) guardrails.Revision {
	return guardrails.Revision{ID: id, GuardrailID: guardrailID, ProjectID: "project-1", Type: kind, Configuration: json.RawMessage(configuration), TimeoutMS: 1000, OnError: guardrails.FailClosed}
}

func fixturePinned(policyRevisionID, blockPattern, redactPattern string) guardrails.Pinned {
	suffix := strings.TrimPrefix(policyRevisionID, "policy-rev-")
	block := fixtureRevision("block-rev-"+suffix, "block-def", "builtin.regex", `{"rules":[{"id":"block-rule","pattern":"`+blockPattern+`"}]}`)
	redact := fixtureRevision("redact-rev-"+suffix, "redact-def", "builtin.regex", `{"rules":[{"id":"redact-rule","pattern":"`+redactPattern+`","replacement":"[hidden]"}]}`)
	entries := []guardrails.Entry{
		{GuardrailID: "block-def", RevisionID: block.ID, Phase: guardrails.Input, Action: guardrails.Block},
		{GuardrailID: "redact-def", RevisionID: redact.ID, Phase: guardrails.Input, Action: guardrails.Redact},
		{GuardrailID: "block-def", RevisionID: block.ID, Phase: guardrails.Output, Action: guardrails.Block},
		{GuardrailID: "redact-def", RevisionID: redact.ID, Phase: guardrails.Output, Action: guardrails.Redact},
	}
	return guardrails.Pinned{
		Policy:      guardrails.PolicyRevision{ID: policyRevisionID, PolicyID: "policy-1", ProjectID: "project-1", Entries: entries},
		Definitions: map[string]guardrails.Revision{block.ID: block, redact.ID: redact},
	}
}

func directBatchEvaluator(r *http.Request, _ authz.Principal, batch []guardrails.PinnedApplication) ([]guardrails.Result, error) {
	results := make([]guardrails.Result, 0, len(batch))
	for _, item := range batch {
		_, engine, err := item.Pinned.Compile()
		if err != nil {
			return nil, err
		}
		result, err := engine.Evaluate(r.Context(), guardrails.Evaluation{Phase: item.Application.Phase, Segments: item.Application.Segments}, &guardrails.Masking{})
		if err != nil && !errors.Is(err, guardrails.ErrBlocked) {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func fixtureSuite() []guardrailFixture {
	redacted := "code [hidden] end"
	return []guardrailFixture{
		{ID: "allow-case", Phase: guardrails.Input, Text: new("a friendly note"), Expected: outcomeAllow},
		{ID: "block-case", Phase: guardrails.Input, Text: new("a blocked-word here"), Expected: outcomeBlock},
		{ID: "redact-case", Phase: guardrails.Input, Text: new("code secret-123 end"), Expected: outcomeRedact, ExpectedRedactedText: &redacted},
		{ID: "output-block-case", Phase: guardrails.Output, Text: new("a blocked-word here"), Expected: outcomeBlock},
	}
}

func TestRunFixtureSuitePassesAndRepeats(t *testing.T) {
	api := &API{evaluateBatch: directBatchEvaluator}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/guardrails/run-fixtures", nil)
	baseline := fixturePinned("policy-rev-base", "blocked-word", "secret-[0-9]+")
	candidate := fixturePinned("policy-rev-candidate", "blocked-word", "secret-[0-9]+")
	first, err := api.runFixtureSuite(request, authz.Principal{}, baseline, candidate, fixtureSuite())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Passed || len(first.Results) != 4 {
		t.Fatalf("report = %+v", first)
	}
	for _, result := range first.Results {
		if !result.Baseline.Matched || !result.Candidate.Matched || !result.Passed {
			t.Fatalf("result = %+v", result)
		}
	}
	second, err := api.runFixtureSuite(request, authz.Principal{}, baseline, candidate, fixtureSuite())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeat differs, first = %+v, second = %+v", first, second)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"friendly", "blocked-word", "secret-123", "[hidden]", "secret-[0-9]"} {
		if strings.Contains(string(raw), hidden) {
			t.Fatalf("report contains %q in %s", hidden, raw)
		}
	}
}

func TestRunFixtureSuiteNamesFailingCandidateWithoutContent(t *testing.T) {
	api := &API{evaluateBatch: directBatchEvaluator}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/guardrails/run-fixtures", nil)
	baseline := fixturePinned("policy-rev-base", "blocked-word", "secret-[0-9]+")
	candidate := fixturePinned("policy-rev-candidate", "other-word", "token-[0-9]+")
	report, err := api.runFixtureSuite(request, authz.Principal{}, baseline, candidate, fixtureSuite())
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatalf("report = %+v", report)
	}
	byID := map[string]fixtureResult{}
	for _, result := range report.Results {
		byID[result.ID] = result
	}
	if !byID["allow-case"].Passed || byID["block-case"].Passed || byID["redact-case"].Passed || byID["output-block-case"].Passed {
		t.Fatalf("results = %+v", report.Results)
	}
	if byID["block-case"].Candidate.Outcome != outcomeAllow || !byID["block-case"].Baseline.Matched {
		t.Fatalf("block case = %+v", byID["block-case"])
	}
	if byID["redact-case"].Candidate.Outcome != outcomeAllow {
		t.Fatalf("redact case = %+v", byID["redact-case"])
	}
}

func TestRunFixtureSuiteRefusesFailedClosedEvaluation(t *testing.T) {
	api := &API{evaluateBatch: directBatchEvaluator}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/guardrails/run-fixtures", nil)
	pinned := fixturePinned("policy-rev-base", "a", "secret-[0-9]+")
	overflow := []guardrailFixture{{ID: "overflow-case", Phase: guardrails.Input, Text: new(strings.Repeat("a", guardrails.MaxFindings+1)), Expected: outcomeBlock}}
	_, err := api.runFixtureSuite(request, authz.Principal{}, pinned, pinned, overflow)
	var problem *httpkit.Problem
	if !errors.As(err, &problem) || problem.Code != fixtureUnevaluableCode {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFixtureSuiteRefusesUnsupportedBeforeEvaluation(t *testing.T) {
	calls := 0
	api := &API{evaluateBatch: func(*http.Request, authz.Principal, []guardrails.PinnedApplication) ([]guardrails.Result, error) {
		calls++
		return nil, nil
	}}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/guardrails/run-fixtures", nil)
	baseline := fixturePinned("policy-rev-base", "blocked-word", "secret-[0-9]+")
	unsupported := fixtureRevision("hook-rev", "hook-def", "webhook", `{"endpoint":"https://example.invalid","credential_ref":"cred-1","content_egress":true}`)
	candidate := guardrails.Pinned{
		Policy: guardrails.PolicyRevision{ID: "policy-rev-candidate", PolicyID: "policy-1", ProjectID: "project-1", Entries: []guardrails.Entry{
			{GuardrailID: "hook-def", RevisionID: "hook-rev", Phase: guardrails.Input, Action: guardrails.Block},
		}},
		Definitions: map[string]guardrails.Revision{"hook-rev": unsupported},
	}
	if _, err := api.runFixtureSuite(request, authz.Principal{}, baseline, candidate, fixtureSuite()); err == nil {
		t.Fatal("unsupported candidate returned no error")
	}
	if calls != 0 {
		t.Fatalf("evaluator calls = %d", calls)
	}
}

func TestFixtureRunInputValidate(t *testing.T) {
	redacted := "hidden"
	valid := func() fixtureRunInput {
		return fixtureRunInput{
			BaselinePolicyRevisionID:  "00000000-0000-0000-0000-000000000001",
			CandidatePolicyRevisionID: "00000000-0000-0000-0000-000000000002",
			Synthetic:                 true,
			Fixtures:                  []guardrailFixture{{ID: "case-1", Phase: guardrails.Input, Text: new(""), Expected: outcomeAllow}},
		}
	}
	if err := valid().validate(); err != nil {
		t.Fatal(err)
	}
	var omitted fixtureRunInput
	if err := json.Unmarshal([]byte(`{"baseline_policy_revision_id":"00000000-0000-0000-0000-000000000001","candidate_policy_revision_id":"00000000-0000-0000-0000-000000000002","synthetic":true,"fixtures":[{"id":"case-1","phase":"input","expected":"allow"}]}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if err := omitted.validate(); err == nil {
		t.Error("fixture without text returned no error")
	}
	cases := map[string]func(*fixtureRunInput){
		"not synthetic":     func(in *fixtureRunInput) { in.Synthetic = false },
		"bad revision":      func(in *fixtureRunInput) { in.CandidatePolicyRevisionID = "candidate" },
		"streaming phase":   func(in *fixtureRunInput) { in.Fixtures[0].Phase = guardrails.OutputStream },
		"redacted on allow": func(in *fixtureRunInput) { in.Fixtures[0].ExpectedRedactedText = &redacted },
		"duplicate id":      func(in *fixtureRunInput) { in.Fixtures = append(in.Fixtures, in.Fixtures[0]) },
		"oversized text":    func(in *fixtureRunInput) { in.Fixtures[0].Text = new(strings.Repeat("x", maxFixtureTextBytes+1)) },
		"too many fixtures": func(in *fixtureRunInput) {
			for len(in.Fixtures) <= maxGuardrailFixtures {
				next := in.Fixtures[0]
				next.ID = "case-" + strings.Repeat("x", len(in.Fixtures)+1)
				in.Fixtures = append(in.Fixtures, next)
			}
		},
	}
	for name, change := range cases {
		in := valid()
		change(&in)
		if err := in.validate(); err == nil {
			t.Errorf("%s returned no error", name)
		}
	}
}
