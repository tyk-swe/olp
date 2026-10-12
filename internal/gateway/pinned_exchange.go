package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
)

// pinnedExchange owns transport evidence and its one final fact. The operation
// validates completion; durable mapping and downstream delivery are later,
// independent decisions. Pinned operations never enter cross-target failover.
type pinnedExchange struct {
	state    attemptState
	index    int
	billable bool
}

func (s *Server) beginPinned(ctx context.Context, x *execution, p *pin, method string) *pinnedExchange {
	x.finalizePinned(nil, s.now())
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1, &p.provider)
	fact.Mode = x.mode
	if p.hold != nil {
		fact.Budgets = p.hold.budgets
	}
	exchange := &pinnedExchange{
		state: attemptState{parent: ctx, classifier: upstream.Classifier{AtMostOnce: method == http.MethodPost, Declared: p.provider.Connector().Classification()}},
		index: len(x.facts), billable: (x.family == openai.FamilyGeminiInteractions || x.family == openai.FamilyBedrock || x.family == openai.FamilyBedrockInvoke) && x.actor != "api_key_resource",
	}
	x.facts = append(x.facts, fact)
	x.exchange = exchange
	return exchange
}

func (e *pinnedExchange) fail(s *Server, x *execution, class string, failure *attemptFailure) *attemptFailure {
	if failure == nil {
		failure = &attemptFailure{}
	}
	failure.class, failure.dispatched = class, e.state.dispatched.Load()
	failure.credentialRefused = e.state.credentialRefused
	failure.acceptance = e.state.evidence().Acceptance()
	failure.accepted = e.state.accepted
	x.dispatched = x.dispatched || failure.dispatched
	fact := &x.facts[e.index]
	fact.Class = class
	if failure.status != 0 && fact.Status == 0 {
		fact.Status = failure.status
	}
	if failure.retryAfter > 0 {
		fact.RetryAfter = &failure.retryAfter
	}
	x.finalizePinned(nil, s.now())
	return failure
}

func (s *Server) doPinnedRequest(ctx context.Context, x *execution, p *pin, exchange *pinnedExchange, req *http.Request, decodeError func([]byte) *openai.UpstreamError) (*http.Response, *attemptFailure) {
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, exchange.fail(s, x, classCredential, nil)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), exchange.state.trace()))
	resp, err := client.Do(req)
	if resp != nil {
		// A custom RoundTripper may not invoke httptrace; an HTTP response
		// independently proves transmission.
		exchange.state.dispatched.Store(true)
	}
	x.dispatched = x.dispatched || exchange.state.dispatched.Load()
	if exchange.state.dispatched.Load() && p.hold != nil {
		x.spendCaps(p.hold.budgets, p.attempt)
	}
	if err != nil {
		return nil, exchange.fail(s, x, exchange.state.classify(err, false), nil)
	}
	fact := &x.facts[exchange.index]
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte, fact.Status = &received, resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		exchange.state.accepted = true
		// Headers prove acceptance, never valid completion or delivery.
		fact.Class = classProtocol
		x.finalizePinned(nil, s.now())
		return resp, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	_ = resp.Body.Close()
	failure := &attemptFailure{status: resp.StatusCode, upstream: x.redacted(decodeError(raw))}
	class := exchange.state.rejected(resp.StatusCode, failure.upstream)
	if class == classRateLimit {
		failure.retryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), s.now())
	}
	return nil, exchange.fail(s, x, class, failure)
}

// completePinned records validated upstream completion before any local
// persistence or delivery that can fail without changing the provider's result.
func (x *execution) completePinned() {
	if e := x.exchange; e != nil {
		e.state.settled = true
		x.facts[e.index].Class = classSuccess
	}
}

func (x *execution) finalizePinned(out *outcome, at time.Time) {
	e := x.exchange
	if e == nil {
		return
	}
	fact := &x.facts[e.index]
	fact.Duration = at.Sub(fact.StartedAt)
	if out != nil {
		fact.Committed = fact.Committed || out.committed
	}
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = string(e.state.evidence().Acceptance())
	}
	uncertain := e.billable && e.state.evidence().Acceptance().Unresolved()
	// Valid terminal work without usage can still have unknown consumption.
	uncertain = uncertain || e.billable && e.state.accepted && fact.Usage == nil
	fact.recordEvidence(uncertain)
}
