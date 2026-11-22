package operatorcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/management"
)

const maxGuardrailFixtureFileBytes = 256 << 10

type guardrailFixtureFile struct {
	Synthetic contract.GuardrailFixtureRunInputSynthetic `json:"synthetic"`
	Fixtures  []contract.GuardrailFixture                `json:"fixtures"`
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
	baseline, baselineErr := uuid.Parse(opts.one("baseline-revision"))
	candidate, candidateErr := uuid.Parse(opts.one("candidate-revision"))
	if opts.one("file") == "" || baselineErr != nil || candidateErr != nil {
		return errors.New("guardrails run-fixtures requires --file FIXTURES --baseline-revision REVISION_ID --candidate-revision REVISION_ID")
	}
	suite, err := readGuardrailFixtureFile(opts.one("file"))
	if err != nil {
		return err
	}
	body, err := json.Marshal(contract.GuardrailFixtureRunInput{
		BaselinePolicyRevisionId: baseline, CandidatePolicyRevisionId: candidate,
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
	var report contract.GuardrailFixtureReport
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

func (runner Runner) writeGuardrailFixtureReport(report contract.GuardrailFixtureReport) error {
	for _, result := range report.Results {
		fmt.Fprintf(runner.Out, "%s: baseline=%s candidate=%s expected=%s %s\n", result.Id, result.Baseline.Outcome, result.Candidate.Outcome, result.Expected, fixtureStatus(result.Passed))
	}
	failed := failedFixtureIDs(report.Results)
	fmt.Fprintf(runner.Out, "%d of %d fixtures passed\n", len(report.Results)-len(failed), len(report.Results))
	if !report.Passed {
		return fmt.Errorf("guardrail fixtures failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func failedFixtureIDs(results []contract.GuardrailFixtureResult) []string {
	failed := slices.DeleteFunc(slices.Clone(results), func(result contract.GuardrailFixtureResult) bool { return result.Passed })
	ids := make([]string, len(failed))
	for i, result := range failed {
		ids[i] = result.Id
	}
	return ids
}

func fixtureStatus(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
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
	return suite, nil
}
