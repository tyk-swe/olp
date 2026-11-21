package guardrails

import "slices"

type Placement struct {
	Scope                                          Scope
	ScopeID, ProjectID, PolicyID, PolicyRevisionID string
}

type resolvedEntry struct {
	entry     Entry
	placement Placement
}

type Compatibility struct {
	TypedUnary, RetainsOutput, Uninspected, StreamTextRewriteUnavailable bool
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
	PolicyID         string              `json:"policy_id"`
	PolicyRevisionID string              `json:"policy_revision_id"`
	GuardrailID      string              `json:"guardrail_id"`
	RevisionID       string              `json:"revision_id"`
	Scope            Scope               `json:"scope"`
	Phase            Phase               `json:"phase"`
	Mode             string              `json:"mode"`
	Notes            []CompatibilityNote `json:"notes"`
}

func (e *Engine) Preview(c Compatibility, visible func(Placement) bool) []PreviewEntry {
	if e == nil {
		return []PreviewEntry{}
	}
	return slices.AppendSeq([]PreviewEntry{}, func(yield func(PreviewEntry) bool) {
		for _, r := range e.entries {
			if visible(r.placement) && !yield(r.preview(c)) {
				return
			}
		}
	})
}

func (r compiledEntry) preview(c Compatibility) PreviewEntry {
	mode := "enforce"
	if r.entry.Monitor {
		mode = "monitor"
	}
	return PreviewEntry{
		PolicyID: r.placement.PolicyID, PolicyRevisionID: r.placement.PolicyRevisionID,
		GuardrailID: r.entry.GuardrailID, RevisionID: r.entry.RevisionID,
		Scope: r.placement.Scope, Phase: r.entry.Phase, Mode: mode, Notes: r.notes(c),
	}
}

func (r compiledEntry) notes(c Compatibility) []CompatibilityNote {
	return slices.AppendSeq([]CompatibilityNote{}, func(yield func(CompatibilityNote) bool) {
		if c.Uninspected {
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
	return !r.entry.Monitor && r.entry.Action != Annotate
}

func (r compiledEntry) rewriteConflict() bool {
	return !r.entry.Monitor && r.entry.Phase != InputParallel && (r.entry.Action == Redact || r.entry.Action == Mask)
}

func (r compiledEntry) streamTextRewriteConflict() bool {
	return !r.entry.Monitor && r.entry.Phase == OutputStream && r.entry.Action == Redact && r.definition.Type != "builtin.tools"
}

func (r compiledEntry) retainedOutputConflict() bool {
	return r.entry.Phase == Output || r.entry.Phase == OutputStream ||
		r.entry.Phase == Input && !r.entry.Monitor && r.entry.Action == Mask
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
