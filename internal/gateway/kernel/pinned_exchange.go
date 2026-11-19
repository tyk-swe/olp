package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

type pinnedExchange struct {
	request  *call
	state    attemptState
	index    int
	hold     *dispatchHold
	billable bool
	bound    *boundExchange
	endedAt  time.Time
	now      func() time.Time
}

type pinnedResponse struct {
	*http.Response
	exchange *pinnedExchange
}

func (e *pinnedExchange) fact() *events.AttemptFact {
	return &e.request.facts[e.index]
}

func (s *Server) beginPinned(ctx context.Context, x *call, p *pin, method string) *pinnedExchange {
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1, &p.provider)
	fact.Mode = x.mode
	if x.retainedAdmission != nil {
		fact.PricingPinned = true
	}
	if p.hold != nil {
		fact.Budgets = p.hold.budgets
	}
	retainedBillable := x.retained.billing == retainedMeteredWork || x.retainedAdmission != nil && (fact.Price != nil || fact.CardRev != nil)
	nativeBillable := (x.family == wire.GeminiInteractions || x.family == wire.Bedrock || x.family == wire.BedrockInvoke) && x.actor != "api_key_resource"
	exchange := &pinnedExchange{
		state:   attemptState{parent: ctx, classifier: upstream.Classifier{AtMostOnce: method == http.MethodPost, Declared: p.provider.Connector().Classification()}},
		request: x, index: len(x.facts), hold: p.hold, billable: retainedBillable || nativeBillable, now: s.now,
	}
	exchange.bound = s.bindExchange(x, &p.provider, p.slot, p.model, p.provider.Connector(), &exchange.state)
	x.facts = append(x.facts, fact)
	x.pinned = append(x.pinned, exchange)
	if exchange.hold != nil {
		exchange.hold.pinned = append(exchange.hold.pinned, exchange)
	}
	return exchange
}

func (e *pinnedExchange) fail(class string, failure *attemptFailure) *attemptFailure {
	if failure == nil {
		failure = &attemptFailure{}
	}
	failure.class, failure.dispatched = class, e.state.dispatched.Load()
	failure.credentialRefused = e.state.credentialRefused
	failure.acceptance = e.state.evidence().Acceptance()
	failure.accepted = e.state.accepted
	fact := e.fact()
	fact.Class = class
	if failure.status != 0 && fact.Status == 0 {
		fact.Status = failure.status
	}
	if failure.retryAfter > 0 {
		fact.RetryAfter = &failure.retryAfter
	}
	e.end()
	return failure
}

func (s *Server) doPinnedRequest(ctx context.Context, p *pin, exchange *pinnedExchange, req *http.Request, decodeError func([]byte) *wire.UpstreamError) (*pinnedResponse, *attemptFailure) {
	client, err := exchange.bound.client(ctx)
	if err != nil {
		return nil, exchange.fail(classCredential, nil)
	}
	resp, err := exchange.bound.do(client, req)
	x := exchange.request
	dispatched := exchange.state.dispatched.Load() || resp != nil
	exchange.state.dispatched.Store(dispatched)
	x.dispatched = x.dispatched || dispatched
	if dispatched && exchange.hold != nil {
		x.spendCaps(exchange.hold.budgets, p.attempt)
	}
	if err != nil {
		exchange.fact().LocalRefusal = errors.Is(err, errPinnedUploadRead)
		return nil, exchange.fail(exchange.state.classify(err, false), nil)
	}
	fact := exchange.fact()
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte, fact.Status = &received, resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		exchange.state.accepted = true
		// Headers prove acceptance, never valid completion or delivery.
		fact.Class = classProtocol
		exchange.recordEvidence()
		return &pinnedResponse{Response: resp, exchange: exchange}, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	_ = resp.Body.Close()
	failure := &attemptFailure{status: resp.StatusCode, upstream: x.redacted(decodeError(raw))}
	class := exchange.state.rejected(resp.StatusCode, failure.upstream)
	if class == classRateLimit {
		failure.retryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), s.now())
	}
	return nil, exchange.fail(class, failure)
}

func (e *pinnedExchange) complete(class string) {
	if e.state.settled && class == classSuccess {
		return
	}
	e.state.settled = true
	e.fact().Class = class
	e.end()
}

