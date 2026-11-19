package estimate

import "unicode/utf8"

// ExactBytes is how much of a request's text is counted exactly. Past it the
// rest is charged at the token-to-byte ratio of the exact part, and the count
// is reported as calibrated.
//
// Exact encoding costs time in proportion to the text, on every request, and at
// the 3,000 requests per second of the high-throughput scenario each
// millisecond of it is three cores. BenchmarkCount, on a shared Haswell-class
// core (the fastest of seven runs, on a machine that was never idle), counts
// 100,000 tokens in 5 to 6 ms of English prose, code or JSON and in 10 to 18 ms
// of mixed Chinese, Japanese, Hindi, Arabic and Russian. That is past the 5 ms
// that justifies a bound, and the cost grows with the prompt. BenchmarkMeter
// shows what 32 KiB buys: the same 100,000-token prompts cost admission 0.4 to
// 0.7 ms of prose, code or JSON and 1.2 ms of the multilingual text, whatever
// their length. And 32 KiB is about 8,000 tokens of prose, so interactive
// prompts are still counted exactly. Only retrieval and agent contexts pay the
// approximation.
//
// What the approximation costs is accuracy, and how much depends on how alike
// the exact part and the rest are. A prompt of one kind of text, however long,
// is within a quarter of a percent of its exact count, which the
// four-characters rule is not for prose, code or a script of its own. A prompt
// that changes kind after its first 32 KiB, such as a short instruction ahead of
// a long document in another script, applies a ratio that is wrong for the
// rest: of the mixes TestMixedPromptsStayWithinBounds counts, the worst, a page
// of prose ahead of 370 KB of Chinese and Japanese, comes out 44% under, and the
// four-characters rule is closer than the ratio on some of them, so the ratio
// of the head does not beat every fixed rule. The count is calibrated, not
// exact, and says so, and the usage reports hold it against what the upstream
// reports. Counting a sample from the end of the prompt as well would narrow the
// error, at the price of a second exact pass that costs as much again.
//
// The bound is in bytes, which is not quite time. The worst text for it is one
// unbroken piece just inside the bound, which only the heap merge can take:
// BenchmarkMeterWorstCase puts that at 7 to 9 ms. It costs a request about what
// exact counting of an honest 100,000-token prompt would have, so it is no
// reason to count less of everything else.
const ExactBytes = 32 << 10

// minSample is the least text a tail is extrapolated from. A ratio measured on
// a few bytes, such as a one-line system prompt that comes ahead of an unbroken
// piece too long to count, says nothing about the piece: against exact counts
// it is out by a factor of two either way. Under the sample the tail is charged
// at four bytes to a token, the rule for a size alone, and the count is
// reported as the heuristic it then is.
const minSample = ExactBytes / 8

// scanWindow is how far past what is left of the exact budget a Meter reads. A
// piece of text that crosses the budget is not counted, whatever it is, so what
// lies beyond the budget matters only in that it is there; reading a little
// past it lets the piece that straddles the budget end where it ends, and
// reading no more keeps the work on a long unbroken run bounded. A walked prompt
// keeps this much past the budget as well, and so can hand a Meter every byte it
// would read of a prompt it did not keep whole.
const scanWindow = 2 << 10

// Meter totals the text of one request for one counter. The request's text is
// added one segment at a time, a segment being whatever the tokenizer counts
// on its own, such as one message field: tokens do not merge across segments.
// A Meter is not safe for concurrent use. Its zero value counts nothing and
// reports a heuristic zero; get one from Counter.Meter.
type Meter struct {
	counter Counter
	// encoding is nil when the counter is a heuristic one, including when the
	// encoding's rank data could not be loaded.
	encoding *Encoding

	// Tokenizer counts: the exact tokens of the first exactBytes bytes added,
	// and how many bytes were left uncounted after them. A tail implies exact
	// bytes, since a meter with none turns into a heuristic one.
	budget     int
	tokens     int64
	exactBytes int64
	tailBytes  int64

	// heuristic is the sum of each segment's heuristic charge.
	heuristic int64
}

// Meter starts a total for a request. It loads the counter's encoding if this
// is its first use.
func (c Counter) Meter() Meter {
	m := Meter{counter: c}
	if c.encoding != nil && c.encoding.load() == nil {
		m.encoding, m.budget = c.encoding, ExactBytes
	}
	return m
}

// Add counts one segment of text.
func (m *Meter) Add(text string) { m.add(text, 0, -1) }

// add counts one segment, which the caller may not hold whole: text is its
// first bytes and rest is how many bytes follow them, as a request walker that
// keeps only what a counter reads has it. The caller may also know the
// segment's heuristic charge, as a walker that measures each field as it reads
// it does; a negative charge means unknown, and only a segment held whole may
// be unknown. The charge is only read when the meter is a heuristic one, so the
// exact path never pays for it.
func (m *Meter) add(text string, rest, heuristic int64) {
	if text == "" && rest == 0 {
		return
	}
	if m.encoding == nil {
		m.heuristic += measured(text, heuristic)
		return
	}
	// Once any text is left out, later text is too, so the ratio applies to
	// one contiguous tail.
	var covered int
	if m.tailBytes == 0 && m.budget > 0 {
		var n int
		n, covered, _ = m.encoding.countPrefix(prefixOf(text, m.budget+scanWindow), m.budget)
		m.tokens += int64(n)
		m.exactBytes += int64(covered)
		m.budget -= covered
	}
	if covered < len(text) || rest > 0 {
		if m.exactBytes == 0 {
			// Nothing was counted exactly, because the first piece alone is past
			// the bound, so there is no ratio to charge the rest by. The meter
			// becomes a heuristic one, which says so in its provenance.
			m.encoding = nil
			m.heuristic += measured(text, heuristic)
			return
		}
		m.tailBytes += int64(len(text)-covered) + rest
	}
}

// Total returns the tokens added so far and how they were counted.
func (m *Meter) Total() (int64, Provenance) {
	if m.encoding == nil {
		return m.counter.scale(m.heuristic)
	}
	if m.tailBytes == 0 {
		return m.tokens, ProvenanceTokenizer
	}
	if m.exactBytes < minSample {
		return addBounded(m.tokens, HeuristicBytesTokens(int(m.tailBytes))), ProvenanceHeuristic
	}
	// The tail costs what the exact part cost per byte, rounded up so the
	// estimate errs toward reserving too much.
	tail := (m.tailBytes*m.tokens + m.exactBytes - 1) / m.exactBytes
	return m.tokens + tail, ProvenanceCalibrated
}

// measured is a segment's heuristic charge: the one already known, or counted.
func measured(text string, known int64) int64 {
	if known >= 0 {
		return known
	}
	return HeuristicTokens(text)
}

// prefixOf is the longest prefix of text that is at most n bytes and does not
// end inside a character.
func prefixOf(text string, n int) string {
	if n >= len(text) {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}
