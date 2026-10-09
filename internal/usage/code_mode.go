package usage

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

func (s *Server) registerCodeMode(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/code/attempts", s.codeAttempts)
	s.Access.Route(mux, "GET /api/v1/code/refusals", s.codeRefusals)
	s.Access.Route(mux, "GET /api/v1/code/token-windows", s.codeTokenWindows)
}
func (s *Server) codeAttempts(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeUsageList(r, p, `SELECT to_jsonb(x) FROM olp.code_attempts x`)
}
func (s *Server) codeRefusals(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeUsageList(r, p, `SELECT to_jsonb(x) FROM olp.code_refusals x`)
}
func (s *Server) codeTokenWindows(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeList(r, p, `SELECT jsonb_build_object('id',x.id,'project_id',x.project_id,'windows',COALESCE((SELECT jsonb_agg(jsonb_build_object('period',w.period,'starts_at',w.starts_at,'reserved',w.reserved,'measured',w.measured) ORDER BY w.starts_at DESC,w.period) FROM olp.code_token_windows w WHERE w.budget_id=x.id),'[]'::jsonb)) FROM olp.code_token_budgets x`)
}
