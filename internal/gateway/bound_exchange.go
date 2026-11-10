package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/routing"
)

type boundExchange struct {
	server  *Server
	call    *call
	config  connectors.Config
	target  providerinvoke.Binding
	state   *attemptState
	carrier bool
}

func (s *Server) bindExchange(x *call, provider *routing.Provider, slot routing.Slot, model string, config connectors.Config, state *attemptState) *boundExchange {
	return &boundExchange{server: s, call: x, config: config, target: s.targetBinding(x, provider, slot, model), state: state}
}

func (e *boundExchange) validateBaseEndpoint(endpoint string) error {
	_, err := e.server.egress.ValidateEndpoint(endpoint)
	return err
}

func (e *boundExchange) validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	parsed.RawQuery, parsed.Fragment, parsed.RawFragment = "", "", ""
	return e.validateBaseEndpoint(parsed.String())
}

func (e *boundExchange) request(ctx context.Context, method, endpoint string, body io.Reader, header http.Header) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header = header.Clone()
	return request, nil
}

func (e *boundExchange) trace(request *http.Request) *http.Request {
	if e.state == nil {
		return request
	}
	return request.WithContext(httptrace.WithClientTrace(request.Context(), e.state.trace()))
}

func (e *boundExchange) authenticate(ctx context.Context, request *http.Request, body []byte) error {
	credential, err := e.target.Credential(ctx)
	e.call.grantGeneration = credential.Generation
	if err != nil {
		return err
	}
	applied, err := e.server.auth.Apply(ctx, request, e.config, credential.Secret, body)
	e.call.sensitive.Include(applied)
	return err
}

func (e *boundExchange) client(ctx context.Context) (*http.Client, error) {
	return e.target.Client(ctx)
}

func (e *boundExchange) dispatchClient(ctx context.Context) (*http.Client, error) {
	if e.config.CarriedByPlugin() {
		e.carrier = true
		return e.config.CarrierClient(e.server.cfg.Carrier, e.call.sensitive.Values()), nil
	}
	return e.target.Client(ctx)
}

func (e *boundExchange) do(client *http.Client, request *http.Request) (*http.Response, error) {
	response, err := client.Do(e.trace(request))
	if e.state == nil {
		return response, err
	}
	if e.carrier {
		e.state.dispatched.Store(!errors.Is(err, connectors.ErrNotSent))
		e.state.notSent.Store(errors.Is(err, connectors.ErrNotSent))
	} else if response != nil {
		e.state.dispatched.Store(true)
	}
	return response, err
}
