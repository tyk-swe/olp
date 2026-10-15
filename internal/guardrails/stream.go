package guardrails

import (
	"context"
	"slices"
	"unicode/utf8"
)

// Stream holds original frames until no finite-width detector can match text
// spanning them. Successful strict frames are emitted byte-for-byte. The
// adapter supplies text deltas from its native inspection contract.
type Stream struct {
	engine     *Engine
	request    Evaluation
	window     int
	lookbehind int
	bytes      int
	text       string
	frames     []heldFrame
	decisions  []Evidence
	rewrite    func([]byte, string) ([]byte, error)
	masks      *Masking
}
type heldFrame struct {
	frame     []byte
	textBytes int
}

// Rewriter is supplied only by transformed native routes. The adapter changes
// text-delta fields while keeping event ordering and control metadata intact.
func (s *Stream) Rewriter(rewrite func([]byte, string) ([]byte, error)) { s.rewrite = rewrite }

func (s *Stream) Masking(masks *Masking) { s.masks = masks; s.lookbehind = max(s.lookbehind, 64) }

func NewStream(engine *Engine, request Evaluation, window int) (*Stream, error) {
	if err := engine.ValidateStream(window); err != nil {
		return nil, err
	}
	width := 0
	if engine != nil {
		for _, r := range engine.entries {
			if r.entry.Phase == OutputStream {
				width = max(width, r.detector.width()+utf8.UTFMax)
			}
		}
	}
	request.Phase = OutputStream
	return &Stream{engine: engine, request: request, window: window, lookbehind: width}, nil
}

// Push returns only frames proven safe to release. A frame carrying a tool
// call must be assembled and schema-checked by the native adapter beforehand.
func (s *Stream) Push(ctx context.Context, frame []byte, text string) ([][]byte, error) {
	if !utf8.ValidString(text) {
		return nil, ErrSurface
	}
	if (s.engine == nil || !s.engine.HasPhase(OutputStream)) && s.masks == nil {
		return [][]byte{frame}, nil
	}
	// Inspect across the retained suffix before releasing any part of a new
	// frame. Native framing and escaped JSON count toward the memory bound.
	if len(frame) > s.window || s.bytes+len(frame) > s.window {
		return nil, ErrSurface
	}
	s.frames = append(s.frames, heldFrame{slices.Clone(frame), len(text)})
	s.bytes += len(frame)
	s.text += text
	if err := s.inspect(ctx); err != nil {
		return nil, err
	}
	release := len(s.text) - s.lookbehind
	var out [][]byte
	for len(s.frames) > 0 && release >= s.frames[0].textBytes {
		first := s.frames[0]
		// Retain control-only frames while there is no safe text boundary.
		if first.textBytes == 0 && release < 0 {
			break
		}
		out = append(out, first.frame)
		release -= first.textBytes
		s.bytes -= len(first.frame)
		s.text = s.text[first.textBytes:]
		s.frames = s.frames[1:]
	}
	return out, nil
}
func (s *Stream) inspect(ctx context.Context) error {
	text := s.text
	if s.masks != nil {
		text = s.masks.Restore(text)
	}
	s.request.Segments = []Segment{{ID: "stream", Role: "assistant", Text: text}}
	result, err := s.engine.Evaluate(ctx, s.request, nil)
	for _, d := range result.Decisions {
		s.record(d)
	}
	if result.Changed || text != s.text {
		if s.rewrite == nil {
			return ErrSurface
		}
		next := result.Segments[0].Text
		remaining := next
		total := 0
		for i := range s.frames {
			if s.frames[i].textBytes > 0 {
				frame, err := s.rewrite(s.frames[i].frame, remaining)
				if err != nil {
					return err
				}
				s.frames[i].frame = frame
				s.frames[i].textBytes = len(remaining)
				remaining = ""
			}
			total += len(s.frames[i].frame)
		}
		if total > s.window {
			return ErrSurface
		}
		s.bytes = total
		s.text = next
	}
	return err
}
func (s *Stream) record(d Evidence) {
	for i, v := range s.decisions {
		if v.GuardrailID == d.GuardrailID && v.Revision == d.Revision && v.Phase == d.Phase && v.Action == d.Action && v.Outcome == d.Outcome {
			s.decisions[i].LatencyMS += d.LatencyMS
			for _, c := range d.Categories {
				s.decisions[i].Categories = appendCategory(s.decisions[i].Categories, c)
			}
			return
		}
	}
	if len(s.decisions) < MaxEntries*4 {
		s.decisions = append(s.decisions, d)
	}
}
func (s *Stream) Finish(ctx context.Context) ([][]byte, error) {
	if err := s.inspect(ctx); err != nil {
		return nil, err
	}
	out := make([][]byte, len(s.frames))
	for i, f := range s.frames {
		out[i] = f.frame
	}
	s.frames = nil
	s.text = ""
	s.bytes = 0
	return out, nil
}
func (s *Stream) Decisions() []Evidence { return slices.Clone(s.decisions) }
