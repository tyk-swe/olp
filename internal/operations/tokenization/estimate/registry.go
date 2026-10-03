package estimate

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Family groups the models that share a tokenizer, which is what an estimate
// depends on and what usage reports compare estimates by.
type Family string

const (
	// FamilyOpenAIO200k is the models that use the o200k_base encoding.
	FamilyOpenAIO200k Family = "openai-o200k"
	// FamilyOpenAICL100k is the models that use the cl100k_base encoding.
	FamilyOpenAICL100k Family = "openai-cl100k"
	// FamilyAnthropic is Claude. Its tokenizer is not public.
	FamilyAnthropic Family = "anthropic"
	// FamilyGemini is Gemini. Its tokenizer is not public either.
	FamilyGemini Family = "gemini"
	// FamilyOther is every model the registry does not recognize, including
	// hosted open-weight models and deployments whose names say nothing.
	FamilyOther Family = "other"
)

// Provenance says how much an estimate can be trusted. The values are the
// strings usage records store.
type Provenance string

const (
	// ProvenanceTokenizer is an exact count by the model's own tokenizer of a
	// prompt that is all text and message framing. A prompt with a part the
	// count cannot see as the model does, or leaves out, is calibrated.
	ProvenanceTokenizer Provenance = "tokenizer"
	// ProvenanceCalibrated is a count that is partly a measured ratio or a
	// guess: the tail of a prompt past ExactBytes, a family's heuristic times
	// its factor, or an exact count of text in a prompt that also has images,
	// documents or media, charged at a flat rate, tool schemas, tool calls,
	// structured-output schemas and reasoning, which a model reads in a rendering
	// of its own, or encrypted content that no count reads.
	ProvenanceCalibrated Provenance = "calibrated"
	// ProvenanceHeuristic is the unscaled four-characters-per-token rule. It is
	// also the count of a prompt whose tail past ExactBytes had too little exact
	// text before it to measure a ratio from, and was charged at four bytes to a
	// token.
	ProvenanceHeuristic Provenance = "heuristic"
)

// Calibration factors scale the heuristic for families that have no public
// tokenizer. They stay at 1 until the reference catalog (roadmap M2.4) carries
// measured values; a factor other than 1 makes the heuristic count calibrated.
const (
	anthropicFactor = 1.0
	geminiFactor    = 1.0
	otherFactor     = 1.0
)

// CharsPerToken is the ratio the heuristic charges text at. Four characters
// per token is a conservative portable approximation across tokenizers.
const CharsPerToken = 4

// HeuristicTokens charges text at four characters per token, rounded up so no
// text is free. Characters are counted rather than bytes, so a multi-byte
// script is not overcharged.
func HeuristicTokens(text string) int64 {
	return int64((utf8.RuneCountInString(text) + CharsPerToken - 1) / CharsPerToken)
}

// HeuristicBytesTokens charges n bytes at four bytes per token, rounded up. It
// is for callers that hold only a size, such as an opaque document.
func HeuristicBytesTokens(n int) int64 {
	return int64((n + CharsPerToken - 1) / CharsPerToken)
}

// Counter counts the tokens of a model's text. The zero value is a heuristic
// counter for FamilyOther without a factor.
type Counter struct {
	family Family
	// encoding is nil for a family without a public tokenizer.
	encoding *Encoding
	// factor scales the heuristic; zero means one.
	factor float64
}

// ForModel returns the counter for an upstream model name, the name the
// provider is sent. A name the registry cannot place gets the heuristic: a
// wrong tokenizer would miscount silently, so nothing defaults to one.
func ForModel(model string) Counter {
	switch family := FamilyOf(model); family {
	case FamilyOpenAIO200k:
		return Counter{family: family, encoding: O200kBase}
	case FamilyOpenAICL100k:
		return Counter{family: family, encoding: CL100kBase}
	case FamilyAnthropic:
		return Counter{family: family, factor: anthropicFactor}
	case FamilyGemini:
		return Counter{family: family, factor: geminiFactor}
	default:
		return Counter{family: FamilyOther, factor: otherFactor}
	}
}

// scale applies the family's factor to a heuristic total. A factor of one
// leaves the plain four-characters rule, which says so in its provenance.
func (c Counter) scale(heuristic int64) (int64, Provenance) {
	if c.factor <= 0 || c.factor == 1 {
		return heuristic, ProvenanceHeuristic
	}
	return int64(math.Ceil(float64(heuristic) * c.factor)), ProvenanceCalibrated
}

