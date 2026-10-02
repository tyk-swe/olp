package gateway

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

// Two kinds of header describe a response beyond what the upstream said. Every
// key reports its remaining request and token allowance in the family its
// surface's SDKs already read, so a client can pace itself; a key whose policy
// sets response_metadata also learns how the gateway served the request. Both
// are written before the response is committed, from what admission and the
// attempt loop already hold, and neither costs a round trip.

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

	headerAttempts      = "X-Olp-Attempts"
	headerRouteRevision = "X-Olp-Route-Revision"
	headerProvider      = "X-Olp-Provider"
	headerCost          = "X-Olp-Cost"
)

// exposedHeaders is what a browser client may read from an inference response,
// in the spelling the headers are documented in. Header names match without
// regard to case, so this is the list of the names above and of those a request
// already carried.
const exposedHeaders = "X-Request-Id, Retry-After, X-Should-Retry, X-OLP-Delivery-Replay, " +
	"X-OLP-Attempts, X-OLP-Route-Revision, X-OLP-Provider, X-OLP-Cost, " +
	"x-ratelimit-limit-requests, x-ratelimit-remaining-requests, x-ratelimit-reset-requests, " +
	"x-ratelimit-limit-tokens, x-ratelimit-remaining-tokens, x-ratelimit-reset-tokens, " +
	"anthropic-ratelimit-requests-limit, anthropic-ratelimit-requests-remaining, anthropic-ratelimit-requests-reset, " +
	"anthropic-ratelimit-tokens-limit, anthropic-ratelimit-tokens-remaining, anthropic-ratelimit-tokens-reset"

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

// headerList collects the headers one response adds, so that they cost two
// allocations however many there are: their values are written one after the
// other into a single buffer that becomes one string, and the lists the header
// map holds are slices of one array. A response that adds nothing allocates
// nothing. The buffer is the caller's, large enough for every value a response
// can carry, and is passed through each step the way append is, so that it stays
// on the stack.
type headerList struct {
	names [maxResponseHeaders]string
	ends  [maxResponseHeaders]int
	count int
}

// maxResponseHeaders is the most a response adds: the six rate-limit headers of a
// key limited on both dimensions, and the four of metadata.
const maxResponseHeaders = 10

// headerBuffer is the room a response's header values are written in.
type headerBuffer [320]byte

// add names the value that ends the text.
func (l *headerList) add(name string, text []byte) {
	l.names[l.count], l.ends[l.count] = name, len(text)
	l.count++
}

func (l *headerList) integer(text []byte, name string, n int64) []byte {
	text = strconv.AppendInt(text, n, 10)
	l.add(name, text)
	return text
}

func (l *headerList) str(text []byte, name, value string) []byte {
	text = append(text, value...)
	l.add(name, text)
	return text
}

// apply stores every collected value in h, replacing any the header already held.
func (l *headerList) apply(h http.Header, text []byte) {
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

// rate collects the allowance a surface reports, for the dimensions the key
// limits and no others. resetAfter is the time left in the window, which a
// surface that states an instant does not use.
func (l *headerList) rate(text []byte, surface string, state limits.RateState, resetAfter time.Duration) []byte {
	names := rateHeadersOf(surface)
	if names == nil || !state.Limited() {
		return text
	}
	if state.LimitsRequests() {
		text = l.integer(text, names.limitRequests, state.RequestLimit)
		text = l.integer(text, names.remainingRequests, state.RequestRemaining)
		text = l.reset(text, names.resetRequests, names.instant, state, resetAfter)
	}
	if state.LimitsTokens() {
		text = l.integer(text, names.limitTokens, state.TokenLimit)
		text = l.integer(text, names.remainingTokens, state.TokenRemaining)
		text = l.reset(text, names.resetTokens, names.instant, state, resetAfter)
	}
	return text
}

func (l *headerList) reset(text []byte, name string, instant bool, state limits.RateState, after time.Duration) []byte {
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

// metadata collects what the gateway tells a key that opted in: the attempts it
// made, the route revision that served, the vendor that answered, and for a
// response that is not a stream, its cost. A request that made no attempt, as
// one answered from the gateway's own records is, has none of these to tell.
func (l *headerList) metadata(text []byte, x *execution, streamed bool) []byte {
	if x.attemptCount == 0 {
		return text
	}
	text = l.integer(text, headerAttempts, int64(x.attemptCount))
	if x.route != nil && x.route.RevisionID != "" {
		text = l.str(text, headerRouteRevision, x.route.RevisionID)
	}
	// A provider that has no vendor, as a plugin's has none, names no one.
	if x.attemptVendor != "" {
		text = l.str(text, headerProvider, x.attemptVendor)
	}
	if streamed {
		return text
	}
	if cost, ok := x.responseCost(); ok {
		text = l.str(text, headerCost, cost.String())
	}
	return text
}

// responseHeaders adds what a successful response says about the key's
// allowance and, for a key that opted in, about how it was served, to the
// headers that are about to be committed with it. It runs before every success
// WriteHeader, and for a stream before its first frame: the attempt that serves
// a stream has been recorded on the execution by then, though its fact has not.
//
// The allowance is the one the key's admission reservation measured, at that
// time: the reset is counted down to now, but the counts are not read again.
// A key that limits nothing, a request that was admitted without a reservation,
// and a surface with no rate-limit headers add none.
func (x *execution) responseHeaders(h http.Header, streamed bool) {
	surface, state := x.clientSurface(), x.lease.RateState()
	if !x.responseMetadata && (rateHeadersOf(surface) == nil || !state.Limited()) {
		return
	}
	var (
		buf  headerBuffer
		list headerList
	)
	text := list.rate(buf[:0], surface, state, x.lease.RateReset())
	if x.responseMetadata {
		text = list.metadata(text, x, streamed)
	}
	list.apply(h, text)
}

// setRateLimitHeaders adds the allowance a rejection reports. The reservation
// that refused the request reserved nothing, so the counts are what the window
// still held and the reset is the one the script measured a moment ago.
func setRateLimitHeaders(h http.Header, surface string, state limits.RateState) {
	if !state.Limited() || rateHeadersOf(surface) == nil {
		return
	}
	var (
		buf  headerBuffer
		list headerList
	)
	list.apply(h, list.rate(buf[:0], surface, state, state.ResetAfter))
}
