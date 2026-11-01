package mcpgrants

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
)

type authorizeRequest struct {
	Flow Flow `json:"flow"`
}
type completionRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

func (s *Server) authorize(r *http.Request, p access.Principal) (access.Reply, error) {
	if p.Kind != "user" {
		return access.Reply{}, access.Forbidden()
	}
	id, err := access.IDParam(r, "enrollment_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input authorizeRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	result, runErr := s.store().Authorize(r.Context(), id, p.UserID(), input.Flow, strings.TrimRight(s.Access.Origin, "/")+"/agents/grants")
	if runErr != nil {
		return access.Reply{}, oauthProblem(runErr)
	}
	return access.Detail(result, ""), nil
}

func (s *Server) complete(r *http.Request, p access.Principal) (access.Reply, error) {
	if p.Kind != "user" {
		return access.Reply{}, access.Forbidden()
	}
	id, err := access.IDParam(r, "enrollment_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input completionRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.Code == "" || len(input.Code) > 8192 || input.State == "" || len(input.State) > 256 {
		return access.Reply{}, access.Invalid("code", "Use the code and state from the authorization callback.")
	}
	result, runErr := s.store().Complete(r.Context(), id, p.UserID(), input.Code, input.State)
	if runErr != nil {
		return access.Reply{}, oauthProblem(runErr)
	}
	return access.Detail(result, ""), nil
}

func (s *Server) poll(r *http.Request, p access.Principal) (access.Reply, error) {
	if p.Kind != "user" {
		return access.Reply{}, access.Forbidden()
	}
	id, err := access.IDParam(r, "enrollment_id")
	if err != nil {
		return access.Reply{}, err
	}
	result, runErr := s.store().Poll(r.Context(), id, p.UserID())
	if runErr != nil {
		return access.Reply{}, oauthProblem(runErr)
	}
	return access.Detail(result, ""), nil
}
