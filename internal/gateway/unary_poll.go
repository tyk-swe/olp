package gateway

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

const (
	maxUnaryPolls   = 240
	pollInterval    = time.Second
	minPollInterval = 100 * time.Millisecond
	maxPollInterval = 30 * time.Second
	pollCancelBound = 5 * time.Second
)

type unaryFailure struct {
	class   string
	failure *attemptFailure
}

type unaryExchange struct {
	server   *Server
	ctx      context.Context
	x        *call
	state    *attemptState
	plan     *operationplan.Plan
	client   *http.Client
	slot     routing.Slot
	provider *routing.Provider
	model    string
	fact     *AttemptFact
	bodyCap  int
	inject   func(http.Header)
}

func (s *Server) validateUnaryEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	_, err = s.egress.ValidateEndpoint(u.String())
	return err
}

func (e *unaryExchange) do(method, target string, body []byte) (*http.Response, *unaryFailure) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(e.ctx, e.state.trace()), method, target, reader)
	if err != nil {
		return nil, &unaryFailure{class: classConnect}
	}
	e.inject(req.Header)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "olp/gateway")
	if err := e.server.applySlotCredential(e.ctx, e.x, req, e.plan.Config(), e.slot, body, e.provider, e.model); err != nil {
		if e.ctx.Err() != nil {
			return nil, &unaryFailure{class: e.state.classify(err, false)}
		}
		return nil, &unaryFailure{class: classCredential}
	}
	response, err := e.client.Do(req)
	if err != nil {
		return nil, &unaryFailure{class: e.state.classify(err, false)}
	}
	if e.fact.FirstByte == nil {
		received := e.server.now().Sub(e.fact.StartedAt)
		e.fact.FirstByte = &received
	}
	return response, nil
}

func (e *unaryExchange) rejected(response *http.Response) *unaryFailure {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
	response.Body.Close()
	e.fact.Status = response.StatusCode
	failure := &attemptFailure{status: response.StatusCode, upstream: e.x.redacted(openai.ParseErrorBody(raw))}
	class := e.state.rejected(response.StatusCode, failure.upstream)
	if class == classRateLimit {
		failure.retryAfter = upstream.RetryAfter(response.Header.Get("Retry-After"), e.server.now())
	}
	return &unaryFailure{class: class, failure: failure}
}

func (e *unaryExchange) readJSON(response *http.Response) ([]byte, *unaryFailure) {
	defer response.Body.Close()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, &unaryFailure{class: classProtocol, failure: &attemptFailure{contractCode: "fidelity_protocol_violation"}}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(e.bodyCap)+1))
	if err != nil {
		return nil, &unaryFailure{class: e.state.classify(err, false)}
	}
	if len(raw) > e.bodyCap {
		return nil, &unaryFailure{class: classProtocol, failure: &attemptFailure{contractCode: "fidelity_protocol_violation"}}
	}
	return raw, nil
}

func (e *unaryExchange) run(method, submit string, body []byte) ([]byte, *unaryFailure) {
	response, failure := e.do(method, submit, body)
	if failure != nil {
		return nil, failure
	}
	e.fact.Status = response.StatusCode
	if e.plan.Transport() != operations.TransportPoll202 {
		if response.StatusCode != http.StatusOK {
			return nil, e.rejected(response)
		}
		e.state.accepted = true
		return e.readJSON(response)
	}
	if response.StatusCode == http.StatusOK {
		e.state.accepted = true
		raw, readFailure := e.readJSON(response)
		if readFailure != nil {
			return nil, readFailure
		}
		return e.terminal(raw, "")
	}
	if response.StatusCode != http.StatusAccepted {
		return nil, e.rejected(response)
	}
	e.state.accepted = true
	location, err := e.plan.PollLocation(submit, response.Header)
	io.Copy(io.Discard, io.LimitReader(response.Body, errorBodyLimit))
	response.Body.Close()
	if err != nil {
		return nil, &unaryFailure{class: classProtocol, failure: &attemptFailure{contractCode: "fidelity_protocol_violation"}}
	}
	if err := e.server.validateUnaryEndpoint(location); err != nil {
		return nil, &unaryFailure{class: classProtocol, failure: &attemptFailure{contractCode: "fidelity_protocol_violation"}}
	}
	return e.poll(location)
}

func (e *unaryExchange) terminal(raw []byte, location string) ([]byte, *unaryFailure) {
	state, err := e.plan.PollState(raw)
	if err != nil {
		return nil, &unaryFailure{class: classProtocol, failure: &attemptFailure{contractCode: "fidelity_protocol_violation"}}
	}
	switch state {
	case operations.PollSucceeded:
		return raw, nil
	case operations.PollFailed:
		return nil, &unaryFailure{class: classUpstreamServer, failure: &attemptFailure{status: http.StatusOK}}
	}
	return nil, &unaryFailure{class: classProtocol, failure: &attemptFailure{contractCode: "fidelity_protocol_violation"}}
}

func (e *unaryExchange) poll(location string) ([]byte, *unaryFailure) {
	var waitHeader http.Header
	for polls := 0; polls < maxUnaryPolls; polls++ {
		if polls > 0 {
			if failure := e.wait(location, waitHeader); failure != nil {
				return nil, failure
			}
		}
		response, failure := e.do(http.MethodGet, location, nil)
		if failure != nil {
			if e.ctx.Err() != nil {
				e.cancelPoll(location)
				return nil, failure
			}
			waitHeader = nil
			continue
		}
		waitHeader = response.Header
		switch {
		case response.StatusCode == http.StatusOK:
			e.fact.Status = response.StatusCode
			raw, readFailure := e.readJSON(response)
			if readFailure != nil {
				return nil, readFailure
			}
			state, err := e.plan.PollState(raw)
			if err != nil {
				return nil, &unaryFailure{class: classProtocol, failure: &attemptFailure{contractCode: "fidelity_protocol_violation"}}
			}
			if state == operations.PollSucceeded {
				return raw, nil
			}
			if state == operations.PollFailed {
				return nil, &unaryFailure{class: classUpstreamServer, failure: &attemptFailure{status: http.StatusOK}}
			}
		case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
			io.Copy(io.Discard, io.LimitReader(response.Body, errorBodyLimit))
			response.Body.Close()
		default:
			return nil, e.rejected(response)
		}
	}
	e.cancelPoll(location)
	return nil, &unaryFailure{class: classTimeout, failure: &attemptFailure{overall: true}}
}

func (e *unaryExchange) wait(location string, header http.Header) *unaryFailure {
	wait := pollInterval
	if header != nil && header.Get("Retry-After") != "" {
		wait = min(max(upstream.RetryAfter(header.Get("Retry-After"), e.server.now()), minPollInterval), maxPollInterval)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-e.ctx.Done():
		e.cancelPoll(location)
		return &unaryFailure{class: e.state.classify(e.ctx.Err(), false)}
	}
}

func (e *unaryExchange) cancelPoll(location string) {
	method, target, ok := e.plan.CancelPoll(location)
	if !ok || method == "" || target == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), pollCancelBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return
	}
	e.inject(req.Header)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "olp/gateway")
	if err := e.server.applySlotCredential(ctx, e.x, req, e.plan.Config(), e.slot, nil, e.provider, e.model); err != nil {
		return
	}
	response, err := e.client.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, errorBodyLimit))
	response.Body.Close()
}
