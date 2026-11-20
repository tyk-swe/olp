package kernel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/upstream"
)

func (run attemptRun) prepare() (attemptPlan, *attemptEnd) {
	s, x, a, state := run.server, run.x, run.target, run.state
	if err := s.bindExchange(x, run.provider, run.slot, a.UpstreamModel, run.connector, state).validateBaseEndpoint(run.provider.Endpoint); err != nil {
		return attemptPlan{}, ended(ClassConnect, nil)
	}
	prepared, err := x.generationRequest(run.provider, a.TargetID, a.UpstreamModel)
	if err != nil {
		return attemptPlan{}, ended(ClassProtocol, nil)
	}
	x.guard.input = prepared.guardrailSegments
	for _, decision := range prepared.policyDecisions {
		recordDecision(x, decision)
	}
	reader := providerinvoke.NewResponseReader(prepared.config, prepared.wire, x.generation.parsed.Stream, int(s.Config.MaxEventBytes), s.Config.MaxResponseBytes)
	responseRequest, err := wire.NewRequest(prepared.wire, x.generation.parsed.Route, reader.Streaming(), prepared.document)
	if err != nil {
		return attemptPlan{}, ended(ClassProtocol, nil)
	}
	codec := protocols.PrepareResponse(responseRequest, x.generation.parsed, x.named().Slug, protocols.EmbeddingEncoding(x.generation.parsed, run.provider.ParameterDefaults))
	deadline, _ := run.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return attemptPlan{}, ended(ClassTimeout, &AttemptFailure{overall: true})
	}
	sending, cancel := context.WithCancel(run.traced)
	firstByte := time.AfterFunc(min(a.Timeout, remaining), func() { state.reason.CompareAndSwap(0, 1); cancel() })
	return attemptPlan{
		config: prepared.config, format: prepared.wire, contract: prepared.plan, reader: reader, codec: codec,
		body:    prepared.config.WrapRequest(prepared.document.Bytes(), a.UpstreamModel),
		sending: sending, cancel: cancel, firstByte: firstByte,
	}, nil
}

func (run attemptRun) send(plan attemptPlan, fact events.AttemptFact) (attemptSent, *attemptEnd) {
	s, x, a := run.server, run.x, run.target
	unsent := attemptSent{plan: plan, config: plan.config, fact: fact}
	endpoint, err := plan.config.URL(plan.format, a.UpstreamModel, plan.reader.Streaming())
	if err != nil {
		return unsent, ended(ClassProtocol, nil)
	}
	exchange := s.bindExchange(x, run.provider, run.slot, a.UpstreamModel, plan.config, run.state)
	req, err := exchange.request(plan.sending, http.MethodPost, endpoint, bytes.NewReader(plan.body), make(http.Header))
	if err != nil {
		return unsent, ended(ClassConnect, nil)
	}
	run.trace.InjectUpstream(req.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "olp/gateway")
	req.Header.Set("Accept", plan.reader.Accept())
	config := run.sendingConfig(plan, req.Header)
	exchange.config = config
	if end := run.authenticate(plan, exchange, req); end != nil {
		return unsent, end
	}
	client, err := exchange.dispatchClient(plan.sending)
	if err != nil {
		return unsent, ended(ClassCredential, nil)
	}
	parallel, end := run.announce(plan)
	announced := attemptSent{plan: plan, config: config, fact: fact, parallel: parallel}
	if end != nil {
		return announced, end
	}
	resp, err := exchange.do(client, req)
	if err != nil {
		return announced, ended(run.state.classify(err, false), nil)
	}
	return run.accept(attemptSent{plan: plan, config: config, fact: fact, parallel: parallel, response: resp})
}

func (run attemptRun) sendingConfig(plan attemptPlan, header http.Header) connectors.Config {
	if plan.contract != nil {
		return plan.config
	}
	return forwardAnthropicBeta(header, run.x, plan.format, plan.config)
}

func (run attemptRun) authenticate(plan attemptPlan, exchange *boundExchange, req *http.Request) *attemptEnd {
	err := exchange.authenticate(plan.sending, req, plan.body)
	switch {
	case err == nil:
		return nil
	case plan.sending.Err() != nil:
		return ended(run.state.classify(err, false), nil)
	case errors.Is(err, connectors.ErrSigningUnavailable):
		// The plugin couldn't sign the request, which says nothing
		// about the credential: the upstream is out of reach from
		// here, like one the gateway can't connect to.
		return ended(ClassConnect, nil)
	}
	return ended(ClassCredential, nil)
}

func (run attemptRun) announce(plan attemptPlan) (*parallelGuardrail, *attemptEnd) {
	s, x := run.server, run.x
	if plan.contract != nil && plan.contract.ToolContinuation() {
		if err := s.claimToolWork(plan.sending, x, plan.contract, run.target, run.slot); err != nil {
			return nil, ended(ClassProtocol, &AttemptFailure{contractCode: "continuation_unavailable", noRetry: true})
		}
	}
	parallel := s.startParallel(plan.sending, x, plan.cancel)
	if x.workflow == nil {
		return parallel, nil
	}
	if e := s.MarkSessionDispatched(plan.sending, x); e != nil {
		return parallel, ended(ClassConnect, &AttemptFailure{contractCode: e.Code, noRetry: true})
	}
	return parallel, nil
}

func (run attemptRun) accept(sent attemptSent) (attemptSent, *attemptEnd) {
	s, resp := run.server, sent.response
	received := s.Now().Sub(sent.fact.StartedAt)
	fact := sent.fact
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	answered := attemptSent{plan: sent.plan, config: sent.config, fact: fact, parallel: sent.parallel, response: resp}
	if resp.StatusCode == http.StatusOK {
		run.state.accepted = true
		return answered, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, ErrorBodyLimit))
	status, rejection := sent.config.Rejection(resp.StatusCode, raw)
	f := &AttemptFailure{Status: status, upstream: run.x.redacted(openai.ParseErrorBody(rejection))}
	class := run.state.rejected(status, f.upstream)
	if class == classRateLimit || (class == ClassUpstreamServer && resp.StatusCode == http.StatusServiceUnavailable) {
		f.retryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), s.Now())
	}
	return answered, ended(class, f)
}
