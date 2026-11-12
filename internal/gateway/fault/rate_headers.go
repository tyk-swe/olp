package fault

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

// The header names are in canonical form: they are stored in the header map as
// written, which is how a response avoids canonicalising each one.
const (
	headerOpenAILimitRequests     = "X-Ratelimit-Limit-Requests"
	headerOpenAIRemainingRequests = "X-Ratelimit-Remaining-Requests"
	headerOpenAIResetRequests     = "X-Ratelimit-Reset-Requests"
	headerOpenAILimitTokens       = "X-Ratelimit-Limit-Tokens"
	headerOpenAIRemainingTokens   = "X-Ratelimit-Remaining-Tokens"
	headerOpenAIResetTokens       = "X-Ratelimit-Reset-Tokens"

	headerAnthropicLimitRequests     = "Anthropic-Ratelimit-Requests-Limit"
	headerAnthropicRemainingRequests = "Anthropic-Ratelimit-Requests-Remaining"
	headerAnthropicResetRequests     = "Anthropic-Ratelimit-Requests-Reset"
	headerAnthropicLimitTokens       = "Anthropic-Ratelimit-Tokens-Limit"
	headerAnthropicRemainingTokens   = "Anthropic-Ratelimit-Tokens-Remaining"
	headerAnthropicResetTokens       = "Anthropic-Ratelimit-Tokens-Reset"
)

// rateHeaders names where one surface's SDKs read an allowance.
type rateHeaders struct {
	limitRequests, remainingRequests, resetRequests string
	limitTokens, remainingTokens, resetTokens       string
	// instant says a reset is the RFC 3339 time the window ends, as Anthropic
	// states it; otherwise it is the time that is left, as OpenAI states it.
	instant bool
}

var (
	openAIRateHeaders = rateHeaders{
		headerOpenAILimitRequests, headerOpenAIRemainingRequests, headerOpenAIResetRequests,
		headerOpenAILimitTokens, headerOpenAIRemainingTokens, headerOpenAIResetTokens, false,
	}
	anthropicRateHeaders = rateHeaders{
		headerAnthropicLimitRequests, headerAnthropicRemainingRequests, headerAnthropicResetRequests,
		headerAnthropicLimitTokens, headerAnthropicRemainingTokens, headerAnthropicResetTokens, true,
	}
)

func ReportsRate(surface string) bool { return rateHeadersOf(surface) != nil }

// rateHeadersOf is the header family of a surface, or nil for one whose SDKs read
// none: Gemini and Bedrock have no such headers, and a native operation is
// addressed by no SDK at all.
func rateHeadersOf(surface string) *rateHeaders {
	switch surface {
	case "openai":
		return &openAIRateHeaders
	case "anthropic":
		return &anthropicRateHeaders
	}
	return nil
}

// HeaderList collects the headers one response adds, so that they cost two
// allocations however many there are: their values are written one after the
// other into a single buffer that becomes one string, and the lists the header
// map holds are slices of one array. A response that adds nothing allocates
// nothing. The buffer is the caller's, large enough for every value a response
// can carry, and is passed through each step the way append is, so that it stays
// on the stack.
type HeaderList struct {
	names [maxResponseHeaders]string
	ends  [maxResponseHeaders]int
	count int
}

// maxResponseHeaders is the most a response adds: the six rate-limit headers of a
// key limited on both dimensions, and the four of metadata.
const maxResponseHeaders = 10

// HeaderBuffer is the room a response's header values are written in.
type HeaderBuffer [320]byte

// add names the value that ends the text.
func (l *HeaderList) add(name string, text []byte) {
	l.names[l.count], l.ends[l.count] = name, len(text)
	l.count++
}

func (l *HeaderList) Integer(text []byte, name string, n int64) []byte {
	text = strconv.AppendInt(text, n, 10)
	l.add(name, text)
	return text
}

func (l *HeaderList) Str(text []byte, name, value string) []byte {
	text = append(text, value...)
	l.add(name, text)
	return text
}

// Apply stores every collected value in h, replacing any the header already held.
func (l *HeaderList) Apply(h http.Header, text []byte) {
	if l.count == 0 {
		return
	}
	joined := string(text)
	lists := new([len(l.names)]string)
	begin := 0
	for index := range l.count {
		lists[index] = joined[begin:l.ends[index]]
		h[l.names[index]] = lists[index : index+1 : index+1]
		begin = l.ends[index]
	}
}

// Rate collects the allowance a surface reports, for the dimensions the key
// limits and no others. resetAfter is the time left in the window, which a
// surface that states an instant does not use.
func (l *HeaderList) Rate(text []byte, surface string, state limits.RateState, resetAfter time.Duration) []byte {
	names := rateHeadersOf(surface)
	if names == nil || !state.Limited() {
		return text
	}
	if state.LimitsRequests() {
		text = l.Integer(text, names.limitRequests, state.RequestLimit)
		text = l.Integer(text, names.remainingRequests, state.RequestRemaining)
		text = l.reset(text, names.resetRequests, names.instant, state, resetAfter)
	}
	if state.LimitsTokens() {
		text = l.Integer(text, names.limitTokens, state.TokenLimit)
		text = l.Integer(text, names.remainingTokens, state.TokenRemaining)
		text = l.reset(text, names.resetTokens, names.instant, state, resetAfter)
	}
	return text
}

func (l *HeaderList) reset(text []byte, name string, instant bool, state limits.RateState, after time.Duration) []byte {
	if instant {
		text = state.ResetAt.AppendFormat(text, time.RFC3339)
	} else {
		text = appendResetDuration(text, after)
	}
	l.add(name, text)
	return text
}

// appendResetDuration appends d in the notation time.Duration prints and OpenAI's
// reset headers use: 20ms, 1s, 8.64s, 1m0s. A reset is a whole number of
// milliseconds within a minute, which is written without allocating; any other
// duration is left to time.Duration.
func appendResetDuration(b []byte, d time.Duration) []byte {
	switch {
	case d <= 0:
		return append(b, "0s"...)
	case d%time.Millisecond != 0 || d >= time.Hour:
		return append(b, d.String()...)
	}
	ms := d.Milliseconds()
	if ms < 1000 {
		return append(strconv.AppendInt(b, ms, 10), "ms"...)
	}
	if minutes := ms / 60_000; minutes > 0 {
		b = append(strconv.AppendInt(b, minutes, 10), 'm')
		ms %= 60_000
	}
	b = strconv.AppendInt(b, ms/1000, 10)
	if fraction := ms % 1000; fraction != 0 {
		b = append(b, '.', byte('0'+fraction/100), byte('0'+fraction/10%10), byte('0'+fraction%10))
		for b[len(b)-1] == '0' {
			b = b[:len(b)-1]
		}
	}
	return append(b, 's')
}

// setRateLimitHeaders adds the allowance a rejection reports. The reservation
// that refused the request reserved nothing, so the counts are what the window
// still held and the reset is the one the script measured a moment ago.
func setRateLimitHeaders(h http.Header, surface string, state limits.RateState) {
	if !state.Limited() || rateHeadersOf(surface) == nil {
		return
	}
	var (
		buf  HeaderBuffer
		list HeaderList
	)
	list.Apply(h, list.Rate(buf[:0], surface, state, state.ResetAfter))
}
