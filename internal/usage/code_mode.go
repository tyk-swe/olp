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
	return s.Access.CodeUsageList(r, p, `SELECT jsonb_build_object(
'upstream_status',x.upstream_status,'outcome_origin',x.outcome_origin,'outcome',x.outcome,'outcome_observed_at',x.outcome_observed_at,
'account_id',x.account_id,'api_key_id',x.api_key_id,'binding_id',x.binding_id,'bound_evidence',x.bound_evidence,
'cached_tokens',x.cached_tokens,'created_at',x.created_at,'finished_at',x.finished_at,'id',x.id,
'input_tokens',x.input_tokens,'model',x.model,'operation',x.operation,'output_tokens',x.output_tokens,
'project_id',x.project_id,'reasoning_tokens',x.reasoning_tokens,'refusal',x.refusal,'reported_tokens',x.reported_tokens,
'reserved_tokens',x.reserved_tokens,'route_id',x.route_id,'state',x.state,'route_revision_id',x.route_revision_id,
'end_user_digest',x.end_user_digest,'attribution',x.attribution,'paid_binding_id',x.paid_binding_id,'paid_provider_id',x.paid_provider_id,
'paid_slot_id',x.paid_slot_id,'requested_model',x.requested_model,
'paid_monetary_state',CASE WHEN x.paid_binding_id IS NULL THEN NULL WHEN x.state='prepared' THEN 'pending' WHEN x.state='aborted' THEN 'released'
 WHEN EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.request_id=x.id AND f.usage_complete AND f.charge_status<>'billing_uncertain' AND f.estimated_cost IS NOT NULL AND f.charge IS NOT NULL) THEN 'settled' ELSE 'unknown' END
) FROM olp.code_attempts x`, access.CodeMatch{Field: "binding_id", SQL: `(x.binding_id=$%[1]d::uuid OR x.paid_binding_id=$%[1]d::uuid)`})
}
func (s *Server) codeRefusals(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeUsageList(r, p, `SELECT to_jsonb(x) FROM olp.code_refusals x`)
}
func (s *Server) codeTokenWindows(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeList(r, p, `SELECT jsonb_build_object('id',x.id,'project_id',x.project_id,'windows',COALESCE((SELECT jsonb_agg(jsonb_build_object('period',w.period,'starts_at',w.starts_at,'reserved',w.reserved,'measured',w.measured) ORDER BY w.starts_at DESC,w.period) FROM olp.code_token_windows w WHERE w.budget_id=x.id),'[]'::jsonb)) FROM olp.code_token_budgets x`)
}
