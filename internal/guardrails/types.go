package guardrails

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
)

const (
	MaxSegments        = 2048
	MaxContentBytes    = 8 << 20
	MaxFindings        = 4096
	DefaultWindowBytes = 64 << 10
	MaxWindowBytes     = 1 << 20
)

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

// Revision is an immutable, fully resolved definition in a serving release.
// Credential fields in Configuration are references, never secret values.
type Revision struct {
	ID            string                  `json:"id"`
	GuardrailID   string                  `json:"guardrail_id"`
	ProjectID     string                  `json:"project_id"`
	Name          string                  `json:"name"`
	Type          Kind                    `json:"type"`
	Configuration json.RawMessage         `json:"configuration"`
	TimeoutMS     int64                   `json:"timeout_ms"`
	OnError       guardrailpolicy.OnError `json:"on_error"`
}

type Entry struct {
	GuardrailID string                 `json:"guardrail_id"`
	RevisionID  string                 `json:"revision_id"`
	Phase       guardrailpolicy.Phase  `json:"phase"`
	Action      guardrailpolicy.Action `json:"action"`
	Monitor     bool                   `json:"monitor,omitempty"`
	Mandatory   bool                   `json:"mandatory,omitempty"`
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
	ID               string                `json:"id"`
	Scope            guardrailpolicy.Scope `json:"scope"`
	ScopeID          string                `json:"scope_id"`
	PolicyRevisionID string                `json:"policy_revision_id"`
	Order            int                   `json:"order"`
}

const InstallationProject = "installation"

type Boundary struct{ ProjectID, RouteID, ToolsetID, KeyID string }

func ProjectPointer(project string) *string {
	if project == InstallationProject || project == "" {
		return nil
	}
	return &project
}

func ProjectOf(project *string) string {
	if project == nil {
		return InstallationProject
	}
	return *project
}

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

// InputProvenance keeps request tool results visible to output governance
// under IDs that cannot collide with output segments.
func InputProvenance(input []Segment) []Segment {
	return slices.Collect(func(yield func(Segment) bool) {
		for _, s := range input {
			if s.Kind == ToolResult && !yield(Segment{ID: "input_" + s.ID, Role: s.Role, Text: s.Text, Kind: s.Kind, ToolName: s.ToolName, ToolCallID: s.ToolCallID, Untrusted: s.Untrusted, toolJSON: s.toolJSON}) {
				return
			}
		}
	})
}

type Evaluation struct {
	RequestID            string                `json:"request_id"`
	Route                string                `json:"route,omitempty"`
	Phase                guardrailpolicy.Phase `json:"phase"`
	Segments             []Segment             `json:"segments"`
	CompressionExemptIDs []string              `json:"-"`
	WorkflowRecovery     bool                  `json:"-"`
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

// Verdict is one guardrail's finding over the segments it evaluated. The zero
// Verdict passes.
type Verdict struct {
	flagged    bool
	categories []string
	redactions []Redaction
}

func Flag(categories ...string) Verdict { return Verdict{flagged: true, categories: categories} }

// Redacting flags the spans it replaces and passes when there are none.
func Redacting(redactions []Redaction, categories ...string) Verdict {
	if len(redactions) == 0 {
		return Verdict{}
	}
	return Verdict{flagged: true, categories: categories, redactions: redactions}
}

func flagIf(flagged bool, category string) Verdict {
	if flagged {
		return Flag(category)
	}
	return Verdict{}
}

func (v Verdict) Flagged() bool           { return v.flagged }
func (v Verdict) Categories() []string    { return v.categories }
func (v Verdict) Redactions() []Redaction { return v.redactions }

// WireVerdict is the verdict webhooks and plugins return. Every action other
// than pass flags the content, and the entry's own action decides the outcome.
type WireVerdict struct {
	Action     string      `json:"action"`
	Categories []string    `json:"categories,omitempty"`
	Redactions []Redaction `json:"redactions,omitempty"`
}

func (w WireVerdict) Verdict() (Verdict, error) {
	switch guardrailpolicy.Action(w.Action) {
	case "pass":
		if len(w.Categories) > 0 || len(w.Redactions) > 0 {
			return Verdict{}, errEvaluation
		}
		return Verdict{}, nil
	case guardrailpolicy.Block, guardrailpolicy.Redact, guardrailpolicy.Mask, guardrailpolicy.Annotate:
		return Verdict{flagged: true, categories: w.Categories, redactions: w.Redactions}, nil
	}
	return Verdict{}, errEvaluation
}

type Result struct {
	Segments  []Segment
	Decisions []guardrailpolicy.Evidence
	Changed   bool
	Recovery  []Segment
}

type Application struct {
	PolicyRevisionID string                `json:"policy_revision_id"`
	Phase            guardrailpolicy.Phase `json:"phase"`
	Segments         []Segment             `json:"segments"`
}

type Pinned struct {
	Policy      PolicyRevision
	Definitions map[string]Revision
}

type PinnedApplication struct {
	Pinned      Pinned
	Application Application
}

func (p Pinned) Compile() (PolicyRevision, *Engine, error) {
	engine, err := Compile(p.Policy.Entries, p.Definitions)
	return p.Policy, engine, err
}

type AppliedResult struct {
	Segments  []Segment                  `json:"segments"`
	Decisions []guardrailpolicy.Evidence `json:"decisions"`
	Blocked   bool                       `json:"blocked"`
}

func Applied(result Result, err error) AppliedResult {
	return AppliedResult{Segments: result.Segments, Decisions: result.Decisions, Blocked: errors.Is(err, ErrBlocked)}
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

func ValidateSegments(segments []Segment) error {
	if len(segments) > MaxSegments {
		return ErrSurface
	}
	seen := make(map[string]bool, len(segments))
	total := 0
	for _, s := range segments {
		total += len(s.Text)
		if !guardrailpolicy.ValidIdentifier(s.ID) || seen[s.ID] || !utf8.ValidString(s.Text) || total > MaxContentBytes || len(s.Role) > 32 || len(s.Kind) > 32 || len(s.ToolName) > 128 {
			return ErrSurface
		}
		seen[s.ID] = true
	}
	return nil
}
