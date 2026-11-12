package gateway

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
)

// Two kinds of header describe a response beyond what the upstream said. Every
// key reports its remaining request and token allowance in the family its
// surface's SDKs already read, so a client can pace itself; a key whose policy
// sets response_metadata also learns how the gateway served the request. Both
// are written before the response is committed, from what admission and the
// attempt loop already hold, and neither costs a round trip.

const (
	headerAttempts      = "X-Olp-Attempts"
	headerRouteRevision = "X-Olp-Route-Revision"
	headerProvider      = "X-Olp-Provider"
	headerCost          = "X-Olp-Cost"
)

// exposedHeaders is what a browser client may read from an inference response,
// in the spelling the headers are documented in. Header names match without
// regard to case, so this is the list of the names above and of those a request
// already carried.
const exposedHeaders = "X-OLP-Cache, Age, X-OLP-Guardrails, X-Request-Id, Retry-After, X-Should-Retry, X-OLP-Delivery-Replay, X-OLP-Resource-ID, X-Goog-Upload-URL, X-Goog-Upload-Status, X-Goog-Upload-Size-Received, " +
	"X-OLP-Attempts, X-OLP-Route-Revision, X-OLP-Provider, X-OLP-Cost, " +
	"x-ratelimit-limit-requests, x-ratelimit-remaining-requests, x-ratelimit-reset-requests, " +
	"x-ratelimit-limit-tokens, x-ratelimit-remaining-tokens, x-ratelimit-reset-tokens, " +
	"anthropic-ratelimit-requests-limit, anthropic-ratelimit-requests-remaining, anthropic-ratelimit-requests-reset, " +
	"anthropic-ratelimit-tokens-limit, anthropic-ratelimit-tokens-remaining, anthropic-ratelimit-tokens-reset"

// exposedHeadersAnthropic adds the request id the Anthropic SDKs read.
const exposedHeadersAnthropic = exposedHeaders + ", Request-Id"

// metadata collects what the gateway tells a key that opted in: the attempts it
// made, the route revision that served, the vendor that answered, and for a
// response that is not a stream, its cost. A request that made no attempt, as
// one answered from the gateway's own records is, has none of these to tell.
func (x *call) metadataHeaders(l *fault.HeaderList, text []byte, streamed bool) []byte {
	if x.attemptCount == 0 && !x.cacheHit() {
		return text
	}
	text = l.Integer(text, headerAttempts, int64(x.attemptCount))
	if x.route != nil && x.route.RevisionID != "" {
		text = l.Str(text, headerRouteRevision, x.route.RevisionID)
	}
	// A provider that has no vendor, as a plugin's has none, names no one.
	if x.attemptVendor != "" {
		text = l.Str(text, headerProvider, x.attemptVendor)
	}
	if streamed {
		return text
	}
	if cost, ok := x.responseCost(); ok {
		text = l.Str(text, headerCost, cost.String())
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
func (x *call) responseHeaders(h http.Header, streamed bool) {
	x.cacheHeaders(h)
	x.guardrailHeaders(h, streamed, false)
	surface, state := x.clientSurface(), x.lease.RateState()
	reset := x.lease.RateReset()
	if x.cacheRateLease != nil {
		state = withCacheRequestAllowance(state, x.cacheRateLease.RateState())
		if !state.LimitsTokens() {
			reset = x.cacheRateLease.RateReset()
		}
	}
	if !x.responseMetadata && (!fault.ReportsRate(surface) || !state.Limited()) {
		return
	}
	var (
		buf  fault.HeaderBuffer
		list fault.HeaderList
	)
	text := list.Rate(buf[:0], surface, state, reset)
	if x.responseMetadata {
		text = x.metadataHeaders(&list, text, streamed)
	}
	list.Apply(h, text)
}

// withCacheRequestAllowance reports the request counts a cache-aware admission
// reserved separately. The window end follows them when no token lease states one.
func withCacheRequestAllowance(state, requests limits.RateState) limits.RateState {
	merged := state
	merged.RequestLimit = requests.RequestLimit
	merged.RequestRemaining = requests.RequestRemaining
	if !state.LimitsTokens() {
		merged.ResetAt = requests.ResetAt
	}
	return merged
}

type streamedHeaders struct{ x *call }

func (h streamedHeaders) Opening(header http.Header) { h.x.responseHeaders(header, true) }

func (h streamedHeaders) Closing(header http.Header, committed bool) {
	h.x.guardrailHeaders(header, true, committed)
}
