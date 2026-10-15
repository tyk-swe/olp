package guardrails

import (
	"context"
	"slices"
	"strings"
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
	texts      map[string]string
	ids        []string
	frames     []heldFrame
	decisions  []Evidence
	rewrite    func([]byte, map[string]string) ([]byte, error)
	masks      *Masking
}
type heldFrame struct {
	frame     []byte
	textBytes map[string]int
}

// Rewriter is supplied only by transformed native routes. The adapter changes
// text-delta fields while keeping event ordering and control metadata intact.
func (s *Stream) Rewriter(rewrite func([]byte, string) ([]byte, error)) {
	s.rewrite = func(frame []byte, texts map[string]string) ([]byte, error) {
		return rewrite(frame, texts["stream"])
	}
}

// SegmentRewriter changes only the named text fields in a native frame.
func (s *Stream) SegmentRewriter(rewrite func([]byte, map[string]string) ([]byte, error)) {
	s.rewrite = rewrite
}

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
	request.Partial = true
	request.completeSegments = map[string]bool{}
	return &Stream{engine: engine, request: request, window: window, lookbehind: width, texts: map[string]string{}}, nil
}

// Push returns only frames proven safe to release. A frame carrying a tool
// call must be assembled and schema-checked by the native adapter beforehand.
func (s *Stream) Push(ctx context.Context, frame []byte, text string) ([][]byte, error) {
	return s.PushSegments(ctx, frame, []Segment{{ID: "stream", Role: "assistant", Text: text}})
}

// PushSegments keeps independent candidates and text fields in separate
// inspection buffers while retaining their shared native frame ordering.
// Completed prefixes identify candidates whose final boundary is now known.
func (s *Stream) PushSegments(ctx context.Context, frame []byte, segments []Segment, completed ...string) ([][]byte, error) {
	if err := validateSegments(segments); err != nil {
		return nil, err
	}
	if (s.engine == nil || !s.engine.HasPhase(OutputStream)) && s.masks == nil {
		return [][]byte{frame}, nil
	}
	// Inspect across the retained suffix before releasing any part of a new
	// frame. Native framing and escaped JSON count toward the memory bound.
	if len(frame) > s.window || s.bytes+len(frame) > s.window {
		return nil, ErrSurface
	}
	lengths := make(map[string]int, len(segments))
	for _, segment := range segments {
		if s.request.completeSegments[segment.ID] && segment.Text != "" {
			return nil, ErrSurface
		}
		if _, exists := s.texts[segment.ID]; !exists {
			if len(s.ids) >= MaxSegments {
				return nil, ErrSurface
			}
			s.ids = append(s.ids, segment.ID)
		}
		s.texts[segment.ID] += segment.Text
		lengths[segment.ID] = len(segment.Text)
	}
	for _, prefix := range completed {
		for _, id := range s.ids {
			if strings.HasPrefix(id, prefix) {
				s.request.completeSegments[id] = true
			}
		}
	}
	s.frames = append(s.frames, heldFrame{slices.Clone(frame), lengths})
	s.bytes += len(frame)
	if err := s.inspect(ctx); err != nil {
		return nil, err
	}
	release := make(map[string]int, len(s.texts))
	for id, text := range s.texts {
		release[id] = len(text) - s.lookbehind
		if s.request.completeSegments[id] {
			release[id] = len(text)
		}
	}
	var out [][]byte
	for len(s.frames) > 0 {
		first := s.frames[0]
		ready := true
		for id, n := range first.textBytes {
			if n > 0 && release[id] < n {
				ready = false
				break
			}
		}
		if !ready {
			break
		}
		out = append(out, first.frame)
		for id, n := range first.textBytes {
			release[id] -= n
			s.texts[id] = s.texts[id][n:]
		}
		s.bytes -= len(first.frame)
		s.frames = s.frames[1:]
	}
	return out, nil
}
func (s *Stream) inspect(ctx context.Context) error {
	s.request.Segments = make([]Segment, 0, len(s.ids))
	for _, id := range s.ids {
		text := s.texts[id]
		if s.masks != nil {
			text = s.masks.Restore(text)
		}
		s.request.Segments = append(s.request.Segments, Segment{ID: id, Role: "assistant", Text: text})
	}
	result, err := s.engine.Evaluate(ctx, s.request, nil)
	for _, d := range result.Decisions {
		s.record(d)
	}
	changed := map[string]string{}
	for _, segment := range result.Segments {
		if segment.Text != s.texts[segment.ID] {
			changed[segment.ID] = segment.Text
		}
	}
	if len(changed) > 0 {
		if s.rewrite == nil {
			return ErrSurface
		}
		remaining := make(map[string]string, len(changed))
		for id, text := range changed {
			remaining[id] = text
		}
		total := 0
		for i := range s.frames {
			fields := map[string]string{}
			for id, text := range remaining {
				if s.frames[i].textBytes[id] > 0 {
					fields[id] = text
					s.frames[i].textBytes[id] = len(text)
					remaining[id] = ""
				}
			}
			if len(fields) > 0 {
				frame, err := s.rewrite(s.frames[i].frame, fields)
				if err != nil {
					return err
				}
				s.frames[i].frame = frame
			}
			total += len(s.frames[i].frame)
		}
		if total > s.window {
			return ErrSurface
		}
		s.bytes = total
		for id, text := range changed {
			s.texts[id] = text
		}
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
	s.request.Partial = false
	if err := s.inspect(ctx); err != nil {
		return nil, err
	}
	out := make([][]byte, len(s.frames))
	for i, f := range s.frames {
		out[i] = f.frame
	}
	s.frames = nil
	s.texts = map[string]string{}
	s.ids = nil
	s.bytes = 0
	return out, nil
}
func (s *Stream) Decisions() []Evidence { return slices.Clone(s.decisions) }
