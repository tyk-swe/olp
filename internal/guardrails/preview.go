package guardrails

import (
	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"slices"
)

type Placement struct {
	Scope                                          guardrailpolicy.Scope
	ScopeID, ProjectID, PolicyID, PolicyRevisionID string
}

type resolvedEntry struct {
	entry     Entry
	placement Placement
}

type Compatibility struct {
	TypedUnary, RetainsOutput, Uninspected, StreamTextRewriteUnavailable bool
	TypeSafeInput                                                        bool
	StreamWindow                                                         int
}

type CompatibilityNote string

const (
	RewriteUnavailable        CompatibilityNote = "rewrite_unavailable"
	RetainedOutputUninspected CompatibilityNote = "retained_output_uninspected"
	StreamingUnavailable      CompatibilityNote = "streaming_unavailable"
	SurfaceUninspected        CompatibilityNote = "surface_uninspected"
)

type PreviewEntry struct {
	PolicyID         string                `json:"policy_id"`
	PolicyRevisionID string                `json:"policy_revision_id"`
	GuardrailID      string                `json:"guardrail_id"`
	RevisionID       string                `json:"revision_id"`
	Scope            guardrailpolicy.Scope `json:"scope"`
	Phase            guardrailpolicy.Phase `json:"phase"`
	Mode             string                `json:"mode"`
	Notes            []CompatibilityNote   `json:"notes"`
}

func (e *Engine) Preview(c Compatibility, visible func(Placement) bool) []PreviewEntry {
	if e == nil {
		return []PreviewEntry{}
	}
	entries := slices.Collect(func(yield func(compiledEntry) bool) {
		for _, r := range e.entries {
			if visible(r.placement) && !yield(r) {
				return
			}
		}
	})
	uninspected := c.Uninspected && (!c.TypeSafeInput || !onlyTypeSafe(entries))
	return slices.AppendSeq([]PreviewEntry{}, func(yield func(PreviewEntry) bool) {
		for _, r := range entries {
			if !yield(r.preview(c, uninspected)) {
				return
			}
		}
	})
}

func (r compiledEntry) preview(c Compatibility, uninspected bool) PreviewEntry {
	mode := "enforce"
	if r.entry.Monitor {
		mode = "monitor"
	}
	return PreviewEntry{
		PolicyID: r.placement.PolicyID, PolicyRevisionID: r.placement.PolicyRevisionID,
		GuardrailID: r.entry.GuardrailID, RevisionID: r.entry.RevisionID,
		Scope: r.placement.Scope, Phase: r.entry.Phase, Mode: mode, Notes: r.notes(c, uninspected),
	}
}

func (r compiledEntry) notes(c Compatibility, uninspected bool) []CompatibilityNote {
	return slices.AppendSeq([]CompatibilityNote{}, func(yield func(CompatibilityNote) bool) {
		if uninspected {
			if r.enforcesUnavailable() {
				yield(SurfaceUninspected)
			}
			return
		}
		if (c.TypedUnary && r.rewriteConflict() || c.StreamTextRewriteUnavailable && r.streamTextRewriteConflict()) &&
			!yield(RewriteUnavailable) {
			return
		}
		if c.RetainsOutput && r.retainedOutputConflict() && !yield(RetainedOutputUninspected) {
			return
		}
		if c.StreamWindow > 0 && r.streamConflict(c.StreamWindow) {
			yield(StreamingUnavailable)
		}
	})
}

func (r compiledEntry) enforcesUnavailable() bool {
	return !r.entry.Monitor && r.entry.Action != guardrailpolicy.Annotate
}

func (r compiledEntry) rewriteConflict() bool {
	return !r.entry.Monitor && r.entry.Phase != guardrailpolicy.InputParallel && (r.entry.Action == guardrailpolicy.Redact || r.entry.Action == guardrailpolicy.Mask)
}

func (r compiledEntry) streamTextRewriteConflict() bool {
	return !r.entry.Monitor && (r.entry.Phase == guardrailpolicy.Input && r.entry.Action == guardrailpolicy.Mask ||
		r.entry.Phase == guardrailpolicy.OutputStream && r.entry.Action == guardrailpolicy.Redact && r.definition.Type != "builtin.tools")
}

func (r compiledEntry) retainedOutputConflict() bool {
	return r.entry.Phase == guardrailpolicy.Output || r.entry.Phase == guardrailpolicy.OutputStream ||
		r.entry.Phase == guardrailpolicy.Input && !r.entry.Monitor && r.entry.Action == guardrailpolicy.Mask
}

func (e *Engine) ValidateRewrites() error {
	if e.entriesWhere(compiledEntry.rewriteConflict) {
		return ErrSurface
	}
	return nil
}

func (e *Engine) ValidateRetainedOutput() error {
	if e.entriesWhere(compiledEntry.retainedOutputConflict) {
		return ErrSurface
	}
	return nil
}
