package guardrails

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type (
	Phase   = contract.GuardrailPolicyEntryPhase
	Action  = contract.GuardrailPolicyEntryAction
	Outcome = contract.GuardrailEvidenceOutcome
	Scope   = contract.GuardrailAttachmentScope
	OnError = contract.CreateGuardrailRequestOnError
)

const (
	Input         = contract.GuardrailPolicyEntryPhaseInput
	InputParallel = contract.GuardrailPolicyEntryPhaseInputParallel
	Output        = contract.GuardrailPolicyEntryPhaseOutput
	OutputStream  = contract.GuardrailPolicyEntryPhaseOutputStream

	Block    = contract.GuardrailPolicyEntryActionBlock
	Redact   = contract.GuardrailPolicyEntryActionRedact
	Mask     = contract.GuardrailPolicyEntryActionMask
	Annotate = contract.GuardrailPolicyEntryActionAnnotate
	Pass     = Action("pass")

	FailClosed = contract.CreateGuardrailRequestOnErrorFailClosed
	FailOpen   = contract.CreateGuardrailRequestOnErrorFailOpen

	Passed       = contract.GuardrailEvidenceOutcomePassed
	Blocked      = contract.GuardrailEvidenceOutcomeBlocked
	Redacted     = contract.GuardrailEvidenceOutcomeRedacted
	Masked       = contract.GuardrailEvidenceOutcomeMasked
	Annotated    = contract.GuardrailEvidenceOutcomeAnnotated
	Monitored    = contract.GuardrailEvidenceOutcomeMonitored
	FailedOpen   = contract.GuardrailEvidenceOutcomeFailedOpen
	FailedClosed = contract.GuardrailEvidenceOutcomeFailedClosed
	Skipped      = contract.GuardrailEvidenceOutcomeSkipped

	InstallationScope = contract.GuardrailAttachmentScopeInstallation
	OrganizationScope = contract.GuardrailAttachmentScopeOrganization
	ProjectScope      = contract.GuardrailAttachmentScopeProject
	RouteScope        = contract.GuardrailAttachmentScopeRoute
	ToolsetScope      = contract.GuardrailAttachmentScopeToolset
	KeyScope          = contract.GuardrailAttachmentScopeApiKey
)

const (
	MaxEntries         = 128
	MaxSegments        = 2048
	MaxContentBytes    = 8 << 20
	MaxFindings        = 4096
	MaxEvidenceBytes   = 256 << 10
	DefaultWindowBytes = 64 << 10
	MaxWindowBytes     = 1 << 20
)

func OutputPhase(p Phase) bool { return p == Output || p == OutputStream }

type Role string

const (
	System    Role = "system"
	User      Role = "user"
	Assistant Role = "assistant"
	Tool      Role = "tool"
	Developer Role = "developer"
)

type SegmentKind string

const (
	Text           SegmentKind = ""
	ToolCall       SegmentKind = "tool_call"
	ToolResult     SegmentKind = "tool_result"
	ToolDefinition SegmentKind = "tool_definition"
)

type Kind string

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
var categoryID = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// Revision is an immutable, fully resolved definition in a serving release.
// Credential fields in Configuration are references, never secret values.
type Revision struct {
	LegacyRuleID  string          `json:"legacy_rule_id,omitempty"`
	ID            string          `json:"id"`
	GuardrailID   string          `json:"guardrail_id"`
	ProjectID     string          `json:"project_id"`
	Name          string          `json:"name"`
	Type          Kind            `json:"type"`
	Configuration json.RawMessage `json:"configuration"`
	TimeoutMS     int64           `json:"timeout_ms"`
	OnError       OnError         `json:"on_error"`
}

type Entry struct {
	GuardrailID string `json:"guardrail_id"`
	RevisionID  string `json:"revision_id"`
	Phase       Phase  `json:"phase"`
	Action      Action `json:"action"`
	Monitor     bool   `json:"monitor,omitempty"`
	Mandatory   bool   `json:"mandatory,omitempty"`
}

type PolicyRevision struct {
	ID        string  `json:"id"`
	PolicyID  string  `json:"policy_id"`
	ProjectID string  `json:"project_id"`
	Name      string  `json:"name"`
	Revision  int64   `json:"revision"`
	Entries   []Entry `json:"entries"`
}

type Attachment struct {
	ID               string `json:"id"`
	Scope            Scope  `json:"scope"`
	ScopeID          string `json:"scope_id"`
	PolicyRevisionID string `json:"policy_revision_id"`
	Order            int    `json:"order"`
}

type Boundary struct{ OrganizationID, ProjectID, RouteID, ToolsetID, KeyID string }

