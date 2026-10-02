package estimate

import (
	"encoding/json"
	"slices"
	"sync"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Count is how many tokens a counter finds in a prompt, and how it counted
// them.
type Count struct {
	Tokens     int64
	Provenance Provenance
}

// Prompt is a request walked once. Walking reads every prompt field of every
// dialect into text segments and flat charges, which no model's tokenizer has
// been asked about yet; counting for a model's family happens on demand, and
// once per family however many attempts, credential slots or translated
// targets ask. A request routed to several models of one family is counted for
// that family once, and a request routed to models of several is counted once
// for each.
//
// The text charged is a message's content, its tool calls, a tool result,
// the name and arguments a call travels with, and a tool catalogue with its
// schemas. Roles and the per-message framing count only for the families that
// document a framing, which are OpenAI's. Images and media parts cost a flat
// charge each, whatever counts the rest. A prompt of a request that is not a
// generation, such as an embedding, has no reply to reserve.
//
// A request is held for as long as its upstream answers, which for a stream may
// be an hour, and a prompt is held with it, so a prompt keeps no more of the
// text than a counter reads: the first retainBytes bytes, and for the rest how
// many bytes there were. That is every count there is to make, and it is what
// keeps a megabyte request from weighing twice.
type Prompt struct {
	request    *openai.Request
	generation bool
	in         input
	tally      *tally
}

// tally is the counts of one prompt, which a prompt of the same text can share.
type tally struct {
	mu      sync.Mutex
	counted []counted
	observe func(Family)
}

type counted struct {
	family Family
	count  Count
}

// Walk reads the prompt of a request. A nil request is a generation with no
// prompt, as admission sees one that named nothing.
func Walk(request *openai.Request) *Prompt {
	p := &Prompt{request: request, generation: request == nil || request.Family.Operation() == openai.OperationGeneration, tally: &tally{}}
	if request != nil {
		walkInput(&p.in, request.Family, func(name string) json.RawMessage {
			if raw := request.Field(name); len(raw) > 0 {
				return raw
			}
			return nil
		})
	}
	return p
}

// Request is the request the prompt was walked from.
func (p *Prompt) Request() *openai.Request { return p.request }

// Observe makes fn run each time the prompt is counted for a family, and not
// when a count is reused. It is how a test, or a metric, sees that a family
// was counted once. The text a provider's defaults add is small and counted for
// each estimate, and is not observed. fn runs with the prompt's counts locked,
// so it must not use the prompt.
func (p *Prompt) Observe(fn func(Family)) *Prompt {
	p.tally.mu.Lock()
	p.tally.observe = fn
	p.tally.mu.Unlock()
	return p
}

// Follow shares the counts of source when this prompt reads the same as it. The
// request a provider receives is often the one the caller sent, in another
// dialect, and counting it again would find the same tokens.
func (p *Prompt) Follow(source *Prompt) {
	if p != source && p.in.equal(&source.in) {
		p.tally = source.tally
	}
}

// Input counts the prompt for a counter, caching the count by the counter's
// family. It is the tokens of the input alone, without any allowed reply.
func (p *Prompt) Input(c Counter) Count {
	family := c.Family()
	t := p.tally
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, entry := range t.counted {
		if entry.family == family {
			return entry.count
		}
	}
	count := c.count(&p.in, false)
	t.counted = append(t.counted, counted{family, count})
	if t.observe != nil {
		t.observe(family)
	}
	return count
}

// Estimate is what admission reserves for one request on one model's counter.
type Estimate struct {
	// Input is the tokens of the prompt, at least one for a request that is not
	// a generation.
	Input int64
	// Output is the reply bound the request or its provider named, and nil when
	// neither did. Candidates is how many replies it asked for.
	Output     *int64
	Candidates int64
	// Provenance says how Input was counted, and Family is the counter's.
	Provenance Provenance
	Family     Family

	generation bool
}

// Tokens is what the request may consume, charged before the upstream reports
// what it used: the prompt, plus for a generation the largest reply the caller
// allowed, the bound it named for one candidate multiplied by the candidates it
// asked for. It is deliberately generous: a reservation is reconciled against
// the real usage as soon as the attempt ends, and admitting work that cannot fit
// in the window is worse than deferring work that would have.
func (e Estimate) Tokens() int64 {
	if !e.generation {
		return max(e.Input, 1)
	}
	bound := int64(DefaultOutputTokens)
	if e.Output != nil {
		bound = *e.Output
	}
	return max(addBounded(e.Input, multiplyBounded(max(bound, 1), max(e.Candidates, 1))), 1)
}

// Estimate prices the request for the counter of the model it is sent to. A
// provider's defaults complete what the request left out: its reply bound,
// its candidates, and the text of an input field the caller omitted.
func (p *Prompt) Estimate(c Counter, defaults map[string]json.RawMessage) Estimate {
	count := p.Input(c)
	e := Estimate{Input: count.Tokens, Provenance: count.Provenance, Family: c.Family(), Candidates: 1, generation: p.generation}
	if extra := p.defaulted(defaults); extra != nil {
		// The reply is primed once, by whichever input has the first message.
		more := c.count(extra, p.in.messages > 0)
		e.Input = addBounded(e.Input, more.Tokens)
		e.Provenance = Weaker(e.Provenance, more.Provenance)
	}
	if !p.generation {
		e.Input = max(e.Input, 1)
		return e
	}
	e.Output, e.Candidates = outputBounds(p.request, defaults)
	return e
}

// defaulted walks the prompt fields a provider's defaults supply because the
// caller left them out. Almost no default does, so it is nil almost always.
func (p *Prompt) defaulted(defaults map[string]json.RawMessage) *input {
	// A request that named nothing has no prompt, whatever a provider defaults.
	// Nearly every provider's defaults are bounds and sampling, which are not.
	if p.request == nil || !slices.ContainsFunc(promptFields, func(name string) bool { return len(defaults[name]) > 0 }) {
		return nil
	}
	var in input
	walkInput(&in, p.request.Family, func(name string) json.RawMessage {
		if len(p.request.Field(name)) > 0 {
			return nil
		}
		return defaults[name]
	})
	if in.empty() {
		return nil
	}
	return &in
}

// Weaker is the provenance of two counts taken together: the less trustworthy.
func Weaker(a, b Provenance) Provenance {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func rank(p Provenance) int {
	switch p {
	case ProvenanceTokenizer:
		return 0
	case ProvenanceCalibrated:
		return 1
	}
	return 2
}

// count totals one input for the counter. A counter without a tokenizer reports
// the heuristic charges the walk already measured, scaled by its factor; one
// with a tokenizer counts the text exactly, up to the bound a Meter keeps, and
// adds the family's framing, without priming the reply again when primed says
// another input already did. Flat charges stand outside both.
func (c Counter) count(in *input, primed bool) Count {
	m := c.Meter()
	if m.encoding == nil {
		tokens, provenance := c.scale(in.heuristic)
		return Count{addBounded(tokens, in.flat), provenance}
	}
	for _, s := range in.text {
		m.add(s.text, s.rest, s.heuristic)
	}
	// Roles are text a tokenizer reads, and a meter that has degraded to the
	// heuristic has none to read them with.
	if m.encoding != nil {
		for _, role := range in.roles {
			m.Add(role)
		}
	}
	tokens, provenance := m.Total()
	if m.encoding != nil {
		// The framing is the family's own, and what it frames was counted exactly
		// even when the tail was charged at the heuristic after it, so it is
		// charged either way. A meter that counted nothing exactly has become a
		// heuristic one and has no framing to charge.
		framing := c.Framing()
		if primed {
			framing.Reply = 0
		}
		tokens = addBounded(tokens, framing.Overhead(in.messages, in.names))
		// An exact count of the text is not an exact count of a request whose
		// images and media were charged at a flat rate, or whose tools a model
		// reads in a rendering the count does not know.
		if in.approx && provenance == ProvenanceTokenizer {
			provenance = ProvenanceCalibrated
		}
	}
	return Count{addBounded(tokens, in.flat), provenance}
}
