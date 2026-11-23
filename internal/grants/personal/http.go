package personal

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

type authorizeRequest struct {
	Flow Flow `json:"flow"`
}
type completionRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

func (s *Server) authorize(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.Kind != "user" {
		return httpkit.Reply{}, httpkit.Forbidden()
	}
	id, err := httpkit.IDParam(r, "enrollment_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input authorizeRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	result, runErr := s.grants.Authorize(r.Context(), id, p.UserID(), input.Flow, strings.TrimRight(s.access.Origin, "/")+"/agents/grants")
	if runErr != nil {
		return httpkit.Reply{}, oauthProblem(runErr)
	}
	return httpkit.Detail(result, ""), nil
}

func (s *Server) complete(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.Kind != "user" {
		return httpkit.Reply{}, httpkit.Forbidden()
	}
	id, err := httpkit.IDParam(r, "enrollment_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input completionRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Code == "" || len(input.Code) > 8192 || input.State == "" || len(input.State) > 256 {
		return httpkit.Reply{}, httpkit.Invalid("code", "Use the code and state from the authorization callback.")
	}
	result, runErr := s.grants.Complete(r.Context(), id, p.UserID(), input.Code, input.State)
	if runErr != nil {
		return httpkit.Reply{}, oauthProblem(runErr)
	}
	return httpkit.Detail(result, ""), nil
}

func (s *Server) poll(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.Kind != "user" {
		return httpkit.Reply{}, httpkit.Forbidden()
	}
	id, err := httpkit.IDParam(r, "enrollment_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	result, runErr := s.grants.Poll(r.Context(), id, p.UserID())
	if runErr != nil {
		return httpkit.Reply{}, oauthProblem(runErr)
	}
	return httpkit.Detail(result, ""), nil
}