type Segment struct {
	ID         string      `json:"id"`
	Role       Role        `json:"role,omitempty"`
	Text       string      `json:"text"`
	Kind       SegmentKind `json:"kind,omitempty"`
	ToolName   string      `json:"tool_name,omitempty"`
	ToolCallID string      `json:"-"`
	Untrusted  bool        `json:"untrusted,omitempty"`
	toolJSON   *JSONSurface
}

func (s Segment) Restore(masks *Masking) Segment {
	return s.withText(masks.Restore(s.Text))
}

func (s Segment) withText(text string) Segment {
	return Segment{ID: s.ID, Role: s.Role, Text: text, Kind: s.Kind, ToolName: s.ToolName, ToolCallID: s.ToolCallID, Untrusted: s.Untrusted, toolJSON: s.toolJSON}
}

type Evaluation struct {
	RequestID            string    `json:"request_id"`
	Route                string    `json:"route,omitempty"`
	Phase                Phase     `json:"phase"`
	Segments             []Segment `json:"segments"`
	CompressionExemptIDs []string  `json:"-"`
	WorkflowRecovery     bool      `json:"-"`
	// LegacyInput is the strict native scalar view used only by migrated
	// blocking input rules. It never changes the writable native surface.
	LegacyInput []Segment `json:"-"`
	// InternalModel prevents recursive evaluator invocations. It is not accepted
	// from an HTTP caller and is set only by the gateway's evaluator runner.
	InternalModel bool `json:"-"`
	// Partial marks a streaming suffix whose final word boundary is not yet known.
	Partial          bool `json:"-"`
	completeSegments map[string]bool
}

type Redaction struct {
	Segment     string `json:"segment"`
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement"`
}

type Verdict struct {
	Action     Action      `json:"action"`
	Categories []string    `json:"categories,omitempty"`
	Redactions []Redaction `json:"redactions,omitempty"`
}

func blockIf(blocked bool, category string) Verdict {
	if blocked {
		return Verdict{Action: Block, Categories: []string{category}}
	}
	return Verdict{Action: Pass}
}

// Evidence deliberately has no content, offsets, patterns, or replacements.
type Evidence struct {
	GuardrailID string   `json:"guardrail_id"`
	Revision    string   `json:"revision"`
	Phase       Phase    `json:"phase"`
	Action      Action   `json:"action"`
	Outcome     Outcome  `json:"outcome"`
	Categories  []string `json:"categories,omitempty"`
	LatencyMS   int64    `json:"latency_ms"`
}

type Result struct {
	Segments  []Segment  `json:"segments"`
	Decisions []Evidence `json:"decisions"`
	Blocked   bool       `json:"blocked"`
	Changed   bool       `json:"-"`
	Recovery  []Segment  `json:"-"`
}

type Failure string

func (f Failure) Error() string { return string(f) }

const (
	ErrBlocked Failure = "guardrail_blocked"
	ErrSurface Failure = "guardrail_surface_unavailable"
)

var errEvaluation = errors.New("guardrail evaluation failed")

func decodeConfig(raw json.RawMessage, into any) error {
	if len(raw) == 0 || len(raw) > 128<<10 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return errors.New("invalid guardrail configuration")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(into) != nil {
		return errors.New("invalid guardrail configuration")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid guardrail configuration")
	}
	return nil
}

func ValidateEvidence(values []Evidence) error {
	if len(values) > MaxEntries*4 {
		return errors.New("too many guardrail decisions")
	}
	for _, e := range values {
		if !identifier.MatchString(e.GuardrailID) || !identifier.MatchString(e.Revision) || !e.Phase.Valid() || !e.Action.Valid() || e.LatencyMS < 0 || e.LatencyMS > int64((24*time.Hour)/time.Millisecond) || len(e.Categories) > 32 {
			return errors.New("invalid guardrail evidence")
		}
		if !e.Outcome.Valid() {
			return errors.New("invalid guardrail outcome")
		}
		for _, c := range e.Categories {
			if !categoryID.MatchString(c) {
				return errors.New("invalid guardrail category")
			}
		}
	}
	if len(values) > 0 {
		raw, _ := json.Marshal(values)
		if len(raw) > MaxEvidenceBytes {
			return errors.New("guardrail evidence exceeds byte bound")
		}
	}
	return nil
}

// EvidenceSize gives the conservative JSON size, including an array separator.
func EvidenceSize(value Evidence) int { raw, _ := json.Marshal(value); return len(raw) + 1 }

func validateSegments(segments []Segment) error {
	if len(segments) > MaxSegments {
		return ErrSurface
	}
	seen := make(map[string]bool, len(segments))
	total := 0
	for _, s := range segments {
		total += len(s.Text)
		if !identifier.MatchString(s.ID) || seen[s.ID] || !utf8.ValidString(s.Text) || total > MaxContentBytes || len(s.Role) > 32 || len(s.Kind) > 32 || len(s.ToolName) > 128 {
			return ErrSurface
		}
		seen[s.ID] = true
	}
	return nil
}
