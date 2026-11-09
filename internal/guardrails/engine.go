package guardrails

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Evaluator is also the shared webhook, model and confined-plugin contract.
// Implementations must honor ctx; content-bearing diagnostics never escape it.
type Evaluator interface {
	Evaluate(context.Context, Revision, Spec, Evaluation) (Verdict, error)
}
type EvaluatorFunc func(context.Context, Revision, Spec, Evaluation) (Verdict, error)

func (f EvaluatorFunc) Evaluate(c context.Context, d Revision, s Spec, e Evaluation) (Verdict, error) {
	return f(c, d, s, e)
}

type detector interface {
	detect(context.Context, Evaluation) (Verdict, error)
	// A zero width means evaluation requires complete content.
	width() int
}
type compiledEntry struct {
	entry      Entry
	definition Revision
	kind       kind
	detector   detector
}
type Engine struct{ entries []compiledEntry }

func (e *Engine) HasCompression() bool {
	return e != nil && slices.ContainsFunc(e.entries, func(entry compiledEntry) bool {
		return entry.entry.Phase == Input && !entry.entry.Monitor && entry.kind.compresses
	})
}

func (r compiledEntry) legacyInput() bool {
	return r.definition.LegacyRuleID != "" && r.entry.Phase == Input && r.entry.Action == Block
}

func (r compiledEntry) bound(external Evaluator) compiledEntry {
	d, ok := r.detector.(externalDetector)
	if !ok {
		return r
	}
	return compiledEntry{r.entry, r.definition, r.kind, externalDetector{definition: d.definition, spec: d.spec, evaluator: external}}
}

func (e *Engine) entriesWhere(match func(compiledEntry) bool) bool {
	return e != nil && slices.ContainsFunc(e.entries, match)
}

func (e *Engine) HasLegacyInput() bool { return e.entriesWhere(compiledEntry.legacyInput) }

func (e *Engine) HasLegacyOutput() bool {
	return e.entriesWhere(func(r compiledEntry) bool { return r.definition.LegacyRuleID != "" && r.entry.Phase == Output })
}

// Bind returns an engine whose external entries evaluate through external.
// An engine without external entries is returned unchanged.
func (e *Engine) Bind(external Evaluator) *Engine {
	if !e.entriesWhere(func(r compiledEntry) bool { _, ok := r.detector.(externalDetector); return ok }) {
		return e
	}
	return &Engine{entries: slices.Collect(func(yield func(compiledEntry) bool) {
		for _, r := range e.entries {
			if !yield(r.bound(external)) {
				return
			}
		}
	})}
}

