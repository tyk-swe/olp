package playground

import (
	"cmp"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

const Timeout = 2 * time.Minute

const BodyLimit = 1 << 20

type API struct {
	kernel *kernel.Server
}

func NewAPI(k *kernel.Server) *API { return &API{kernel: k} }

func (p *API) member(r *http.Request, principal authz.Principal, in request) *kernel.MemberGeneration {
	return p.kernel.MemberGeneration(r, kernel.Member{UserID: principal.ID, Preferences: in.Routing, Document: in.Request, Admit: p.authorize(principal)})
}

func (p *API) authorize(principal authz.Principal) func(*routing.Route) *fault.Error {
	return func(route *routing.Route) *fault.Error {
		if principal.Project(route.ProjectID, authz.View) != nil {
			return fault.ModelNotFound(route.Slug)
		}
		return nil
	}
}

func (p *API) Handle(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
	var in request
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	if err := in.validate(false); err != nil {
		return httpkit.Reply{}, err
	}
	run := p.member(r, principal, in)
	parsed, err := in.parse(in.operation(), "unary", run)
	if err != nil {
		return httpkit.Reply{}, err
	}
	out := run.Run(r.Context(), parsed, nil)
	if e := out.Err; e != nil {
		return httpkit.Reply{}, problem.Fail(cmp.Or(e.Status, http.StatusBadGateway), e.Code, e.Message)
	}
	return httpkit.Reply{
		Status:              http.StatusOK,
		Body:                response(run.Trail(), out.Completion, in),
		DisableHTMLEscaping: in.operation() != "generation",
	}, nil
}