func (e *pinnedExchange) interrupt(class string, local bool) {
	if e.state.settled {
		return
	}
	fact := e.fact()
	fact.Class = class
	fact.LocalRefusal = local
	e.end()
}

func (e *pinnedExchange) invalidNative() {
	if e.state.settled {
		if e.fact().Class == classSuccess {
			e.fact().Class = classProtocol
		}
		return
	}
	e.interrupt(classProtocol, false)
}

func (e *pinnedExchange) end() {
	if !e.endedAt.IsZero() {
		return
	}
	e.endedAt = e.now()
	e.recordEvidence()
}

func (e *pinnedExchange) recordEvidence() {
	fact := e.fact()
	if !e.endedAt.IsZero() {
		fact.Duration = e.endedAt.Sub(fact.StartedAt)
	}
	acceptance := e.state.evidence().Acceptance()
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = acceptance
	}
	fact.RecordEvidence(e.billable && (acceptance.Unresolved() || e.state.accepted && fact.Usage == nil))
}

func (x *call) finalizePinned() {
	for _, exchange := range x.pinned {
		exchange.end()
	}
}

func (e *pinnedExchange) delivery(state usage.ClientState) {
	fact := e.fact()
	fact.Committed = true
	if fact.Interaction == nil {
		return
	}
	current := fact.Interaction.ClientState
	if current == usage.ClientTerminal || current == usage.ClientActionable && state == usage.ClientPartial || state == usage.ClientUnobserved && current != "" {
		return
	}
	fact.Interaction.ClientState = state
}

func (s *Server) settlePinnedHold(ctx context.Context, hold *dispatchHold) {
	if hold == nil || hold.settled {
		return
	}
	var latest *pinnedExchange
	var total int64
	var reconciled bool
	for _, exchange := range hold.pinned {
		exchange.end()
		if !exchange.state.dispatched.Load() {
			continue
		}
		latest = exchange
		fact := exchange.fact()
		actual := quota.SettledAttemptTokens(fact, hold.estimate)
		if actual == nil {
			total = quota.AddBounded(total, hold.estimate)
			continue
		}
		reconciled = true
		total = quota.AddBounded(total, *actual)
	}
	if latest == nil {
		s.releaseHold(ctx, hold)
		return
	}
	var actual *int64
	if reconciled {
		actual = &total
	}
	hold.settle(ctx, true, actual)
	if hold.probed {
		hold.probed = false
		s.health.Record(hold.provider, *latest.fact(), true)
	}
}

func deliveryState[T ~int | ~int64](written T) usage.ClientState {
	if written > 0 {
		return usage.ClientPartial
	}
	return usage.ClientUnobserved
}

func (r *pinnedResponse) readBody(limit int64) ([]byte, error) {
	body, err := readBounded(r.Body, limit)
	if err != nil {
		r.exchange.readFailure()
	}
	return body, err
}

func (e *pinnedExchange) readFailure() {
	switch e.state.interrupted(nil) {
	case context.Canceled:
		e.interrupt(classCancelled, true)
	case context.DeadlineExceeded:
		e.interrupt(classTimeout, false)
	}
}

func (r *pinnedResponse) stageBody(ctx context.Context, spool *staging.Spool, filename, contentType string, maximumLength int64) (*staging.Artifact, error) {
	if spool == nil {
		r.exchange.interrupt(classProtocol, true)
		return nil, staging.ErrUnavailable
	}
	artifact, err := spool.Put(ctx, staging.Upload{
		Body: r.Body, Filename: filename, ContentType: contentType, MaximumLength: maximumLength,
	})
	if err == nil {
		return artifact, nil
	}
	_, tooLarge := errors.AsType[*staging.TooLargeError](err)
	local := !tooLarge && (err == staging.ErrUnavailable || !errors.Is(err, staging.ErrUnavailable))
	r.exchange.interrupt(classProtocol, local)
	r.exchange.readFailure()
	return nil, err
}

var errPinnedUploadRead = errors.New("staged upload read failed")

type pinnedUploadSource struct{ io.Reader }

func (r pinnedUploadSource) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("%w: %w", errPinnedUploadRead, err)
	}
	return n, err
}