// WithoutModelGuardrails removes recursive evaluator calls before the gateway
// inspects native request surfaces. Local and external policy entries remain
// active, and the immutable release engine is never changed.
func (e *Engine) WithoutModelGuardrails() *Engine {
	if e == nil {
		return nil
	}
	entries := make([]compiledEntry, 0, len(e.entries))
	for _, entry := range e.entries {
		if !entry.kind.model {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	if len(entries) == len(e.entries) {
		return e
	}
	return &Engine{entries: entries}
}

// Compile copies configuration before compiling it. No live management object
// or PostgreSQL handle is reachable from a compiled engine.
func Compile(entries []Entry, definitions map[string]Revision) (*Engine, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > MaxEntries {
		return nil, errors.New("too many effective guardrails")
	}
	e := &Engine{}
	for _, entry := range entries {
		d, ok := definitions[entry.RevisionID]
		if !ok || d.ID != entry.RevisionID || d.GuardrailID != entry.GuardrailID || !entry.Phase.Valid() || !entry.Action.Valid() {
			return nil, errors.New("invalid guardrail revision or policy entry")
		}
		if entry.Action == Mask && entry.Phase != Input {
			return nil, errors.New("mask requires synchronous input")
		}
		if entry.Action == Redact && entry.Phase == InputParallel && !entry.Monitor {
			return nil, errors.New("parallel input cannot transform a dispatched request")
		}
		d.Configuration = slices.Clone(d.Configuration)
		k, spec, err := parse(d)
		if err != nil {
			return nil, err
		}
		if !k.supports(entry.Phase) {
			return nil, errors.New("this vendor does not support the configured phase")
		}
		if !entry.Monitor && (entry.Action == Redact || entry.Action == Mask) && !k.transforms {
			return nil, errors.New("this detector does not supply transformation spans")
		}
		e.entries = append(e.entries, compiledEntry{entry, d, k, detectorOf(d, spec)})
	}
	return e, nil
}

func (e *Engine) HasPhase(phase Phase) bool {
	return e.entriesWhere(func(r compiledEntry) bool { return r.entry.Phase == phase })
}

func (e *Engine) HasAction(phase Phase, action Action) bool {
	return e.entriesWhere(func(r compiledEntry) bool {
		return r.entry.Phase == phase && r.entry.Action == action && !r.entry.Monitor
	})
}

// Unavailable records an explicit lack of coverage. Monitoring and annotation
// cannot change the invocation; enforcing entries require a 422 refusal.
func (e *Engine) Unavailable(phases ...Phase) (Result, error) {
	var result Result
	var err error
	if e != nil {
		for _, r := range e.entries {
			if len(phases) > 0 && !slices.Contains(phases, r.entry.Phase) {
				continue
			}
			outcome := FailedOpen
			if !r.entry.Monitor && r.entry.Action != Annotate {
				outcome = FailedClosed
				err = ErrSurface
			}
			result.Decisions = append(result.Decisions, Evidence{GuardrailID: r.definition.GuardrailID, Revision: r.definition.ID, Phase: r.entry.Phase, Action: r.entry.Action, Outcome: outcome, Categories: []string{"surface.unavailable"}})
		}
	}
	return result, err
}

// ValidateRoute is shared by activation, configuration planning and serving.
func (e *Engine) ValidateRoute(strict bool, deadlineMS int64) error {
	if e == nil {
		return nil
	}
	for _, r := range e.entries {
		if strict && !r.entry.Monitor && (r.entry.Action == Redact || r.entry.Action == Mask) {
			return errors.New("strict routes cannot redact or mask")
		}
		if r.definition.TimeoutMS > deadlineMS {
			return errors.New("guardrail timeout exceeds route deadline")
		}
	}
	return nil
}

func (e *Engine) ValidateStream(window int) error {
	if e == nil {
		return nil
	}
	if window < 1 || window > MaxWindowBytes {
		return ErrSurface
	}
	for _, r := range e.entries {
		if r.entry.Monitor || r.entry.Action == Annotate {
			continue
		}
		// Tool schemas inspect completed argument values buffered by the native
		// stream adapter, within the same configured byte limit.
		if r.entry.Phase == OutputStream && r.definition.Type == "builtin.tools" {
			continue
		}
		if r.entry.Phase == Output || (r.entry.Phase == OutputStream && (r.detector.width() == 0 || r.detector.width()+utf8.UTFMax > window)) {
			return ErrSurface
		}
	}
	return nil
}

// Masking belongs to a single caller request, including retries. It must never
// be persisted or placed on a process-wide cache.
type Masking struct {
	mu        sync.Mutex
	prefix    string
	values    map[string]string
	originals map[string]string
}

func (m *Masking) placeholder(value string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		m.prefix = "[OLP_" + hex.EncodeToString(nonce[:]) + "_"
		m.values = map[string]string{}
		m.originals = map[string]string{}
	}
	if p, ok := m.originals[value]; ok {
		return p, nil
	}
	if len(m.values) >= MaxFindings {
		return "", errEvaluation
	}
	p := fmt.Sprintf("%s%d]", m.prefix, len(m.values))
	m.values[p] = value
	m.originals[value] = p
	return p, nil
}
func (m *Masking) Restore(text string) string {
	if m == nil {
		return text
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prefix == "" || !strings.Contains(text, m.prefix) {
		return text
	}
	// Replace in a single pass: a restored value must never itself be treated
	// as a placeholder chosen by a caller.
	pairs := make([]string, 0, len(m.values)*2)
	for p, v := range m.values {
		pairs = append(pairs, p, v)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

func (e *Engine) Evaluate(ctx context.Context, request Evaluation, masks *Masking) (Result, error) {
	result := Result{Segments: request.Segments}
	if e == nil {
		return result, nil
	}
	if !request.Phase.Valid() {
		return result, ErrSurface
	}
	if !e.HasPhase(request.Phase) {
		return result, nil
	}
	if err := ValidateSegments(request.Segments); err != nil {
		return result, err
	}
	for _, r := range e.entries {
		if r.entry.Phase != request.Phase {
			continue
		}
		ev := Evidence{GuardrailID: r.definition.GuardrailID, Revision: r.definition.ID, Phase: request.Phase, Action: r.entry.Action, Outcome: Passed}
		if request.InternalModel && r.kind.model {
			ev.Outcome = Skipped
			result.Decisions = append(result.Decisions, ev)
			continue
		}
		started := time.Now()
		bounded, cancel := context.WithTimeout(ctx, time.Duration(r.definition.TimeoutMS)*time.Millisecond)
		evaluation := request
		evaluation.Segments = result.Segments
		if r.kind.compresses && len(request.CompressionExemptIDs) > 0 {
			evaluation.Segments = slices.Collect(func(yield func(Segment) bool) {
				for _, segment := range result.Segments {
					if !slices.Contains(request.CompressionExemptIDs, segment.ID) && !yield(segment) {
						return
					}
				}
			})
			if len(evaluation.Segments) == 0 {
				cancel()
				ev.Outcome = Skipped
				result.Decisions = append(result.Decisions, ev)
				continue
			}
		}
		if r.legacyInput() && request.LegacyInput != nil {
			evaluation.Segments = request.LegacyInput
		}
		verdict, err := r.detector.detect(bounded, evaluation)
		if err == nil {
			err = bounded.Err()
		}
		cancel()
		ev.LatencyMS = time.Since(started).Milliseconds()
		if err == nil {
			err = validateVerdict(verdict, evaluation.Segments)
		}
		if err != nil {
			ev.Outcome = FailedOpen
			if r.definition.OnError == FailClosed && !r.entry.Monitor {
				ev.Outcome = FailedClosed
			}
			result.Decisions = append(result.Decisions, ev)
			if blocks(ev.Outcome) {
				return result, ErrBlocked
			}
			continue
		}
		ev.Categories = slices.Compact(slices.Sorted(slices.Values(verdict.Categories())))
		if verdict.Flagged() {
			switch {
			case r.entry.Monitor:
				ev.Outcome = Monitored
			case r.entry.Action == Annotate:
				ev.Outcome = Annotated
			case r.entry.Action == Block:
				ev.Outcome = Blocked
			default:
				if len(verdict.Redactions()) == 0 {
					ev.Outcome = FailedClosed
					break
				}
				firstRecovery := len(result.Recovery)
				originals := func() []Segment {
					if !request.WorkflowRecovery || !r.kind.compresses {
						return nil
					}
					return slices.Collect(func(yield func(Segment) bool) {
						for _, segment := range result.Segments {
							if slices.ContainsFunc(verdict.Redactions(), func(redaction Redaction) bool { return redaction.Segment == segment.ID }) && !yield(segment) {
								return
							}
						}
					})
				}()
				if !result.Changed {
					result.Segments = slices.Clone(result.Segments)
				}
				if err = transform(result.Segments, verdict.Redactions(), r.entry.Action, masks); err != nil {
					ev.Outcome = FailedClosed
					break
				}
				result.Recovery = slices.Concat(result.Recovery, originals)
				if len(originals) > 0 {
					result.Segments = slices.Collect(func(yield func(Segment) bool) {
						for _, segment := range result.Segments {
							index := slices.IndexFunc(originals, func(original Segment) bool { return original.ID == segment.ID })
							if index >= 0 {
								if !yield(segment.withText(fmt.Sprintf("%s\n[recovery id: %d]", segment.Text, firstRecovery+index))) {
									return
								}
								continue
							}
							if !yield(segment) {
								return
							}
						}
					})
				}
				result.Changed = true
				ev.Outcome = Redacted
				if r.entry.Action == Mask {
					ev.Outcome = Masked
				}
			}
		}
		result.Decisions = append(result.Decisions, ev)
		if blocks(ev.Outcome) {
			return result, ErrBlocked
		}
	}
	return result, nil
}

func blocks(outcome Outcome) bool { return outcome == Blocked || outcome == FailedClosed }

func validateVerdict(v Verdict, segments []Segment) error {
	if len(v.categories) > 32 || len(v.redactions) > MaxFindings {
		return errEvaluation
	}
	for _, c := range v.categories {
		if !categoryID.MatchString(c) {
			return errEvaluation
		}
	}
	texts := make(map[string]string, len(segments))
	for _, s := range segments {
		texts[s.ID] = s.Text
	}
	last := map[string]int{}
	redactions := slices.Clone(v.redactions)
	sort.SliceStable(redactions, func(i, j int) bool {
		if redactions[i].Segment == redactions[j].Segment {
			return redactions[i].Start < redactions[j].Start
		}
		return redactions[i].Segment < redactions[j].Segment
	})
	for _, r := range redactions {
		text, ok := texts[r.Segment]
		if !ok || r.Start < last[r.Segment] || r.End <= r.Start || r.End > len(text) || !utf8.ValidString(text[:r.Start]) || !utf8.ValidString(text[:r.End]) || !utf8.ValidString(r.Replacement) || len(r.Replacement) > 4096 {
			return errEvaluation
		}
		last[r.Segment] = r.End
	}
	return nil
}

func transform(segments []Segment, edits []Redaction, action Action, masks *Masking) error {
	edits = slices.Clone(edits)
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].Start > edits[j].Start })
	for i := range segments {
		text := segments[i].Text
		for _, r := range edits {
			if r.Segment != segments[i].ID {
				continue
			}
			replacement := r.Replacement
			if action == Mask {
				if masks == nil {
					return errEvaluation
				}
				var err error
				replacement, err = masks.placeholder(text[r.Start:r.End])
				if err != nil {
					return err
				}
			}
			text = text[:r.Start] + replacement + text[r.End:]
			if len(text) > MaxContentBytes {
				return errEvaluation
			}
		}
		segments[i].Text = text
	}
	return nil
}
