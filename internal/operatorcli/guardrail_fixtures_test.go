package operatorcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
)

const fixtureBaselineID = "00000000-0000-0000-0000-000000000001"
const fixtureCandidateID = "00000000-0000-0000-0000-000000000002"

func fixtureRunner(t *testing.T, handler http.HandlerFunc) (Runner, *bytes.Buffer, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	suite := `{"synthetic":true,"fixtures":[
		{"id":"allow-case","phase":"input","text":"a friendly note","expected":"allow"},
		{"id":"redact-case","phase":"input","text":"code secret-123 end","expected":"redact","expected_redacted_text":"code [hidden] end"}
	]}`
	path := filepath.Join(dir, "fixtures.json")
	if err := os.WriteFile(path, []byte(suite), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	runner := Runner{
		Getenv: func(name string) string {
			switch name {
			case "OLP_MANAGEMENT_URL":
				return server.URL
			case "OLP_MANAGEMENT_TOKEN_FILE":
				return token
			}
			return ""
		},
		Out: out, Err: &bytes.Buffer{},
	}
	return runner, out, path
}

func fixtureArgs(path string) []string {
	return []string{"guardrails", "run-fixtures", "--file", path, "--baseline-revision", fixtureBaselineID, "--candidate-revision", fixtureCandidateID}
}

func reportHandler(passed bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/guardrails/run-fixtures" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var run contract.GuardrailFixtureRunInput
		if json.Unmarshal(body, &run) != nil || run.BaselinePolicyRevisionId.String() != fixtureBaselineID || run.CandidatePolicyRevisionId.String() != fixtureCandidateID || !run.Synthetic || len(run.Fixtures) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		candidate := contract.GuardrailFixtureOutcome{Outcome: "redact", Matched: true}
		if !passed {
			candidate = contract.GuardrailFixtureOutcome{Outcome: "allow", Matched: false}
		}
		json.NewEncoder(w).Encode(contract.GuardrailFixtureReport{
			BaselinePolicyRevisionId: run.BaselinePolicyRevisionId, CandidatePolicyRevisionId: run.CandidatePolicyRevisionId, Passed: passed,
			Results: []contract.GuardrailFixtureResult{
				{Id: "allow-case", Phase: "input", Expected: "allow", Baseline: contract.GuardrailFixtureOutcome{Outcome: "allow", Matched: true}, Candidate: contract.GuardrailFixtureOutcome{Outcome: "allow", Matched: true}, Passed: true},
				{Id: "redact-case", Phase: "input", Expected: "redact", Baseline: contract.GuardrailFixtureOutcome{Outcome: "redact", Matched: true}, Candidate: candidate, Passed: passed},
			},
		})
	}
}

func TestGuardrailsRunFixturesPassesWithoutPrintingContent(t *testing.T) {
	runner, out, path := fixtureRunner(t, reportHandler(true))
	if err := runner.Run(t.Context(), fixtureArgs(path)); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "allow-case: baseline=allow candidate=allow expected=allow passed") || !strings.Contains(text, "2 of 2 fixtures passed") {
		t.Fatalf("output = %q", text)
	}
	for _, hidden := range []string{"friendly", "secret-123", "[hidden]"} {
		if strings.Contains(text, hidden) {
			t.Fatalf("output contains %q in %q", hidden, text)
		}
	}
}

func TestGuardrailsRunFixturesFailsNamingFixtureWithoutContent(t *testing.T) {
	runner, out, path := fixtureRunner(t, reportHandler(false))
	err := runner.Run(t.Context(), fixtureArgs(path))
	if err == nil || !strings.Contains(err.Error(), "redact-case") {
		t.Fatalf("err = %v", err)
	}
	combined := out.String() + err.Error()
	if !strings.Contains(out.String(), "redact-case: baseline=redact candidate=allow expected=redact failed") {
		t.Fatalf("output = %q", out.String())
	}
	for _, hidden := range []string{"friendly", "secret-123", "[hidden]"} {
		if strings.Contains(combined, hidden) {
			t.Fatalf("output contains %q in %q", hidden, combined)
		}
	}
}

func TestGuardrailsRunFixturesServerFailurePrintsNoResults(t *testing.T) {
	runner, out, path := fixtureRunner(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"type":"https://openllmproxy.dev/problems/guardrail_fixture_unsupported","title":"Unprocessable Entity","status":422,"detail":"Fixture runs support only deterministic built-in evaluators."}`))
	})
	if err := runner.Run(t.Context(), fixtureArgs(path)); err == nil {
		t.Fatal("server failure returned no error")
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q", out.String())
	}
}
