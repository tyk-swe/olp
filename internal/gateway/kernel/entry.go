package kernel

import (
	"net/http"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
)

func (s *Server) enter(w http.ResponseWriter, r *http.Request, x *Call, authenticate func(*http.Request) (keyauthority.Authority, *fault.Error)) (keyauthority.Authority, bool) {
	if !s.Gate.Admit(r.Context()) {
		s.Refuse(x, w, fault.Overloaded)
		return keyauthority.Authority{}, false
	}
	authority, e := authenticate(r)
	if e == nil {
		s.BindAuthority(x, authority)
		e = providerStatePermitted(x.family, authority)
	}
	if e == nil {
		x.attribution, e = ingress.ParseAttribution(r, authority)
	}
	if e != nil {
		s.Gate.Release(r.Context())
		s.Refuse(x, w, e)
		return keyauthority.Authority{}, false
	}
	return authority, true
}

func (s *Server) authenticateInference(r *http.Request) (keyauthority.Authority, *fault.Error) {
	return s.Gate.Authenticate(r, "inference")
}

func providerStatePermitted(family wire.Format, authority keyauthority.Authority) *fault.Error {
	switch family {
	case wire.AnthropicFile, wire.AnthropicBatch, wire.GeminiFile, wire.GeminiBatch, wire.CloudBatch:
		if !authority.Policy.AllowProviderState {
			return ProviderStateForbidden()
		}
	}
	return nil
}

func (x *Call) NoteAttribution(r *http.Request, authority keyauthority.Authority) *fault.Error {
	var e *fault.Error
	x.attribution, e = ingress.ParseAttribution(r, authority)
	return e
}

func (x *Call) NoteAttributionValues(values []string, authority keyauthority.Authority) *fault.Error {
	var e *fault.Error
	x.attribution, e = ingress.ParseAttributionValues(values, authority)
	return e
}

func (s *Server) Refuse(x *Call, w http.ResponseWriter, e *fault.Error) {
	s.RefuseOn(x, w, e, x.family.Surface())
}

func (s *Server) RefuseOn(x *Call, w http.ResponseWriter, e *fault.Error, surface string) {
	x.Failure = e
	s.Finish(x, nil, e.Status)
	fault.WriteSurfaceError(w, e, surface)
}
