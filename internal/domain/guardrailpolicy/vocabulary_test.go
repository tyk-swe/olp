package guardrailpolicy

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/management"
)

func TestVocabularyMatchesManagementContract(t *testing.T) {
	checkVocabulary(t, publishedEnum(t, "GuardrailPolicyEntry", "phase"), map[Phase]contract.GuardrailPolicyEntryPhase{
		Input:         contract.GuardrailPolicyEntryPhaseInput,
		InputParallel: contract.GuardrailPolicyEntryPhaseInputParallel,
		Output:        contract.GuardrailPolicyEntryPhaseOutput,
		OutputStream:  contract.GuardrailPolicyEntryPhaseOutputStream,
	})
	checkVocabulary(t, publishedEnum(t, "GuardrailPolicyEntry", "action"), map[Action]contract.GuardrailPolicyEntryAction{
		Block:    contract.GuardrailPolicyEntryActionBlock,
		Redact:   contract.GuardrailPolicyEntryActionRedact,
		Mask:     contract.GuardrailPolicyEntryActionMask,
		Annotate: contract.GuardrailPolicyEntryActionAnnotate,
	})
	checkVocabulary(t, publishedEnum(t, "CreateGuardrailRequest", "on_error"), map[OnError]contract.CreateGuardrailRequestOnError{
		FailClosed: contract.CreateGuardrailRequestOnErrorFailClosed,
		FailOpen:   contract.CreateGuardrailRequestOnErrorFailOpen,
	})
	checkVocabulary(t, publishedEnum(t, "GuardrailEvidence", "outcome"), map[Outcome]contract.GuardrailEvidenceOutcome{
		Passed:       contract.GuardrailEvidenceOutcomePassed,
		Blocked:      contract.GuardrailEvidenceOutcomeBlocked,
		Redacted:     contract.GuardrailEvidenceOutcomeRedacted,
		Masked:       contract.GuardrailEvidenceOutcomeMasked,
		Annotated:    contract.GuardrailEvidenceOutcomeAnnotated,
		Monitored:    contract.GuardrailEvidenceOutcomeMonitored,
		FailedOpen:   contract.GuardrailEvidenceOutcomeFailedOpen,
		FailedClosed: contract.GuardrailEvidenceOutcomeFailedClosed,
		Skipped:      contract.GuardrailEvidenceOutcomeSkipped,
	})
	checkVocabulary(t, publishedEnum(t, "GuardrailAttachment", "scope"), map[Scope]contract.GuardrailAttachmentScope{
		InstallationScope: contract.GuardrailAttachmentScopeInstallation,
		OrganizationScope: contract.GuardrailAttachmentScopeOrganization,
		ProjectScope:      contract.GuardrailAttachmentScopeProject,
		RouteScope:        contract.GuardrailAttachmentScopeRoute,
		ToolsetScope:      contract.GuardrailAttachmentScopeToolset,
		KeyScope:          contract.GuardrailAttachmentScopeApiKey,
	})
}

type publishedValues struct {
	name   string
	values []string
}

func publishedEnum(t *testing.T, schema, property string) publishedValues {
	t.Helper()
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(management.Document, &document); err != nil {
		t.Fatal(err)
	}
	var enum struct {
		Enum []string `json:"enum"`
	}
	name := schema + "." + property
	if err := json.Unmarshal(document.Components.Schemas[schema].Properties[property], &enum); err != nil || len(enum.Enum) == 0 {
		t.Fatalf("%s publishes no enum: %v", name, err)
	}
	return publishedValues{name: name, values: enum.Enum}
}

func checkVocabulary[T interface {
	~string
	Valid() bool
}, C ~string](t *testing.T, published publishedValues, bound map[T]C) {
	t.Helper()
	for ours, theirs := range bound {
		if string(ours) != string(theirs) {
			t.Errorf("%s: the vocabulary says %q where the contract says %q", published.name, ours, theirs)
		}
	}
	if len(bound) != len(published.values) {
		t.Errorf("%s publishes %v, the vocabulary names %d values", published.name, published.values, len(bound))
	}
	for _, value := range published.values {
		if _, named := bound[T(value)]; !named {
			t.Errorf("%s publishes %q, which the vocabulary does not name", published.name, value)
		}
		if !T(value).Valid() {
			t.Errorf("%s publishes %q, which the vocabulary rejects", published.name, value)
		}
	}
	if unpublished := T(published.values[0] + "-unpublished"); unpublished.Valid() {
		t.Errorf("%s does not publish %q, which the vocabulary accepts", published.name, unpublished)
	}
}
