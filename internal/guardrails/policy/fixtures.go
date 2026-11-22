package policy

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/guardrails"
)

const (
	maxFixtureRunBytes     = 256 << 10
	maxGuardrailFixtures   = 32
	maxFixtureTextBytes    = 8192
	fixtureSegmentID       = "text"
	fixtureUnsupportedCode = "guardrail_fixture_unsupported"
	fixtureUnevaluableCode = "guardrail_fixture_unevaluable"
)

var fixtureIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

var fixtureBuiltinTypes = []guardrails.Kind{"builtin.regex", "builtin.pii", "builtin.secrets", "builtin.keywords"}

type fixtureOutcome string

const (
	outcomeAllow  fixtureOutcome = "allow"
	outcomeBlock  fixtureOutcome = "block"
	outcomeRedact fixtureOutcome = "redact"
)

type guardrailFixture struct {
	ID                   string           `json:"id"`
	Phase                guardrails.Phase `json:"phase"`
	Text                 *string          `json:"text"`
	Expected             fixtureOutcome   `json:"expected"`
	ExpectedRedactedText *string          `json:"expected_redacted_text,omitempty"`
}

type fixtureRunInput struct {
	BaselinePolicyRevisionID  string             `json:"baseline_policy_revision_id"`
	CandidatePolicyRevisionID string             `json:"candidate_policy_revision_id"`
	Synthetic                 bool               `json:"synthetic"`
	Fixtures                  []guardrailFixture `json:"fixtures"`
}

type fixtureOutcomeResult struct {
	Outcome fixtureOutcome `json:"outcome"`
	Matched bool           `json:"matched"`
}

type fixtureResult struct {
	ID        string               `json:"id"`
	Phase     guardrails.Phase     `json:"phase"`
	Expected  fixtureOutcome       `json:"expected"`
	Baseline  fixtureOutcomeResult `json:"baseline"`
	Candidate fixtureOutcomeResult `json:"candidate"`
	Passed    bool                 `json:"passed"`
}

type fixtureReport struct {
	BaselinePolicyRevisionID  string          `json:"baseline_policy_revision_id"`
	CandidatePolicyRevisionID string          `json:"candidate_policy_revision_id"`
	Passed                    bool            `json:"passed"`
	Results                   []fixtureResult `json:"results"`
}

func (in fixtureRunInput) validate() error {
	if uuid.Validate(in.BaselinePolicyRevisionID) != nil {
		return httpkit.Invalid("baseline_policy_revision_id", "Choose an existing policy revision.")
	}
	if uuid.Validate(in.CandidatePolicyRevisionID) != nil {
		return httpkit.Invalid("candidate_policy_revision_id", "Choose an existing policy revision.")
	}
	if !in.Synthetic {
		return httpkit.Invalid("synthetic", "Confirm the file uses only synthetic examples.")
	}
	if len(in.Fixtures) < 1 || len(in.Fixtures) > maxGuardrailFixtures {
		return httpkit.Invalid("fixtures", "Use between 1 and 32 fixtures.")
	}
	for i, fixture := range in.Fixtures {
		field := fmt.Sprintf("fixtures.%d", i)
		if slices.ContainsFunc(in.Fixtures[:i], func(earlier guardrailFixture) bool { return earlier.ID == fixture.ID }) {
			return httpkit.Invalid(field+".id", "Use a distinct fixture ID.")
		}
		if err := fixture.validate(field); err != nil {
			return err
		}
	}
	return nil
}

func (f guardrailFixture) validate(field string) error {
	if !fixtureIDPattern.MatchString(f.ID) {
		return httpkit.Invalid(field+".id", "Use a fixture ID of 1 to 64 letters, digits, dots, underscores or hyphens, starting with a letter or digit.")
	}
	if f.Phase != guardrails.Input && f.Phase != guardrails.Output {
		return httpkit.Invalid(field+".phase", "Choose the unary input or output phase.")
	}
	if f.Text == nil || !utf8.ValidString(*f.Text) || len(*f.Text) > maxFixtureTextBytes {
		return httpkit.Invalid(field+".text", "Give the fixture text as UTF-8 of at most 8192 bytes. It may be empty.")
	}
	if f.Expected != outcomeAllow && f.Expected != outcomeBlock && f.Expected != outcomeRedact {
		return httpkit.Invalid(field+".expected", "Choose allow, block or redact.")
	}
	if f.ExpectedRedactedText != nil && (f.Expected != outcomeRedact || !utf8.ValidString(*f.ExpectedRedactedText) || len(*f.ExpectedRedactedText) > maxFixtureTextBytes) {
		return httpkit.Invalid(field+".expected_redacted_text", "Give the expected redacted text only for a redact expectation, as UTF-8 text of at most 8192 bytes.")
	}
	return nil
}

func fixtureReady(pinned guardrails.Pinned) error {
	for _, entry := range pinned.Policy.Entries {
		if entry.Phase != guardrails.Input && entry.Phase != guardrails.Output {
			return fixtureUnsupported()
		}
		if entry.Action == guardrails.Mask {
			return fixtureUnsupported()
		}
		definition, ok := pinned.Definitions[entry.RevisionID]
		if !ok || !slices.Contains(fixtureBuiltinTypes, definition.Type) {
			return fixtureUnsupported()
		}
	}
	if _, _, err := pinned.Compile(); err != nil {
		return fixtureUnsupported()
	}
	return nil
}

func fixtureUnsupported() error {
	return httpkit.Fail(http.StatusUnprocessableEntity, fixtureUnsupportedCode, "Fixture runs support only unary input and output entries with deterministic built-in evaluators.")
}

