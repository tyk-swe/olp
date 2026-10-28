package guardrails

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"
)

const (
	Input              = "input"
	InputParallel      = "input_parallel"
	Output             = "output"
	OutputStream       = "output_stream"
	Block              = "block"
	Redact             = "redact"
	Mask               = "mask"
	Annotate           = "annotate"
	FailClosed         = "fail_closed"
	FailOpen           = "fail_open"
	MaxEntries         = 128
	MaxSegments        = 2048
	MaxContentBytes    = 8 << 20
	MaxFindings        = 4096
	MaxEvidenceBytes   = 256 << 10
	DefaultWindowBytes = 64 << 10
	MaxWindowBytes     = 1 << 20
)

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
	Type          string          `json:"type"`
	Configuration json.RawMessage `json:"configuration"`
	TimeoutMS     int64           `json:"timeout_ms"`
	OnError       string          `json:"on_error"`
}

type Entry struct {
	GuardrailID string `json:"guardrail_id"`
	RevisionID  string `json:"revision_id"`
	Phase       string `json:"phase"`
	Action      string `json:"action"`
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
	Scope            string `json:"scope"`
	ScopeID          string `json:"scope_id"`
	PolicyRevisionID string `json:"policy_revision_id"`
	Order            int    `json:"order"`
}

type Boundary struct{ OrganizationID, ProjectID, RouteID, ToolsetID, KeyID string }

type Segment struct {
	ID        string `json:"id"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text"`
	Kind      string `json:"kind,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Untrusted bool   `json:"untrusted,omitempty"`
	toolJSON  *JSONSurface
}

func (s Segment) Restore(masks *Masking) Segment {
	return s.withText(masks.Restore(s.Text))
}

func (s Segment) withText(text string) Segment {
	return Segment{ID: s.ID, Role: s.Role, Text: text, Kind: s.Kind, ToolName: s.ToolName, Untrusted: s.Untrusted, toolJSON: s.toolJSON}
}

type Evaluation struct {
	RequestID string    `json:"request_id"`
	Route     string    `json:"route,omitempty"`
	Phase     string    `json:"phase"`
	Segments  []Segment `json:"segments"`
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
	Action     string      `json:"action"`
	Categories []string    `json:"categories,omitempty"`
	Redactions []Redaction `json:"redactions,omitempty"`
}

// Evidence deliberately has no content, offsets, patterns, or replacements.
type Evidence struct {
	GuardrailID string   `json:"guardrail_id"`
	Revision    string   `json:"revision"`
	Phase       string   `json:"phase"`
	Action      string   `json:"action"`
	Outcome     string   `json:"outcome"`
	Categories  []string `json:"categories,omitempty"`
	LatencyMS   int64    `json:"latency_ms"`
}

type Result struct {
	Segments  []Segment  `json:"segments"`
	Decisions []Evidence `json:"decisions"`
	Blocked   bool       `json:"blocked"`
	Changed   bool       `json:"-"`
}

type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string { return e.Code }

var ErrBlocked = &Error{Code: "guardrail_blocked", Status: 400}
var ErrSurface = &Error{Code: "guardrail_surface_unavailable", Status: 422}
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

func validPhase(p string) bool {
	return slices.Contains([]string{Input, InputParallel, Output, OutputStream}, p)
}
func validAction(a string) bool { return slices.Contains([]string{Block, Redact, Mask, Annotate}, a) }

func ValidateEvidence(values []Evidence) error {
	if len(values) > MaxEntries*4 {
		return errors.New("too many guardrail decisions")
	}
	for _, e := range values {
		if !identifier.MatchString(e.GuardrailID) || !identifier.MatchString(e.Revision) || !validPhase(e.Phase) || !validAction(e.Action) || e.LatencyMS < 0 || e.LatencyMS > int64((24*time.Hour)/time.Millisecond) || len(e.Categories) > 32 {
			return errors.New("invalid guardrail evidence")
		}
		if !slices.Contains([]string{"passed", "blocked", "redacted", "masked", "annotated", "monitored", "failed_open", "failed_closed", "skipped"}, e.Outcome) {
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
