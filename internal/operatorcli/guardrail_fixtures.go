package operatorcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/olp/management"
)

const (
	maxGuardrailFixtureFileBytes = 256 << 10
	maxGuardrailFixtureCases     = 32
	maxGuardrailFixtureTextBytes = 8192
)

var guardrailFixtureID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type guardrailFixtureCase struct {
	ID                   string  `json:"id"`
	Phase                string  `json:"phase"`
	Text                 string  `json:"text"`
	Expected             string  `json:"expected"`
	ExpectedRedactedText *string `json:"expected_redacted_text,omitempty"`
}

type guardrailFixtureFile struct {
	Synthetic bool                   `json:"synthetic"`
	Fixtures  []guardrailFixtureCase `json:"fixtures"`
}

type guardrailFixtureRun struct {
	BaselinePolicyRevisionID  string                 `json:"baseline_policy_revision_id"`
	CandidatePolicyRevisionID string                 `json:"candidate_policy_revision_id"`
	Synthetic                 bool                   `json:"synthetic"`
	Fixtures                  []guardrailFixtureCase `json:"fixtures"`
}

type guardrailFixtureOutcome struct {
	Outcome string `json:"outcome"`
	Matched bool   `json:"matched"`
}

type guardrailFixtureResult struct {
	ID        string                  `json:"id"`
	Phase     string                  `json:"phase"`
	Expected  string                  `json:"expected"`
	Baseline  guardrailFixtureOutcome `json:"baseline"`
	Candidate guardrailFixtureOutcome `json:"candidate"`
	Passed    bool                    `json:"passed"`
}

type guardrailFixtureReport struct {
	BaselinePolicyRevisionID  string                   `json:"baseline_policy_revision_id"`
	CandidatePolicyRevisionID string                   `json:"candidate_policy_revision_id"`
	Passed                    bool                     `json:"passed"`
	Results                   []guardrailFixtureResult `json:"results"`
}

func (runner Runner) guardrailFixtures(ctx context.Context, args []string) error {
	opts, positional, err := parseOptions(args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("guardrail fixture commands take named options")
	}
	if err := checkOptions(opts, "file", "baseline-revision", "candidate-revision", "output"); err != nil {
		return err
	}
	if opts.one("file") == "" || opts.one("baseline-revision") == "" || opts.one("candidate-revision") == "" {
		return errors.New("guardrails run-fixtures requires --file FIXTURES --baseline-revision REVISION_ID --candidate-revision REVISION_ID")
	}
	suite, err := readGuardrailFixtureFile(opts.one("file"))
	if err != nil {
		return err
	}
	body, err := json.Marshal(guardrailFixtureRun{
		BaselinePolicyRevisionID: opts.one("baseline-revision"), CandidatePolicyRevisionID: opts.one("candidate-revision"),
		Synthetic: suite.Synthetic, Fixtures: suite.Fixtures,
	})
	if err != nil {
		return err
	}
	operation, ok := management.Lookup("run_guardrail_fixtures")
	if !ok {
		return errors.New("guardrail fixture operation missing from contract")
	}
	client, err := runner.client()
	if err != nil {
		return err
	}
	result, err := client.Call(ctx, operation, management.Arguments{Body: body})
	if err != nil {
		return err
	}
	var report guardrailFixtureReport
	if err := json.Unmarshal(result.Body, &report); err != nil {
		return errors.New("guardrail fixture report could not be read")
	}
	if output := opts.one("output"); output != "" {
		if err := runner.output(output, append(slices.Clone(result.Body), '\n')); err != nil {
			return err
		}
	}
	return runner.writeGuardrailFixtureReport(report)
}

func (runner Runner) writeGuardrailFixtureReport(report guardrailFixtureReport) error {
	failed := []string{}
	for _, result := range report.Results {
		status := "passed"
		if !result.Passed {
			status = "failed"
			failed = append(failed, result.ID)
		}
		fmt.Fprintf(runner.Out, "%s: baseline=%s candidate=%s expected=%s %s\n", result.ID, result.Baseline.Outcome, result.Candidate.Outcome, result.Expected, status)
	}
	fmt.Fprintf(runner.Out, "%d of %d fixtures passed\n", len(report.Results)-len(failed), len(report.Results))
	if !report.Passed {
		return fmt.Errorf("guardrail fixtures failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func readGuardrailFixtureFile(path string) (guardrailFixtureFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return guardrailFixtureFile{}, errors.New("cannot open guardrail fixture file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxGuardrailFixtureFileBytes+1))
	if err != nil || len(data) > maxGuardrailFixtureFileBytes {
		return guardrailFixtureFile{}, errors.New("guardrail fixture file exceeds 256 KiB or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var suite guardrailFixtureFile
	if decoder.Decode(&suite) != nil || decoder.Decode(new(any)) != io.EOF {
		return guardrailFixtureFile{}, errors.New("guardrail fixture file must contain one JSON fixture suite")
	}
	if err := suite.validate(); err != nil {
		return guardrailFixtureFile{}, err
	}
	return suite, nil
}

func (suite guardrailFixtureFile) validate() error {
	if !suite.Synthetic {
		return errors.New("guardrail fixture file must confirm its examples are synthetic")
	}
	if len(suite.Fixtures) < 1 || len(suite.Fixtures) > maxGuardrailFixtureCases {
		return errors.New("guardrail fixture file must hold between 1 and 32 fixtures")
	}
	seen := make(map[string]bool, len(suite.Fixtures))
	for _, fixture := range suite.Fixtures {
		if !guardrailFixtureID.MatchString(fixture.ID) || seen[fixture.ID] {
			return errors.New("guardrail fixtures need distinct IDs of 1 to 64 letters, digits, dots, underscores or hyphens")
		}
		seen[fixture.ID] = true
		if fixture.Phase != "input" && fixture.Phase != "output" {
			return errors.New("guardrail fixtures support only the unary input and output phases")
		}
		if !utf8.ValidString(fixture.Text) || len(fixture.Text) > maxGuardrailFixtureTextBytes {
			return errors.New("guardrail fixture text must be UTF-8 of at most 8192 bytes")
		}
		if fixture.Expected != "allow" && fixture.Expected != "block" && fixture.Expected != "redact" {
			return errors.New("guardrail fixtures expect allow, block or redact")
		}
		if fixture.ExpectedRedactedText != nil && (fixture.Expected != "redact" || !utf8.ValidString(*fixture.ExpectedRedactedText) || len(*fixture.ExpectedRedactedText) > maxGuardrailFixtureTextBytes) {
			return errors.New("expected redacted text belongs only to a redact expectation and must be UTF-8 of at most 8192 bytes")
		}
	}
	return nil
}