// Family is the family the counter was resolved to.
func (c Counter) Family() Family {
	if c.family == "" {
		return FamilyOther
	}
	return c.family
}

// Count returns the tokens in one text and how the count was made. It is
// Meter for a request that is a single text.
func (c Counter) Count(text string) (int64, Provenance) {
	m := c.Meter()
	m.Add(text)
	return m.Total()
}

// FamilyOf places an upstream model name in a family. The OpenAI rules are
// those of tiktoken 0.14.0 (tiktoken/model.py), an exact name or a prefix,
// plus gpt-oss, whose o200k_harmony encoding has the ranks and pattern of
// o200k_base. Provider path prefixes ("openai/gpt-4o"), fine-tune prefixes
// ("ft:gpt-4o-mini:org::id") and variant suffixes (":online") are removed
// first. Claude and Gemini are recognized by a name part, as in
// "us.anthropic.claude-sonnet-4" or "models/gemini-2.5-pro".
func FamilyOf(model string) Family {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimPrefix(name, "ft:")
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	if family, ok := openAIFamily(name); ok {
		return family
	}
	switch {
	case hasPart(name, "claude"):
		return FamilyAnthropic
	case hasPart(name, "gemini"):
		return FamilyGemini
	}
	return FamilyOther
}

// openAIExact and openAIPrefixes are tiktoken's MODEL_TO_ENCODING and
// MODEL_PREFIX_TO_ENCODING for the two encodings the estimator carries. The
// models of older encodings (r50k_base, p50k_base and their relatives) are
// left to the heuristic. A prefix ending in a hyphen only matches a name that
// continues with a variant, as tiktoken's do; gpt-5 is a bare prefix there too.
var (
	openAIExact = map[string]Family{
		"o1": FamilyOpenAIO200k, "o3": FamilyOpenAIO200k, "o4-mini": FamilyOpenAIO200k,
		"gpt-5": FamilyOpenAIO200k, "gpt-4.1": FamilyOpenAIO200k, "gpt-4o": FamilyOpenAIO200k,
		"gpt-4": FamilyOpenAICL100k, "gpt-3.5-turbo": FamilyOpenAICL100k, "gpt-3.5": FamilyOpenAICL100k,
		"gpt-35-turbo": FamilyOpenAICL100k, "davinci-002": FamilyOpenAICL100k, "babbage-002": FamilyOpenAICL100k,
		"text-embedding-ada-002": FamilyOpenAICL100k, "text-embedding-3-small": FamilyOpenAICL100k,
		"text-embedding-3-large": FamilyOpenAICL100k,
	}
	openAIPrefixes = []struct {
		prefix string
		family Family
	}{
		{"o1-", FamilyOpenAIO200k}, {"o3-", FamilyOpenAIO200k}, {"o4-mini-", FamilyOpenAIO200k},
		{"gpt-5", FamilyOpenAIO200k}, {"gpt-4.5-", FamilyOpenAIO200k}, {"gpt-4.1-", FamilyOpenAIO200k},
		{"chatgpt-4o-", FamilyOpenAIO200k}, {"gpt-4o-", FamilyOpenAIO200k}, {"gpt-oss-", FamilyOpenAIO200k},
		{"gpt-4-", FamilyOpenAICL100k}, {"gpt-3.5-turbo-", FamilyOpenAICL100k}, {"gpt-35-turbo-", FamilyOpenAICL100k},
	}
)

func openAIFamily(name string) (Family, bool) {
	if family, ok := openAIExact[name]; ok {
		return family, true
	}
	for _, p := range openAIPrefixes {
		if strings.HasPrefix(name, p.prefix) {
			return p.family, true
		}
	}
	return "", false
}

// hasPart reports whether name has part as one of the pieces between
// separators, so "claude" is in "anthropic.claude-3" and not in "claudette".
func hasPart(name, part string) bool {
	for from := 0; from < len(name); {
		i := strings.Index(name[from:], part)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(part)
		if (start == 0 || !isAlnum(name[start-1])) && (end == len(name) || !isAlnum(name[end])) {
			return true
		}
		from = start + 1
	}
	return false
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}