func fixtureUnevaluable() error {
	return httpkit.Fail(http.StatusUnprocessableEntity, fixtureUnevaluableCode, "A fixture could not be evaluated for both revisions.")
}

func (a *API) runFixtures(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var in fixtureRunInput
	if err := httpkit.DecodeUnique(r, &in, maxFixtureRunBytes); err != nil {
		return httpkit.Reply{}, err
	}
	if err := in.validate(); err != nil {
		return httpkit.Reply{}, err
	}
	baseline, err := a.pinnedRevision(r, p, in.BaselinePolicyRevisionID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	candidate, err := a.pinnedRevision(r, p, in.CandidatePolicyRevisionID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if baseline.Policy.PolicyID != candidate.Policy.PolicyID {
		return httpkit.Reply{}, httpkit.Invalid("candidate_policy_revision_id", "Choose two revisions of the same policy.")
	}
	report, err := a.runFixtureSuite(r, p, baseline, candidate, in.Fixtures)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(report), nil
}

func (a *API) pinnedRevision(r *http.Request, p authz.Principal, revisionID string) (guardrails.Pinned, error) {
	ctx := r.Context()
	selected, err := revisionOf(ctx, a.server.Pool, revisionID)
	if err != nil {
		return guardrails.Pinned{}, err
	}
	if err := p.Project(guardrails.ProjectPointer(selected.ProjectID), authz.View); err != nil {
		return guardrails.Pinned{}, err
	}
	ids := revisionIDs(selected.Entries)
	definitions, err := guardrails.ReadRevisions(ctx, a.server.Pool, ids)
	if err != nil {
		return guardrails.Pinned{}, err
	}
	if !owned(definitions, ids, selected.ProjectID) {
		return guardrails.Pinned{}, httpkit.Invalid("policy", "Policy references must stay in their project.")
	}
	return guardrails.Pinned{Policy: selected, Definitions: definitions}, nil
}

func (a *API) runFixtureSuite(r *http.Request, p authz.Principal, baseline, candidate guardrails.Pinned, fixtures []guardrailFixture) (fixtureReport, error) {
	if err := fixtureReady(baseline); err != nil {
		return fixtureReport{}, err
	}
	if err := fixtureReady(candidate); err != nil {
		return fixtureReport{}, err
	}
	batch := make([]guardrails.PinnedApplication, 0, 2*len(fixtures))
	for _, fixture := range fixtures {
		batch = append(batch, fixture.application(baseline), fixture.application(candidate))
	}
	evaluated, err := a.evaluateBatch(r, p, batch)
	if err != nil {
		return fixtureReport{}, err
	}
	if len(evaluated) != len(batch) {
		return fixtureReport{}, fixtureUnevaluable()
	}
	results := make([]fixtureResult, 0, len(fixtures))
	for i, fixture := range fixtures {
		baselineOutcome, err := fixtureOutcomeOf(evaluated[2*i], fixture)
		if err != nil {
			return fixtureReport{}, err
		}
		candidateOutcome, err := fixtureOutcomeOf(evaluated[2*i+1], fixture)
		if err != nil {
			return fixtureReport{}, err
		}
		results = append(results, fixtureResult{
			ID: fixture.ID, Phase: fixture.Phase, Expected: fixture.Expected,
			Baseline: baselineOutcome, Candidate: candidateOutcome, Passed: candidateOutcome.Matched,
		})
	}
	return fixtureReport{
		BaselinePolicyRevisionID: baseline.Policy.ID, CandidatePolicyRevisionID: candidate.Policy.ID,
		Passed: !slices.ContainsFunc(results, func(result fixtureResult) bool { return !result.Passed }), Results: results,
	}, nil
}

func (fixture guardrailFixture) application(pinned guardrails.Pinned) guardrails.PinnedApplication {
	return guardrails.PinnedApplication{Pinned: pinned, Application: guardrails.Application{
		PolicyRevisionID: pinned.Policy.ID, Phase: fixture.Phase,
		Segments: []guardrails.Segment{{ID: fixtureSegmentID, Role: fixtureRole(fixture.Phase), Text: *fixture.Text}},
	}}
}

func fixtureRole(phase guardrails.Phase) guardrails.Role {
	if phase == guardrails.Output {
		return guardrails.Assistant
	}
	return guardrails.User
}

func fixtureOutcomeOf(result guardrails.Result, fixture guardrailFixture) (fixtureOutcomeResult, error) {
	if hasDecision(result, guardrails.FailedOpen, guardrails.FailedClosed) {
		return fixtureOutcomeResult{}, fixtureUnevaluable()
	}
	index := slices.IndexFunc(result.Segments, func(segment guardrails.Segment) bool { return segment.ID == fixtureSegmentID })
	if index < 0 {
		return fixtureOutcomeResult{}, fixtureUnevaluable()
	}
	outcome := observedOutcome(result)
	matched := outcome == fixture.Expected && (fixture.ExpectedRedactedText == nil || result.Segments[index].Text == *fixture.ExpectedRedactedText)
	return fixtureOutcomeResult{Outcome: outcome, Matched: matched}, nil
}

func observedOutcome(result guardrails.Result) fixtureOutcome {
	if hasDecision(result, guardrails.Blocked) {
		return outcomeBlock
	}
	if result.Changed || hasDecision(result, guardrails.Redacted) {
		return outcomeRedact
	}
	return outcomeAllow
}

func hasDecision(result guardrails.Result, outcomes ...guardrails.Outcome) bool {
	return slices.ContainsFunc(result.Decisions, func(decision guardrails.Evidence) bool { return slices.Contains(outcomes, decision.Outcome) })
}
