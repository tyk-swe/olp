package gateway

import (
	"net/http"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func (s *Server) enter(w http.ResponseWriter, r *http.Request, x *call, authenticate func(*http.Request) (keyauthority.Authority, *Error)) (keyauthority.Authority, bool) {
	if !s.admit(r.Context()) {
		s.refuse(x, w, overloaded)
		return keyauthority.Authority{}, false
	}
	authority, e := authenticate(r)
	if e == nil {
		s.bindAuthority(x, authority)
		e = providerStatePermitted(x.family, authority)
	}
	if e == nil {
		x.attribution, e = s.parseAttribution(r, authority)
	}
	if e != nil {
		s.release(r.Context())
		s.refuse(x, w, e)
		return keyauthority.Authority{}, false
	}
	return authority, true
}

func (s *Server) authenticateInference(r *http.Request) (keyauthority.Authority, *Error) {
	return s.authenticate(r, "inference")
}

func providerStatePermitted(family wire.Format, authority keyauthority.Authority) *Error {
	switch family {
	case wire.AnthropicFile, wire.AnthropicBatch, wire.GeminiFile, wire.GeminiBatch, wire.CloudBatch:
		if !authority.Policy.AllowProviderState {
			return providerStateForbidden()
		}
	}
	return nil
}

func (s *Server) refuse(x *call, w http.ResponseWriter, e *Error) {
	s.refuseOn(x, w, e, x.family.Surface())
}

func (s *Server) refuseOn(x *call, w http.ResponseWriter, e *Error, surface string) {
	x.failure = e
	s.finish(x, nil, e.Status)
	writeSurfaceError(w, e, surface)
}
