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
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/upstream"
)

func (run *attemptRun) prepare() *attemptEnd {
	s, x, a := run.server, run.x, run.target
	if err := s.bindExchange(x, run.provider, run.slot, a.UpstreamModel, run.config, run.state).validateBaseEndpoint(run.provider.Endpoint); err != nil {
		return ended(ClassConnect, nil)
	}
	prepared, err := x.generationRequest(run.provider, a.TargetID, a.UpstreamModel)
	if err != nil {
		return ended(ClassProtocol, nil)
	}
	x.guard.input = prepared.guardrailSegments
	for _, decision := range prepared.policyDecisions {
		recordDecision(x, decision)
	}
	run.config, run.format, run.contract = prepared.config, prepared.wire, prepared.plan
	run.body = run.config.WrapRequest(prepared.document.Bytes(), a.UpstreamModel)
	run.reader = providerinvoke.NewResponseReader(run.config, run.format, x.generation.parsed.Stream, int(s.Config.MaxEventBytes), s.Config.MaxResponseBytes)
	responseRequest, err := wire.NewRequest(run.format, x.generation.parsed.Route, run.reader.Streaming(), prepared.document)
	if err != nil {
		return ended(ClassProtocol, nil)
	}
	run.codec = protocols.PrepareResponse(responseRequest, x.generation.parsed, x.named().Slug, protocols.EmbeddingEncoding(x.generation.parsed, run.provider.ParameterDefaults))
	deadline, _ := run.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return ended(ClassTimeout, &AttemptFailure{overall: true})
	}
	run.sending, run.cancel = context.WithCancel(run.traced)
	run.firstByte = time.AfterFunc(min(a.Timeout, remaining), func() { run.state.reason.CompareAndSwap(0, 1); run.cancel() })
	return nil
}

func (run *attemptRun) send() *attemptEnd {
	s, x, a := run.server, run.x, run.target
	endpoint, err := run.config.URL(run.format, a.UpstreamModel, run.reader.Streaming())
	if err != nil {
		return ended(ClassProtocol, nil)
	}
	exchange := s.bindExchange(x, run.provider, run.slot, a.UpstreamModel, run.config, run.state)
	req, err := exchange.request(run.sending, http.MethodPost, endpoint, bytes.NewReader(run.body), make(http.Header))
	if err != nil {
		return ended(ClassConnect, nil)
	}
	run.trace.InjectUpstream(req.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "olp/gateway")
	req.Header.Set("Accept", run.reader.Accept())
	if run.contract == nil {
		run.config = forwardAnthropicBeta(req.Header, x, run.format, run.config)
	}
	exchange.config = run.config
	if end := run.authenticate(exchange, req); end != nil {
		return end
	}
	client, err := exchange.dispatchClient(run.sending)
	if err != nil {
		return ended(classCredential, nil)
	}
	if end := run.announce(); end != nil {
		return end
	}
	resp, err := exchange.do(client, req)
	if err != nil {
		return ended(run.state.classify(err, false), nil)
	}
	run.response = resp
	return run.accept()
}

func (run *attemptRun) authenticate(exchange *boundExchange, req *http.Request) *attemptEnd {
	err := exchange.authenticate(run.sending, req, run.body)
	switch {
	case err == nil:
		return nil
	case run.sending.Err() != nil:
		return ended(run.state.classify(err, false), nil)
	case errors.Is(err, connectors.ErrSigningUnavailable):
		// The plugin couldn't sign the request, which says nothing
		// about the credential: the upstream is out of reach from
		// here, like one the gateway can't connect to.
		return ended(ClassConnect, nil)
	}
	return ended(classCredential, nil)
}

func (run *attemptRun) announce() *attemptEnd {
	s, x := run.server, run.x
	if run.contract != nil && run.contract.ToolContinuation() {
		if err := s.claimToolWork(run.sending, x, run.contract, run.target, run.slot); err != nil {
			return ended(ClassProtocol, &AttemptFailure{contractCode: "continuation_unavailable", noRetry: true})
		}
	}
	run.parallel = s.startParallel(run.sending, x, run.cancel)
	if x.workflow == nil {
		return nil
	}
	if e := s.markSessionDispatched(run.sending, x); e != nil {
		return ended(ClassConnect, &AttemptFailure{contractCode: e.Code, noRetry: true})
	}
	return nil
}

func (run *attemptRun) accept() *attemptEnd {
	s, resp := run.server, run.response
	received := s.Now().Sub(run.fact.StartedAt)
	run.fact.FirstByte = &received
	run.fact.Status = resp.StatusCode
	if resp.StatusCode == http.StatusOK {
		run.state.accepted = true
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	status, rejection := run.config.Rejection(resp.StatusCode, raw)
	f := &AttemptFailure{Status: status, upstream: run.x.redacted(openai.ParseErrorBody(rejection))}
	class := run.state.rejected(status, f.upstream)
	if class == classRateLimit {
		f.retryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), s.Now())
	}
	return ended(class, f)
}
