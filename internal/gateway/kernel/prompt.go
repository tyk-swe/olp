package kernel

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/publishedprompt"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type parsedRequest struct {
	request   *wire.Request
	reference *usage.PromptReference
	decoded   int64
}

func (s *Server) parsePrompt(x *Call, family wire.Format, body []byte, pathModel string, authorize func(*routing.Route) *fault.Error) (parsedRequest, *fault.Error) {
	doc, err := protocols.ParseSource(family, body)
	if err != nil {
		return parsedRequest{}, requestError(err)
	}
	return s.parsePromptDocument(x, family, doc, pathModel, authorize, nil)
}

func (s *Server) parsePromptDocument(x *Call, family wire.Format, doc oif.Document, pathModel string, authorize func(*routing.Route) *fault.Error, bindings []oif.Change) (parsedRequest, *fault.Error) {
	expanded, prompt, promptErr := publishedprompt.Expand(publishedprompt.Scope{
		Snapshot: x.Snapshot(), MaxBodyBytes: s.Config.MaxBodyBytes,
		Admit: func(route *routing.Route) *fault.Error {
			if e := authorize(route); e != nil {
				return e
			}
			return x.CheckBody(route)
		},
	}, family, doc, pathModel)
	if promptErr != nil {
		return parsedRequest{}, promptErr
	}
	bound, err := oif.Apply(expanded, bindings)
	if err != nil {
		return parsedRequest{}, fault.ServerError(http.StatusInternalServerError, "internal_error", "The prompt could not be prepared.")
	}
	parsed, err := protocols.ParseDocument(family, bound, pathModel)
	if err != nil {
		return parsedRequest{}, requestError(err)
	}
	return parsedRequest{request: parsed, reference: prompt, decoded: int64(max(expanded.Len(), bound.Len()))}, nil
}
